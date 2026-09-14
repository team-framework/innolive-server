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
	guestSSEMaxPerTicket = 4
)

var (
	ErrGuestQueueUnavailable = errors.New("guest queue unavailable")
	ErrGuestTicketNotFound   = errors.New("guest ticket not found")
	ErrGuestForbidden        = errors.New("guest ticket does not belong to caller")
	ErrGuestAdmissionInvalid = errors.New("guest admission token is invalid")
	ErrGuestQueueFull        = errors.New("guest queue is full")
	ErrGuestRateLimited      = errors.New("guest queue rate limited")
)

// admitHeadScript는 현재 FIFO 선두를 원자적으로 확보해 입장 허가 상태로 바꾼다.
// dequeue와 ticket 변경을 하나의 Redis script로 묶어 연결 실패에도 대기 ticket이
// ZSET 밖에 고립되지 않게 한다.
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

// pruneWaitingScript는 ticket hash가 만료됐거나 waiting 상태를 벗어난 queue 멤버를
// 제거한다. Redis key 만료만으로는 ZSET 멤버가 제거되지 않으므로, 입장 가능한
// guest 슬롯이 없어도 이를 실행해야 한다.
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

// claimAdmissionScript는 guest index와 입장 ticket을 검증하면서 consuming 상태로
// 바꾼다. 세션 생성 중 PEXPIRE로 두 key를 유지해, 만료 뒤 HSET이 guest index 없는
// 새 ticket을 만드는 일을 막는다.
var claimAdmissionScript = redis.NewScript(`
if redis.call('GET', KEYS[2]) ~= ARGV[1] then return '__invalid__' end
if redis.call('HGET', KEYS[1], 'status') ~= 'admitted' then return '__invalid__' end
if redis.call('HGET', KEYS[1], 'guest') ~= ARGV[2] then return '__invalid__' end
if redis.call('HGET', KEYS[1], 'admission') ~= ARGV[3] then return '__invalid__' end
if redis.call('PTTL', KEYS[1]) <= 0 or redis.call('PTTL', KEYS[2]) <= 0 then return '__invalid__' end
redis.call('HSET', KEYS[1], 'status', 'consuming')
redis.call('PEXPIRE', KEYS[1], ARGV[4])
redis.call('PEXPIRE', KEYS[2], ARGV[4])
return 'ok'
`)

var restoreAdmissionScript = redis.NewScript(`
if redis.call('GET', KEYS[2]) ~= ARGV[1] then return '__invalid__' end
if redis.call('HGET', KEYS[1], 'status') ~= 'consuming' then return '__invalid__' end
redis.call('HSET', KEYS[1], 'status', 'admitted', 'expires_at', ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
redis.call('PEXPIRE', KEYS[2], ARGV[3])
redis.call('ZADD', KEYS[3], ARGV[4], ARGV[1])
return 'ok'
`)

var completeAdmissionScript = redis.NewScript(`
if redis.call('GET', KEYS[2]) ~= ARGV[1] then return '__invalid__' end
if redis.call('HGET', KEYS[1], 'status') ~= 'consuming' then return '__invalid__' end
redis.call('ZREM', KEYS[3], ARGV[1])
redis.call('HSET', KEYS[1], 'status', 'consumed', 'session_id', ARGV[2], 'expires_at', ARGV[3])
redis.call('PEXPIRE', KEYS[1], ARGV[4])
redis.call('PEXPIRE', KEYS[2], ARGV[4])
return 'ok'
`)

// heartbeatTicketScript는 ticket hash와 guest index가 모두 존재하고 일치할 때만
// waiting ticket을 연장한다. HSET을 마지막에 실행해 pipeline과 달리 앞선 EXPIRE가
// 실패했을 때 만료된 hash를 다시 만들지 않는다.
var heartbeatTicketScript = redis.NewScript(`
local status = redis.call('HGET', KEYS[1], 'status')
if not status then return '__missing__' end
if redis.call('HGET', KEYS[1], 'guest') ~= ARGV[1] then return '__forbidden__' end
if status ~= 'waiting' then return '__invalid__' end
if redis.call('GET', KEYS[2]) ~= ARGV[2] then return '__missing__' end
if redis.call('PTTL', KEYS[1]) <= 0 or redis.call('PTTL', KEYS[2]) <= 0 then return '__missing__' end
redis.call('HSET', KEYS[1], 'expires_at', ARGV[3])
redis.call('PEXPIRE', KEYS[1], ARGV[4])
redis.call('PEXPIRE', KEYS[2], ARGV[4])
return 'ok'
`)

