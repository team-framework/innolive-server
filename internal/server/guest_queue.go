package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
	"inno-live-server/internal/session"

	"github.com/redis/go-redis/v9"
)

const (
	guestCookieName      = "innolive_guest"
	guestQueueKey        = "innolive:guest:queue"
	guestTicketKey       = "innolive:guest:ticket:"
	guestByIDKey         = "innolive:guest:by-id:"
	guestReserveKey      = "innolive:guest:reservations"
	guestQueueChannel    = "innolive:guest:queue:changed"
	guestQueueMaxWaiting = 100
)

var (
	ErrGuestQueueUnavailable = errors.New("guest queue unavailable")
	ErrGuestTicketNotFound   = errors.New("guest ticket not found")
	ErrGuestForbidden        = errors.New("guest ticket does not belong to caller")
	ErrGuestAdmissionInvalid = errors.New("guest admission token is invalid")
	ErrGuestQueueFull        = errors.New("guest queue is full")
	ErrGuestRateLimited      = errors.New("guest queue rate limited")
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

var rateLimitScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
return count
`)

// pruneWaitingScript removes queue members whose ticket hash has expired or
// was moved out of waiting. Redis expiry does not remove ZSET members, and
// this must run even when no guest slot is available for admission.
var pruneWaitingScript = redis.NewScript(`
local ids = redis.call('ZRANGE', KEYS[1], 0, -1)
local removed = 0
for _, id in ipairs(ids) do
  local status = redis.call('HGET', KEYS[2] .. id, 'status')
  if status ~= 'waiting' then
    redis.call('ZREM', KEYS[1], id)
    removed = removed + 1
  end
end
return removed
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
	metrics         *metrics.Registry
	ttl             time.Duration
	admissionTTL    time.Duration
	guestSessionTTL time.Duration
	maxGuests       int
	trustedProxies  []*net.IPNet
	mu              sync.Mutex
}

func NewGuestQueue(ctx context.Context, cfg config.Config, sessions *session.Manager, registries ...*metrics.Registry) (*GuestQueue, error) {
	if !cfg.GuestQueueEnabled {
		return nil, nil
	}
	client := redis.NewClient(&redis.Options{Addr: cfg.GuestQueueRedisAddr, Password: cfg.GuestQueueRedisPassword})
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connect guest queue Redis: %w", err)
	}
	var registry *metrics.Registry
	if len(registries) > 0 {
		registry = registries[0]
	}
	trusted := make([]*net.IPNet, 0, len(cfg.GuestQueueTrustedProxies))
	for _, cidr := range cfg.GuestQueueTrustedProxies {
		_, block, _ := net.ParseCIDR(cidr)
		trusted = append(trusted, block)
	}
	return &GuestQueue{client: client, sessions: sessions, metrics: registry, trustedProxies: trusted, ttl: cfg.GuestQueueTTL, admissionTTL: cfg.GuestAdmissionTTL, guestSessionTTL: cfg.GuestSessionTTL, maxGuests: cfg.MaxSessions / 2}, nil
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

