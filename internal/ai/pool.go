package ai

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"
)

// Pool holds one client per AI worker target and assigns sessions to
// workers round-robin. With MPS (or software resource caps) each worker
// process owns a bounded GPU share, so spreading sessions across workers is
// what turns that isolation into per-stream isolation.
type Pool struct {
	clients []*Client
	next    atomic.Uint64
}

func NewPool(targets []string, timeout time.Duration) (*Pool, error) {
	if len(targets) == 0 {
		return nil, errors.New("AI gRPC target list is empty")
	}
	clients := make([]*Client, 0, len(targets))
	for _, target := range targets {
		client, err := New(target, timeout)
		if err != nil {
			for _, created := range clients {
				_ = created.Close()
			}
			return nil, fmt.Errorf("create AI client for %q: %w", target, err)
		}
		clients = append(clients, client)
	}
	return &Pool{clients: clients}, nil
}

// Next returns the client for the next session, cycling through targets.
func (p *Pool) Next() *Client {
	index := (p.next.Add(1) - 1) % uint64(len(p.clients))
	return p.clients[index]
}

func (p *Pool) Clients() []*Client {
	result := make([]*Client, len(p.clients))
	copy(result, p.clients)
	return result
}

func (p *Pool) Close() error {
	var errs []error
	for _, client := range p.clients {
		if err := client.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// AddWhitelist registers a client's reference face on every AI worker (sessions
// spread round-robin across targets, so the whitelist must exist on all of them).
//
// Each worker generates its own entry_id for this face (server-authoritative,
// unlike the old client-supplied face_id), so the id in the returned response
// is only guaranteed valid on whichever worker happened to be picked as the
// representative response in broadcast(). DeleteWhitelist re-sends that same
// id to every worker; a worker whose own id differs will not find a match for
// this specific entry and silently no-ops, leaving that copy until the next
// session-wide (empty entry_id) cleanup. Acceptable for now: it is a stale
// in-memory entry on one worker, not a whitelist leak across sessions.
func (p *Pool) AddWhitelist(ctx context.Context, sessionID string, data []byte) (*aiv1.WhitelistResponse, error) {
	return p.broadcast("add", func(c *Client) (*aiv1.WhitelistResponse, error) {
		return c.AddWhitelist(ctx, sessionID, data)
	})
}

// DeleteWhitelist removes a whitelist entry from every AI worker. Empty
// entryID removes all entries for the session (see AddWhitelist doc for the
// per-worker entry_id caveat on non-empty entryID).
func (p *Pool) DeleteWhitelist(ctx context.Context, sessionID, entryID string) (*aiv1.WhitelistResponse, error) {
	return p.broadcast("delete", func(c *Client) (*aiv1.WhitelistResponse, error) {
		return c.DeleteWhitelist(ctx, sessionID, entryID)
	})
}

// broadcast runs op on every worker concurrently. A partial failure is reported
// naming the failed targets (the caller may retry — workers treat re-application
// as idempotent), and a worker-side rejection ("failed" status) is surfaced
// through the returned response.
func (p *Pool) broadcast(kind string, op func(*Client) (*aiv1.WhitelistResponse, error)) (*aiv1.WhitelistResponse, error) {
	type outcome struct {
		address  string
		response *aiv1.WhitelistResponse
		err      error
	}
	results := make(chan outcome, len(p.clients))
	for _, client := range p.clients {
		go func(c *Client) {
			response, err := op(c)
			results <- outcome{address: c.Address(), response: response, err: err}
		}(client)
	}

	var response *aiv1.WhitelistResponse
	var errs []error
	for range p.clients {
		result := <-results
		if result.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", result.address, result.err))
			continue
		}
		if response == nil || result.response.GetStatusMessage() == "failed" {
			response = result.response
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("whitelist %s broadcast failed on %d/%d targets: %w", kind, len(errs), len(p.clients), errors.Join(errs...))
	}
	return response, nil
}