type guestTicket struct {
	ID             string    `json:"ticket_id"`
	Status         string    `json:"status"`
	Position       int64     `json:"position,omitempty"`
	AdmissionToken string    `json:"admission_token,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// GuestQueue는 짧은 수명의 입장 상태만 관리한다. WebRTC 세션은 process-local인
// session.Manager에 남으므로 Redis를 사용할 수 없을 때 queue는 의도적으로
// fail-closed 처리한다.
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

	sseMu    sync.Mutex
	sseConns map[string]int
	bcast    guestBroadcast
}

// guestBroadcast는 queue 변경 알림을 프로세스당 하나의 Redis 구독으로 받아
// 등록된 SSE 연결에 fan-out한다. 마지막 구독자가 나가면 Redis 구독을 닫아
// SSE 연결 수가 Redis 구독 수를 늘리지 않게 한다.
type guestBroadcast struct {
	mu     sync.Mutex
	subs   map[chan struct{}]struct{}
	pubsub *redis.PubSub
	cancel context.CancelFunc
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
	return &GuestQueue{client: client, sessions: sessions, metrics: registry, trustedProxies: parseTrustedProxyCIDRs(cfg.GuestQueueTrustedProxies), ttl: cfg.GuestQueueTTL, admissionTTL: cfg.GuestAdmissionTTL, guestSessionTTL: cfg.GuestSessionTTL, maxGuests: cfg.MaxSessions / 2}, nil
}

func (q *GuestQueue) Close() error {
	if q == nil || q.client == nil {
		return nil
	}
	q.bcast.mu.Lock()
	q.bcast.stopLocked()
	q.bcast.mu.Unlock()
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
	return clientIPFromForwarded(remoteAddr, forwardedFor, q.trustedProxies)
}

func (q *GuestQueue) isTrustedProxy(ip net.IP) bool {
	return isTrustedProxy(ip, q.trustedProxies)
}

func parseTrustedProxyCIDRs(cidrs []string) []*net.IPNet {
	trusted := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, block, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		trusted = append(trusted, block)
	}
	return trusted
}

func clientIPFromForwarded(remoteAddr, forwardedFor string, trusted []*net.IPNet) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	remote := net.ParseIP(host)
	if remote == nil || !isTrustedProxy(remote, trusted) {
		return host
	}

	// 신뢰된 proxy는 기존 XFF 체인 뒤에 자신이 관찰한 주소를 붙인다. 가장 가까운
	// 주소부터 클라이언트 방향으로 순회하며 신뢰된 proxy 대역만 건너뛴다. 왼쪽 첫
	// 값을 읽으면 클라이언트가 임의의 spoofed 주소를 앞에 넣어 우회할 수 있다.
	parts := strings.Split(forwardedFor, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := net.ParseIP(strings.TrimSpace(parts[i]))
		if candidate != nil && !isTrustedProxy(candidate, trusted) {
			return candidate.String()
		}
	}
	return host
}

// remoteIsTrustedProxy는 요청의 RemoteAddr(직접 연결한 상대)가 신뢰 프록시
// 대역에 속하는지 판단한다. per-IP 상한을 적용해도 되는지 가리는 데 쓴다.
func remoteIsTrustedProxy(remoteAddr string, trusted []*net.IPNet) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && isTrustedProxy(ip, trusted)
}

func isTrustedProxy(ip net.IP, trusted []*net.IPNet) bool {
	for _, proxy := range trusted {
		if proxy != nil && proxy.Contains(ip) {
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
		result, err := heartbeatTicketScript.Run(ctx, q.client,
			[]string{guestTicketKey + id, guestByIDKey + guestHash(guest)},
			guestHash(guest), id, expires.Unix(), q.ttl.Milliseconds()).Text()
		if err != nil {
			return guestTicket{}, ErrGuestQueueUnavailable
		}
		switch result {
		case "ok":
		case "__missing__":
			return guestTicket{}, ErrGuestTicketNotFound
		case "__forbidden__":
			return guestTicket{}, ErrGuestForbidden
		default:
			return guestTicket{}, ErrGuestAdmissionInvalid
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
	if q.sessions.GuestCount() >= q.maxGuests {
		return nil, "", ErrGuestAdmissionInvalid
	}
	restore := func() error {
		expires := time.Now().Add(q.admissionTTL)
		result, err := restoreAdmissionScript.Run(context.Background(), q.client,
			[]string{guestTicketKey + id, guestByIDKey + guestHash(guest), guestReserveKey},
			id, expires.Unix(), q.admissionTTL.Milliseconds(), expires.UnixNano()).Text()
		if err != nil || result != "ok" {
			return ErrGuestQueueUnavailable
		}
		return nil
	}
	result, err := claimAdmissionScript.Run(ctx, q.client,
		[]string{guestTicketKey + id, guestByIDKey + guestHash(guest)},
		id, guestHash(guest), guestHash(token), q.admissionTTL.Milliseconds()).Text()
	if err != nil {
		return nil, "", ErrGuestQueueUnavailable
	}
	if result != "ok" {
		return nil, "", ErrGuestAdmissionInvalid
	}
	live, owner, err := q.sessions.CreateForGuest(guestHash(guest), metadata)
	if err != nil {
		if restore() != nil {
			return nil, "", ErrGuestQueueUnavailable
		}
		return nil, "", err
	}
	expires := time.Now().Add(q.guestSessionTTL)
	result, transitionErr := completeAdmissionScript.Run(ctx, q.client,
		[]string{guestTicketKey + id, guestByIDKey + guestHash(guest), guestReserveKey},
		id, live.ID, expires.Unix(), q.guestSessionTTL.Milliseconds()).Text()
	if transitionErr != nil || result != "ok" {
		// Delete는 이 queue를 깨우는 서버 session-cleanup hook을 동기 호출한다.
		// Consume이 q.mu를 잡고 있으므로 요청이 잠금을 해제한 뒤 삭제해야 한다.
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

// GuestSessionClosed는 guest-to-ticket index가 종료된 세션을 여전히 가리킬 때만
// 제거한다. 활성 세션 중 재입장은 막으면서 명시적 종료 뒤 새 queue 등록은 허용한다.
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

// SessionClosed는 guest 슬롯이 해제되는 즉시 대기 guest를 깨운다.
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
	// consuming 전이 뒤 Redis 장애가 나면 ticket이 그 임시 상태에 남을 수 있다.
	// reservation은 의도적으로 유지하므로 이후 성공한 queue 작업에서 같은 admission
	// token을 다시 시도 가능한 상태로 복구할 수 있다.
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

// SubscribeEvents는 티켓별 동시 SSE 연결 상한을 적용하고, 공유 Redis 구독에서
// 변경 알림을 받는 채널과 해제 함수를 반환한다. 상한을 넘으면 ok=false를 준다.
// release는 여러 번 호출해도 안전하다.
func (q *GuestQueue) SubscribeEvents(id string) (<-chan struct{}, func(), bool) {
	q.sseMu.Lock()
	if q.sseConns == nil {
		q.sseConns = make(map[string]int)
	}
	if q.sseConns[id] >= guestSSEMaxPerTicket {
		q.sseMu.Unlock()
		return nil, nil, false
	}
	q.sseConns[id]++
	q.sseMu.Unlock()

	ch := q.bcast.add(q.client)
	var once sync.Once
	release := func() {
		once.Do(func() {
			q.bcast.remove(ch)
			q.sseMu.Lock()
			if q.sseConns[id]--; q.sseConns[id] <= 0 {
				delete(q.sseConns, id)
			}
			q.sseMu.Unlock()
		})
	}
	return ch, release, true
}

func (b *guestBroadcast) add(client *redis.Client) chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs == nil {
		b.subs = make(map[chan struct{}]struct{})
	}
	if b.pubsub == nil {
		ctx, cancel := context.WithCancel(context.Background())
		b.cancel = cancel
		b.pubsub = client.Subscribe(ctx, guestQueueChannel)
		go b.loop(ctx, b.pubsub)
	}
	ch := make(chan struct{}, 1)
	b.subs[ch] = struct{}{}
	return ch
}

func (b *guestBroadcast) remove(ch chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[ch]; !ok {
		return
	}
	delete(b.subs, ch)
	if len(b.subs) == 0 {
		b.stopLocked()
	}
}

// stopLocked는 b.mu를 잡은 채 호출한다.
func (b *guestBroadcast) stopLocked() {
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	if b.pubsub != nil {
		_ = b.pubsub.Close()
		b.pubsub = nil
	}
}

func (b *guestBroadcast) loop(ctx context.Context, pubsub *redis.PubSub) {
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			b.mu.Lock()
			for sub := range b.subs {
				// 버퍼 1 채널에 논블로킹 전달 — 느린 수신자가 fan-out을
				// 막지 않고, 알림은 병합돼도 handler가 최신 상태를 다시 조회한다.
				select {
				case sub <- struct{}{}:
				default:
				}
			}
			b.mu.Unlock()
		}
	}
}

func parseInt64(value string) (int64, bool) {
	var n int64
	_, err := fmt.Sscan(value, &n)
	return n, err == nil
}
