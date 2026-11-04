//go:build guest_queue_load

package server

// 게스트 큐 잠금 부하 하니스(P1-2). 실제 Redis에 대기 티켓을 채우고 SSE 구독자처럼
// 변경 알림마다 Status를 부르게 한 뒤 두 가지를 잰다.
//
//   - fan-out: 알림 한 번에 모든 구독자가 Status를 마칠 때까지 걸린 시간과 Redis 명령 수
//   - 잠금 대기: 그동안 q.mu를 잡는 요청(기존 게스트의 POST /guest-queue)의 응답 시간
//
// 실행: redis-server --port 6390 --save '' --appendonly no 를 띄우고
// GUEST_LOAD_REDIS=127.0.0.1:6390 go test -tags guest_queue_load -run TestGuestQueueLoad -v ./internal/server
// 환경변수: GUEST_LOAD_RTT(왕복 지연 목록, 기본 "0,200us,1ms"),
// GUEST_LOAD_SUBSCRIBERS(구독자 수 목록, 기본 "100,400"), GUEST_LOAD_ROUNDS(기본 20).
// 하니스는 대상 Redis를 매 조합마다 FLUSHALL 하므로 운영 Redis를 가리키지 말 것.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
	"inno-live-server/internal/session"

	"github.com/redis/go-redis/v9"
)

func TestGuestQueueLoad(t *testing.T) {
	addr := os.Getenv("GUEST_LOAD_REDIS")
	if addr == "" {
		t.Skip("GUEST_LOAD_REDIS가 없어 건너뜀")
	}
	rounds := 20
	if value, err := strconv.Atoi(os.Getenv("GUEST_LOAD_ROUNDS")); err == nil && value > 0 {
		rounds = value
	}
	for _, rtt := range loadDurations(t, envOr("GUEST_LOAD_RTT", "0,200us,1ms")) {
		for _, subscribers := range loadInts(t, envOr("GUEST_LOAD_SUBSCRIBERS", "100,400")) {
			name := fmt.Sprintf("rtt=%s/subs=%d", rtt, subscribers)
			t.Run(name, func(t *testing.T) {
				result := runGuestQueueLoad(t, addr, rtt, subscribers, rounds)
				t.Logf("P1-2 결과 %s: fanout p50 %s p95 %s, 라운드당 Redis 명령 %d, Status p50 %s p99 %s, 잠금 요청 p50 %s 최대 %s",
					name, result.fanout50, result.fanout95, result.commandsPerRound,
					result.status50, result.status99, result.probe50, result.probeMax)
			})
		}
	}
}

type guestLoadResult struct {
	fanout50, fanout95 time.Duration
	status50, status99 time.Duration
	probe50, probeMax  time.Duration
	commandsPerRound   int64
}

