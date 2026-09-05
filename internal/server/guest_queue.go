package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/session"

	"github.com/redis/go-redis/v9"
)

const (
	guestCookieName   = "innolive_guest"
	guestQueueKey     = "innolive:guest:queue"
	guestTicketKey    = "innolive:guest:ticket:"
	guestByIDKey      = "innolive:guest:by-id:"
	guestReserveKey   = "innolive:guest:reservations"
	guestQueueChannel = "innolive:guest:queue:changed"
)

var (
	ErrGuestQueueUnavailable = errors.New("guest queue unavailable")
	ErrGuestTicketNotFound   = errors.New("guest ticket not found")
	ErrGuestForbidden        = errors.New("guest ticket does not belong to caller")
	ErrGuestAdmissionInvalid = errors.New("guest admission token is invalid")
)

// admitHeadScript atomically claims the current FIFO head and turns it into an
// admission. Keeping dequeue and ticket mutation in one Redis script means a
// connection failure can never strand a waiting ticket outside the ZSET.
var admitHeadScript = redis.NewScript(`
local ids = redis.call('ZRANGE', KEYS[1], 0, 0)
if #ids == 0 then return '' end
local id = ids[1]
local ticket = KEYS[3] .. id
if redis.call('HGET', ticket, 'status') ~= 'waiting' then
  redis.call('ZREM', KEYS[1], id)
  return '__stale__'
end
local guest = redis.call('HGET', ticket, 'guest')
if not guest then
  redis.call('ZREM', KEYS[1], id)
  return '__stale__'
end
redis.call('ZREM', KEYS[1], id)
redis.call('HSET', ticket, 'status', 'admitted', 'admission', ARGV[1], 'admission_plain', ARGV[2], 'expires_at', ARGV[3])
redis.call('PEXPIRE', ticket, ARGV[4])
redis.call('PEXPIRE', KEYS[4] .. guest, ARGV[4])
redis.call('ZADD', KEYS[2], ARGV[5], id)
return id
`)

var cancelTicketScript = redis.NewScript(`
local ticket = KEYS[3] .. ARGV[1]
local status = redis.call('HGET', ticket, 'status')
if not status then return '__missing__' end
if redis.call('HGET', ticket, 'guest') ~= ARGV[2] then return '__forbidden__' end
if status == 'waiting' then redis.call('ZREM', KEYS[1], ARGV[1])
elseif status == 'admitted' then redis.call('ZREM', KEYS[2], ARGV[1])
else return '__invalid__' end
redis.call('HSET', ticket, 'status', 'cancelled')
if redis.call('GET', KEYS[4] .. ARGV[2]) == ARGV[1] then redis.call('DEL', KEYS[4] .. ARGV[2]) end
return 'ok'
`)