func (q *GuestQueue) CreateOrGet(ctx context.Context, guest, remoteAddr, forwardedFor string) (guestTicket, error) {
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
		values, lookupErr := q.client.HGetAll(ctx, guestTicketKey+id).Result()
		if lookupErr != nil {
			return guestTicket{}, ErrGuestQueueUnavailable
		}
		if values["status"] == "consumed" && values["session_id"] != "" {
			if _, sessionErr := q.sessions.Get(values["session_id"]); errors.Is(sessionErr, session.ErrNotFound) {
				if err := q.client.Del(ctx, guestByIDKey+hash).Err(); err != nil {
					return guestTicket{}, ErrGuestQueueUnavailable
				}
			} else {
				return q.statusLocked(ctx, hash, id)
			}
		} else {
			return q.statusLocked(ctx, hash, id)
		}
	} else if err != redis.Nil {
		return guestTicket{}, ErrGuestQueueUnavailable
	}
	if err := q.allowIP(ctx, q.clientIP(remoteAddr, forwardedFor)); err != nil {
		return guestTicket{}, err
	}
	count, err := q.client.ZCard(ctx, guestQueueKey).Result()
	if err != nil {
		return guestTicket{}, ErrGuestQueueUnavailable
	}
	if count >= guestQueueMaxWaiting {
		return guestTicket{}, ErrGuestQueueFull
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

func (q *GuestQueue) clientIP(remoteAddr, forwardedFor string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	remote := net.ParseIP(host)
	if remote == nil || !q.isTrustedProxy(remote) {
		return host
	}

	// A trusted proxy appends the address it observed to any existing XFF
	// chain. Walk from that closest address towards the client, discarding only
	// addresses that belong to trusted proxy networks. Reading the leftmost
	// value would let a client prepend an arbitrary spoofed address.
	parts := strings.Split(forwardedFor, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := net.ParseIP(strings.TrimSpace(parts[i]))
		if candidate != nil && !q.isTrustedProxy(candidate) {
			return candidate.String()
		}
	}
	return host
}

func (q *GuestQueue) isTrustedProxy(ip net.IP) bool {
	for _, proxy := range q.trustedProxies {
		if proxy.Contains(ip) {
			return true
		}
	}
	return false
}

func (q *GuestQueue) allowIP(ctx context.Context, ip string) error {
	keyPart := guestHash(ip)
	for _, rule := range []struct {
		suffix string
		ttl    time.Duration
		max    int64
	}{{"minute", time.Minute, 5}, {"hour", time.Hour, 30}} {
		key := "innolive:guest:rate:" + rule.suffix + ":" + keyPart
		count, err := rateLimitScript.Run(ctx, q.client, []string{key}, rule.ttl.Milliseconds()).Int64()
		if err != nil {
			return ErrGuestQueueUnavailable
		}
		if count > rule.max {
			if q.metrics != nil {
				q.metrics.IncGuestRateLimited()
			}
			return ErrGuestRateLimited
		}
	}
	return nil
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
	q.observe(ctx)
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
	q.observe(ctx)
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
	current, err := q.client.HGet(ctx, guestTicketKey+id, "session_id").Result()
	if err != nil && err != redis.Nil {
		return ErrGuestQueueUnavailable
	}
	if current == sessionID {
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
	staleWaiting, err := pruneWaitingScript.Run(ctx, q.client, []string{guestQueueKey, guestTicketKey}).Int64()
	if err != nil {
		return false, ErrGuestQueueUnavailable
	}
	changed := staleWaiting > 0
	if staleWaiting > 0 && q.metrics != nil {
		for range staleWaiting {
			q.metrics.IncGuestExpired()
		}
	}
	expired, err := q.client.ZRemRangeByScore(ctx, guestReserveKey, "-inf", fmt.Sprintf("%d", now.UnixNano())).Result()
	if err != nil {
		return false, ErrGuestQueueUnavailable
	}
	if expired > 0 && q.metrics != nil {
		for range expired {
			q.metrics.IncGuestExpired()
		}
	}
	// A Redis outage after the consuming transition can leave the ticket in
	// that transient state. Its reservation intentionally remains, so a later
	// successful queue operation can make the same admission token retryable.
	reservedIDs, err := q.client.ZRange(ctx, guestReserveKey, 0, -1).Result()
	if err != nil {
		return false, ErrGuestQueueUnavailable
	}
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
		if q.metrics != nil {
			q.metrics.IncGuestAdmitted()
		}
	}
	q.observe(ctx)
	return changed, nil
}

func (q *GuestQueue) observe(ctx context.Context) {
	if q.metrics == nil {
		return
	}
	if waiting, err := q.client.ZCard(ctx, guestQueueKey).Result(); err == nil {
		q.metrics.SetGuestQueueWaiting(waiting)
	}
	if reserved, err := q.client.ZCard(ctx, guestReserveKey).Result(); err == nil {
		q.metrics.SetGuestReservations(reserved)
	}
	q.metrics.SetGuestActiveSessions(int64(q.sessions.GuestCount()))
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