func runGuestQueueLoad(t *testing.T, redisAddr string, rtt time.Duration, subscribers, rounds int) guestLoadResult {
	t.Helper()
	ctx := context.Background()
	admin := redis.NewClient(&redis.Options{Addr: redisAddr})
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.FlushAll(ctx).Err(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	target := redisAddr
	if rtt > 0 {
		target = startLatencyProxy(t, redisAddr, rtt/2)
	}
	manager, err := session.NewManager(config.Config{
		PrivacyMode:    config.PrivacyModeBypass,
		FFmpegPath:     "ffmpeg",
		UDPPortMin:     43000,
		UDPPortMax:     43100,
		FrameQueueSize: 2,
		MaxSessions:    10,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	// 게스트 슬롯 1개를 채워 둬 새 티켓이 모두 waiting에 머물게 한다.
	if _, _, err := manager.CreateForGuest("occupant", nil); err != nil {
		t.Fatalf("occupy guest slot: %v", err)
	}
	client := redis.NewClient(&redis.Options{Addr: target, PoolSize: 500})
	t.Cleanup(func() { _ = client.Close() })
	queue := &GuestQueue{
		client:       client,
		sessions:     manager,
		metrics:      metrics.New(),
		ttl:          10 * time.Minute,
		admissionTTL: time.Minute,
		maxGuests:    1,
	}

	tickets := subscribers
	if tickets > guestQueueMaxWaiting {
		tickets = guestQueueMaxWaiting
	}
	type subscriber struct{ guest, id string }
	subs := make([]subscriber, 0, subscribers)
	for i := 0; i < tickets; i++ {
		guest := fmt.Sprintf("guest-%d", i)
		ip := fmt.Sprintf("198.18.%d.%d:443", i/250, i%250+1)
		ticket, err := queue.CreateOrGet(ctx, guest, ip, "")
		if err != nil {
			t.Fatalf("create ticket %d: %v", i, err)
		}
		if ticket.Status != "waiting" {
			t.Fatalf("ticket %d status = %q, want waiting", i, ticket.Status)
		}
		subs = append(subs, subscriber{guest, ticket.ID})
	}
	for len(subs) < subscribers {
		subs = append(subs, subs[len(subs)%tickets])
	}

	var (
		round      atomic.Int64
		processed  atomic.Int64
		statusMu   sync.Mutex
		statusLats []time.Duration
	)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, sub := range subs {
		events, release, ok := queue.SubscribeEvents(sub.id)
		if !ok {
			t.Fatalf("subscribe %s: per-ticket limit", sub.id)
		}
		wg.Add(1)
		go func(guest, id string) {
			defer wg.Done()
			defer release()
			seen := int64(0)
			for {
				select {
				case <-stop:
					return
				case <-events:
				}
				current := round.Load()
				start := time.Now()
				if _, err := queue.Status(ctx, guest, id); err != nil {
					t.Errorf("status: %v", err)
					return
				}
				elapsed := time.Since(start)
				statusMu.Lock()
				statusLats = append(statusLats, elapsed)
				statusMu.Unlock()
				if current > seen {
					seen = current
					processed.Add(1)
				}
			}
		}(sub.guest, sub.id)
	}
	t.Cleanup(func() {
		close(stop)
		wg.Wait()
	})
	// Redis 구독이 실제로 붙을 때까지 기다린다.
	deadline := time.Now().Add(5 * time.Second)
	for {
		counts, err := admin.PubSubNumSub(ctx, guestQueueChannel).Result()
		if err == nil && counts[guestQueueChannel] > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("guest queue subscription did not attach")
		}
		time.Sleep(10 * time.Millisecond)
	}

	probeGuest := subs[0].guest
	var fanouts, probes []time.Duration
	var commands int64
	for r := 1; r <= rounds; r++ {
		processed.Store(0)
		round.Store(int64(r))
		before := redisCommands(t, admin)
		probeDone := make(chan []time.Duration)
		probeStop := make(chan struct{})
		go func() {
			var lats []time.Duration
			for {
				select {
				case <-probeStop:
					probeDone <- lats
					return
				default:
				}
				start := time.Now()
				if _, err := queue.CreateOrGet(ctx, probeGuest, "198.18.255.1:443", ""); err != nil {
					t.Errorf("probe: %v", err)
				}
				lats = append(lats, time.Since(start))
			}
		}()
		start := time.Now()
		queue.publish(ctx)
		for processed.Load() < int64(subscribers) {
			if time.Since(start) > 60*time.Second {
				t.Fatalf("round %d: %d/%d subscribers processed", r, processed.Load(), subscribers)
			}
			time.Sleep(200 * time.Microsecond)
		}
		fanouts = append(fanouts, time.Since(start))
		close(probeStop)
		probes = append(probes, <-probeDone...)
		commands += redisCommands(t, admin) - before
		// 다음 라운드 전에 남은 Status 호출이 끝나도록 잠시 쉰다.
		time.Sleep(50 * time.Millisecond)
	}
	statusMu.Lock()
	defer statusMu.Unlock()
	return guestLoadResult{
		fanout50:         percentile(fanouts, 0.5),
		fanout95:         percentile(fanouts, 0.95),
		status50:         percentile(statusLats, 0.5),
		status99:         percentile(statusLats, 0.99),
		probe50:          percentile(probes, 0.5),
		probeMax:         percentile(probes, 1),
		commandsPerRound: commands / int64(rounds),
	}
}

// startLatencyProxy는 양방향 전달마다 oneWay만큼 늦추는 TCP 프록시를 띄운다.
func startLatencyProxy(t *testing.T, upstream string, oneWay time.Duration) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	pipe := func(dst, src net.Conn) {
		defer dst.Close()
		buf := make([]byte, 32*1024)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				time.Sleep(oneWay)
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			remote, err := net.Dial("tcp", upstream)
			if err != nil {
				_ = conn.Close()
				continue
			}
			go pipe(remote, conn)
			go pipe(conn, remote)
		}
	}()
	return listener.Addr().String()
}

func redisCommands(t *testing.T, client *redis.Client) int64 {
	t.Helper()
	info, err := client.Info(context.Background(), "stats").Result()
	if err != nil {
		t.Fatalf("redis info: %v", err)
	}
	for _, line := range strings.Split(info, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "total_commands_processed:"); ok {
			n, _ := strconv.ParseInt(value, 10, 64)
			return n
		}
	}
	t.Fatal("total_commands_processed not found")
	return 0
}

func percentile(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[int(p*float64(len(sorted)-1))].Round(10 * time.Microsecond)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func loadDurations(t *testing.T, list string) []time.Duration {
	var out []time.Duration
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "0" {
			out = append(out, 0)
			continue
		}
		value, err := time.ParseDuration(item)
		if err != nil {
			t.Fatalf("GUEST_LOAD_RTT %q: %v", item, err)
		}
		out = append(out, value)
	}
	return out
}

func loadInts(t *testing.T, list string) []int {
	var out []int
	for _, item := range strings.Split(list, ",") {
		value, err := strconv.Atoi(strings.TrimSpace(item))
		if err != nil || value <= 0 {
			t.Fatalf("GUEST_LOAD_SUBSCRIBERS %q", item)
		}
		out = append(out, value)
	}
	return out
}
