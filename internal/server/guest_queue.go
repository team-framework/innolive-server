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
	result, err := q.statusLocked(ctx, guestHash(guest), id)
	if err != nil {
		return err
	}
	if result.Status == "waiting" {
		if err := q.client.ZRem(ctx, guestQueueKey, id).Err(); err != nil {
			return ErrGuestQueueUnavailable
		}
	}
	if result.Status == "admitted" {
		if err := q.client.ZRem(ctx, guestReserveKey, id).Err(); err != nil {
			return ErrGuestQueueUnavailable
		}
	}
	_, err = q.client.HSet(ctx, guestTicketKey+id, "status", "cancelled").Result()
	if err != nil {
		return ErrGuestQueueUnavailable
	}
	_ = q.client.Del(ctx, guestByIDKey+guestHash(guest)).Err()
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
	restore := func() {
		expires := time.Now().Add(q.admissionTTL)
		_, _ = q.client.TxPipelined(context.Background(), func(pipe redis.Pipeliner) error {
			pipe.HSet(context.Background(), guestTicketKey+id, "status", "admitted", "expires_at", expires.Unix())
			pipe.Expire(context.Background(), guestTicketKey+id, q.admissionTTL)
			pipe.Expire(context.Background(), guestByIDKey+guestHash(guest), q.admissionTTL)
			pipe.ZAdd(context.Background(), guestReserveKey, redis.Z{Score: float64(expires.UnixNano()), Member: id})
			return nil
		})
	}
	if _, err := q.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HSet(ctx, guestTicketKey+id, "status", "consuming")
		pipe.ZRem(ctx, guestReserveKey, id)
		return nil
	}); err != nil {
		return nil, "", ErrGuestQueueUnavailable
	}
	live, owner, err := q.sessions.CreateForGuest(guestHash(guest), metadata)
	if err != nil {
		restore()
		return nil, "", err
	}
	_, transitionErr := q.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.ZRem(ctx, guestReserveKey, id)
		pipe.HSet(ctx, guestTicketKey+id, "status", "consumed", "session_id", live.ID)
		pipe.Expire(ctx, guestTicketKey+id, q.guestSessionTTL)
		return nil
	})
	if transitionErr != nil {
		_ = q.sessions.Delete(live.ID, "guest_queue_transition_failed")
		restore()
		return nil, "", ErrGuestQueueUnavailable
	}
	time.AfterFunc(q.guestSessionTTL, func() { _ = q.sessions.Delete(live.ID, "guest_session_timeout") })
	q.publish(ctx)
	return live, owner, nil
}

// SessionClosed wakes waiting guests as soon as a guest slot is released.
func (q *GuestQueue) SessionClosed() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if changed, err := q.cleanupAndAdmit(context.Background()); err != nil {
		return
	} else if changed {
		q.publish(context.Background())
	}
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
	reserved, err := q.client.ZCard(ctx, guestReserveKey).Result()
	if err != nil {
		return false, ErrGuestQueueUnavailable
	}
	active, limit := q.sessions.Capacity()
	changed := false
	for q.sessions.GuestCount()+int(reserved) < q.maxGuests && (limit == 0 || active+int(reserved) < limit) {
		ids, err := q.client.ZRange(ctx, guestQueueKey, 0, 0).Result()
		if err != nil {
			return false, ErrGuestQueueUnavailable
		}
		if len(ids) == 0 {
			break
		}
		id := ids[0]
		if err := q.client.ZRem(ctx, guestQueueKey, id).Err(); err != nil {
			return false, ErrGuestQueueUnavailable
		}
		values, err := q.client.HGetAll(ctx, guestTicketKey+id).Result()
		if err != nil {
			return false, ErrGuestQueueUnavailable
		}
		if len(values) == 0 || values["status"] != "waiting" {
			continue
		}
		token, err := newGuestSecret()
		if err != nil {
			return false, err
		}
		expires := now.Add(q.admissionTTL)
		_, err = q.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.HSet(ctx, guestTicketKey+id, "status", "admitted", "admission", guestHash(token), "admission_plain", token)
			pipe.Expire(ctx, guestTicketKey+id, q.admissionTTL)
			pipe.Expire(ctx, guestByIDKey+values["guest"], q.admissionTTL)
			pipe.HSet(ctx, guestTicketKey+id, "expires_at", expires.Unix())
			pipe.ZAdd(ctx, guestReserveKey, redis.Z{Score: float64(expires.UnixNano()), Member: id})
			return nil
		})
		if err != nil {
			return false, ErrGuestQueueUnavailable
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