type guestTicket struct {
	ID             string    `json:"ticket_id"`
	Status         string    `json:"status"`
	Position       int64     `json:"position,omitempty"`
	AdmissionToken string    `json:"admission_token,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// GuestQueue owns only short-lived admission state. WebRTC sessions remain in
// session.Manager, which is already process-local, so the queue intentionally
// fails closed when Redis is unavailable.
type GuestQueue struct {
	client          *redis.Client
	sessions        *session.Manager
	ttl             time.Duration
	admissionTTL    time.Duration
	guestSessionTTL time.Duration
	maxGuests       int
	mu              sync.Mutex
}

func NewGuestQueue(ctx context.Context, cfg config.Config, sessions *session.Manager) (*GuestQueue, error) {
	if !cfg.GuestQueueEnabled {
		return nil, nil
	}
	client := redis.NewClient(&redis.Options{Addr: cfg.GuestQueueRedisAddr, Password: cfg.GuestQueueRedisPassword})
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connect guest queue Redis: %w", err)
	}
	return &GuestQueue{client: client, sessions: sessions, ttl: cfg.GuestQueueTTL, admissionTTL: cfg.GuestAdmissionTTL, guestSessionTTL: cfg.GuestSessionTTL, maxGuests: cfg.MaxSessions / 2}, nil
}

func (q *GuestQueue) Close() error {
	if q == nil || q.client == nil {
		return nil
	}
	return q.client.Close()
}

func newGuestSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func guestHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (q *GuestQueue) CreateOrGet(ctx context.Context, guest string) (guestTicket, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	changed, err := q.cleanupAndAdmit(ctx)
	if err != nil {
		return guestTicket{}, err
	}
	if changed {
		q.publish(ctx)
	}
	hash := guestHash(guest)
	if id, err := q.client.Get(ctx, guestByIDKey+hash).Result(); err == nil {
		return q.statusLocked(ctx, hash, id)
	} else if err != redis.Nil {
		return guestTicket{}, ErrGuestQueueUnavailable
	}
	id, err := newGuestSecret()
	if err != nil {
		return guestTicket{}, err
	}
	expires := time.Now().Add(q.ttl)
	pipe := q.client.TxPipeline()
	pipe.HSet(ctx, guestTicketKey+id, "guest", hash, "status", "waiting", "expires_at", expires.Unix())
	pipe.Expire(ctx, guestTicketKey+id, q.ttl)
	pipe.Set(ctx, guestByIDKey+hash, id, q.ttl)
	pipe.ZAdd(ctx, guestQueueKey, redis.Z{Score: float64(time.Now().UnixNano()), Member: id})
	if _, err := pipe.Exec(ctx); err != nil {
		return guestTicket{}, ErrGuestQueueUnavailable
	}
	changed, err = q.cleanupAndAdmit(ctx)
	if err != nil {
		return guestTicket{}, err
	}
	if changed {
		q.publish(ctx)
	}
	return q.statusLocked(ctx, hash, id)
}

func (q *GuestQueue) Status(ctx context.Context, guest, id string) (guestTicket, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	changed, err := q.cleanupAndAdmit(ctx)
	if err != nil {
		return guestTicket{}, err
	}
	if changed {
		q.publish(ctx)
	}
	return q.statusLocked(ctx, guestHash(guest), id)
}

func (q *GuestQueue) Heartbeat(ctx context.Context, guest, id string) (guestTicket, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	changed, err := q.cleanupAndAdmit(ctx)
	if err != nil {
		return guestTicket{}, err
	}
	if changed {
		q.publish(ctx)
	}
	result, err := q.statusLocked(ctx, guestHash(guest), id)
	if err != nil {
		return guestTicket{}, err
	}
	if result.Status == "waiting" {
		expires := time.Now().Add(q.ttl)
		_, err := q.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Expire(ctx, guestTicketKey+id, q.ttl)
			pipe.Expire(ctx, guestByIDKey+guestHash(guest), q.ttl)
			pipe.HSet(ctx, guestTicketKey+id, "expires_at", expires.Unix())
			return nil
		})
		if err != nil {
			return guestTicket{}, ErrGuestQueueUnavailable
		}
	}
	return q.statusLocked(ctx, guestHash(guest), id)
}

func (q *GuestQueue) Cancel(ctx context.Context, guest, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	result, err := cancelTicketScript.Run(ctx, q.client,
		[]string{guestQueueKey, guestReserveKey, guestTicketKey, guestByIDKey}, id, guestHash(guest)).Text()
	if err != nil {
		return ErrGuestQueueUnavailable
	}
	switch result {
	case "ok":
	case "__missing__":
		return ErrGuestTicketNotFound
	case "__forbidden__":
		return ErrGuestForbidden
	default:
		return ErrGuestAdmissionInvalid
	}
	q.publish(ctx)
	return nil
}

func (q *GuestQueue) Consume(ctx context.Context, guest, token string, metadata map[string]string) (*session.Session, string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	id, err := q.client.Get(ctx, guestByIDKey+guestHash(guest)).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, "", ErrGuestAdmissionInvalid
		}
		return nil, "", ErrGuestQueueUnavailable
	}
	values, err := q.client.HGetAll(ctx, guestTicketKey+id).Result()
	if err != nil {
		return nil, "", ErrGuestQueueUnavailable
	}
	if values["status"] != "admitted" || values["admission"] != guestHash(token) {
		return nil, "", ErrGuestAdmissionInvalid
	}
	if q.sessions.GuestCount() >= q.maxGuests {
		return nil, "", ErrGuestAdmissionInvalid
	}
	restore := func() error {
		expires := time.Now().Add(q.admissionTTL)
		_, err := q.client.TxPipelined(context.Background(), func(pipe redis.Pipeliner) error {
			pipe.HSet(context.Background(), guestTicketKey+id, "status", "admitted", "expires_at", expires.Unix())
			pipe.PExpire(context.Background(), guestTicketKey+id, q.admissionTTL)
			pipe.PExpire(context.Background(), guestByIDKey+guestHash(guest), q.admissionTTL)
			pipe.ZAdd(context.Background(), guestReserveKey, redis.Z{Score: float64(expires.UnixNano()), Member: id})
			return nil
		})
		return err
	}
	if _, err := q.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HSet(ctx, guestTicketKey+id, "status", "consuming")
		return nil
	}); err != nil {
		return nil, "", ErrGuestQueueUnavailable
	}
	live, owner, err := q.sessions.CreateForGuest(guestHash(guest), metadata)
	if err != nil {
		if restore() != nil {
			return nil, "", ErrGuestQueueUnavailable
		}
		return nil, "", err
	}
	_, transitionErr := q.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.ZRem(ctx, guestReserveKey, id)
		expires := time.Now().Add(q.guestSessionTTL)
		pipe.HSet(ctx, guestTicketKey+id, "status", "consumed", "session_id", live.ID, "expires_at", expires.Unix())
		pipe.Expire(ctx, guestTicketKey+id, q.guestSessionTTL)
		pipe.Expire(ctx, guestByIDKey+guestHash(guest), q.guestSessionTTL)
		return nil
	})
	if transitionErr != nil {
		// Delete invokes the server session-cleanup hook, which wakes this queue.
		// Consume holds q.mu, so deletion must run after this request releases it.
		go func(sessionID string) { _ = q.sessions.Delete(sessionID, "guest_queue_transition_failed") }(live.ID)
		if restore() != nil {
			return nil, "", ErrGuestQueueUnavailable
		}
		return nil, "", ErrGuestQueueUnavailable
	}
	time.AfterFunc(q.guestSessionTTL, func() { _ = q.sessions.Delete(live.ID, "guest_session_timeout") })
	q.publish(ctx)
	return live, owner, nil
}

// GuestSessionClosed removes the guest-to-ticket index only when it still
// refers to the terminated session. This permits a new queue entry after an
// explicit session close without allowing re-entry while it remains active.
func (q *GuestQueue) GuestSessionClosed(ctx context.Context, guestHashValue, sessionID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	id, err := q.client.Get(ctx, guestByIDKey+guestHashValue).Result()
	if err == redis.Nil {
		return nil
	}
	if err != nil {
		return ErrGuestQueueUnavailable
	}
	if current, err := q.client.HGet(ctx, guestTicketKey+id, "session_id").Result(); err == nil && current == sessionID {
		if err := q.client.Del(ctx, guestByIDKey+guestHashValue).Err(); err != nil {
			return ErrGuestQueueUnavailable
		}
	}
	return nil
}

// SessionClosed wakes waiting guests as soon as a guest slot is released.
func (q *GuestQueue) SessionClosed(ctx context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if changed, err := q.cleanupAndAdmit(ctx); err != nil {
		return err
	} else if changed {
		q.publish(ctx)
	}
	return nil
}

func (q *GuestQueue) statusLocked(ctx context.Context, hash, id string) (guestTicket, error) {
	values, err := q.client.HGetAll(ctx, guestTicketKey+id).Result()
	if err != nil {
		return guestTicket{}, ErrGuestQueueUnavailable
	}
	if len(values) == 0 {
		return guestTicket{}, ErrGuestTicketNotFound
	}
	if values["guest"] != hash {
		return guestTicket{}, ErrGuestForbidden
	}
	result := guestTicket{ID: id, Status: values["status"]}
	if unix, ok := parseInt64(values["expires_at"]); ok {
		result.ExpiresAt = time.Unix(unix, 0).UTC()
	}
	if result.Status == "waiting" {
		rank, err := q.client.ZRank(ctx, guestQueueKey, id).Result()
		if err == nil {
			result.Position = rank + 1
		}
	}
	if result.Status == "admitted" {
		result.AdmissionToken = values["admission_plain"]
	}
	return result, nil
}

func (q *GuestQueue) cleanupAndAdmit(ctx context.Context) (bool, error) {
	if q == nil || q.client == nil {
		return false, ErrGuestQueueUnavailable
	}
	now := time.Now()
	if err := q.client.ZRemRangeByScore(ctx, guestReserveKey, "-inf", fmt.Sprintf("%d", now.UnixNano())).Err(); err != nil {
		return false, ErrGuestQueueUnavailable
	}
	// A Redis outage after the consuming transition can leave the ticket in
	// that transient state. Its reservation intentionally remains, so a later
	// successful queue operation can make the same admission token retryable.
	reservedIDs, err := q.client.ZRange(ctx, guestReserveKey, 0, -1).Result()
	if err != nil {
		return false, ErrGuestQueueUnavailable
	}
	changed := false
	for _, id := range reservedIDs {
		status, err := q.client.HGet(ctx, guestTicketKey+id, "status").Result()
		if err != nil && err != redis.Nil {
			return false, ErrGuestQueueUnavailable
		}
		if status == "consuming" {
			if err := q.client.HSet(ctx, guestTicketKey+id, "status", "admitted").Err(); err != nil {
				return false, ErrGuestQueueUnavailable
			}
			changed = true
		}
	}
	reserved, err := q.client.ZCard(ctx, guestReserveKey).Result()
	if err != nil {
		return false, ErrGuestQueueUnavailable
	}
	active, limit := q.sessions.Capacity()
	for q.sessions.GuestCount()+int(reserved) < q.maxGuests && (limit == 0 || active+int(reserved) < limit) {
		token, err := newGuestSecret()
		if err != nil {
			return false, err
		}
		expires := now.Add(q.admissionTTL)
		result, err := admitHeadScript.Run(ctx, q.client,
			[]string{guestQueueKey, guestReserveKey, guestTicketKey, guestByIDKey},
			guestHash(token), token, expires.Unix(), q.admissionTTL.Milliseconds(), expires.UnixNano()).Text()
		if err != nil {
			return false, ErrGuestQueueUnavailable
		}
		if result == "" {
			break
		}
		if result == "__stale__" {
			continue
		}
		reserved++
		changed = true
	}
	return changed, nil
}

func (q *GuestQueue) publish(ctx context.Context) {
	_ = q.client.Publish(ctx, guestQueueChannel, "changed").Err()
}

func (q *GuestQueue) Subscribe(ctx context.Context) *redis.PubSub {
	return q.client.Subscribe(ctx, guestQueueChannel)
}

func parseInt64(value string) (int64, bool) {
	var n int64
	_, err := fmt.Sscan(value, &n)
	return n, err == nil
}
