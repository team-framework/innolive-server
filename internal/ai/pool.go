package ai

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// WhitelistResult is the outcome of registering one reference face across the
// pool: a representative worker response plus every worker's own entry id.
type WhitelistResult struct {
	Response *aiv1.WhitelistResponse
	// EntryIDs maps a worker address to the entry id that worker minted for the
	// face. Each worker generates its id independently (uuid4), so the ids for
	// one face differ per worker and deleting it later means sending every
	// worker its own id — see DeleteWhitelistEntries.
	EntryIDs map[string]string
}

// AddWhitelist registers a client's reference face on every AI worker (sessions
// spread round-robin across targets, so the whitelist must exist on all of them).
func (p *Pool) AddWhitelist(ctx context.Context, sessionID string, data []byte) (*WhitelistResult, error) {
	return p.broadcast("add", func(c *Client) (*aiv1.WhitelistResponse, error) {
		return c.AddWhitelist(ctx, sessionID, data)
	})
}

// DeleteWhitelistEntries removes one registered face from every worker holding
// it, addressing each worker with the entry id that worker itself minted
// (entryIDs is the map from AddWhitelist). Workers absent from the map never
// stored the face and are skipped; a worker that no longer knows the id (it
// restarted, or the entry is already gone) counts as deleted, so a face never
// becomes permanently undeletable.
func (p *Pool) DeleteWhitelistEntries(ctx context.Context, sessionID string, entryIDs map[string]string) error {
	return p.runOnWorkers("delete", func(c *Client) error {
		entryID := entryIDs[c.Address()]
		if entryID == "" {
			return nil
		}
		_, err := c.DeleteWhitelist(ctx, sessionID, entryID)
		return ignoreNotFound(err)
	})
}

// ClearWhitelist removes every entry each worker currently holds for the
// session. The entry ids come from the worker itself rather than from this
// server's bookkeeping, so the worker whitelist ends up empty even if the two
// have drifted apart. The workers reject an empty entry id, so there is no
// single "delete all" call to use instead.
func (p *Pool) ClearWhitelist(ctx context.Context, sessionID string) error {
	return p.runOnWorkers("clear", func(c *Client) error {
		snapshot, err := c.GetWhitelistStatus(ctx, sessionID)
		if err != nil {
			return ignoreNotFound(err)
		}
		for _, entryID := range snapshot.GetEntryIds() {
			if _, err := c.DeleteWhitelist(ctx, sessionID, entryID); err != nil {
				if err = ignoreNotFound(err); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// ignoreNotFound drops the error a worker returns for an entry or session it
// does not have: the caller wanted it gone, and it is gone.
func ignoreNotFound(err error) error {
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}

// broadcast runs op on every worker concurrently. A partial failure is reported
// naming the failed targets (the caller may retry — workers treat re-application
// as idempotent), and a worker-side rejection ("failed" status) is surfaced
// through the returned response.
func (p *Pool) broadcast(kind string, op func(*Client) (*aiv1.WhitelistResponse, error)) (*WhitelistResult, error) {
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

	result := &WhitelistResult{EntryIDs: make(map[string]string, len(p.clients))}
	var errs []error
	for range p.clients {
		worker := <-results
		if worker.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", worker.address, worker.err))
			continue
		}
		if entryID := worker.response.GetEntryId(); entryID != "" {
			result.EntryIDs[worker.address] = entryID
		}
		if result.Response == nil || worker.response.GetStatusMessage() == "failed" {
			result.Response = worker.response
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("whitelist %s broadcast failed on %d/%d targets: %w", kind, len(errs), len(p.clients), errors.Join(errs...))
	}
	return result, nil
}

// runOnWorkers runs op on every worker concurrently, joining failures with the
// target that produced them.
func (p *Pool) runOnWorkers(kind string, op func(*Client) error) error {
	results := make(chan error, len(p.clients))
	for _, client := range p.clients {
		go func(c *Client) {
			if err := op(c); err != nil {
				results <- fmt.Errorf("%s: %w", c.Address(), err)
				return
			}
			results <- nil
		}(client)
	}
	var errs []error
	for range p.clients {
		if err := <-results; err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("whitelist %s failed on %d/%d targets: %w", kind, len(errs), len(p.clients), errors.Join(errs...))
	}
	return nil
}
