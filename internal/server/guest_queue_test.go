package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
	"inno-live-server/internal/origin"
	"inno-live-server/internal/session"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestGuestQueueClientIPUsesNearestUntrustedForwardedAddress(t *testing.T) {
	_, proxyNetwork, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	queue := &GuestQueue{trustedProxies: []*net.IPNet{proxyNetwork}}

	// The client prepended a spoofed value, then the trusted proxy appended
	// the real client address it observed. The rightmost untrusted value wins.
	got := queue.clientIP("10.0.0.10:443", "198.51.100.99, 203.0.113.25")
	if got != "203.0.113.25" {
		t.Fatalf("client IP = %q, want real nearest address", got)
	}

	// Intermediate trusted proxies are not considered clients either.
	got = queue.clientIP("10.0.0.10:443", "203.0.113.25, 10.0.0.20")
	if got != "203.0.113.25" {
		t.Fatalf("client IP through trusted proxy chain = %q", got)
	}
}

func TestGuestQueueClientIPIgnoresForwardedHeaderFromUntrustedPeer(t *testing.T) {
	_, proxyNetwork, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	queue := &GuestQueue{trustedProxies: []*net.IPNet{proxyNetwork}}

	got := queue.clientIP("203.0.113.25:443", "198.51.100.99")
	if got != "203.0.113.25" {
		t.Fatalf("client IP = %q, want direct peer", got)
	}
}

func TestGuestQueuePrunesExpiredWaitingTicketsWhenGuestCapacityIsFull(t *testing.T) {
	redisServer, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(redisServer.Close)

	manager, err := session.NewManager(config.Config{
		PrivacyMode:    config.PrivacyModeBypass,
		FFmpegPath:     "ffmpeg",
		UDPPortMin:     42000,
		UDPPortMax:     42100,
		FrameQueueSize: 2,
		MaxSessions:    2,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	if _, _, err := manager.CreateForGuest("active-guest", nil); err != nil {
		t.Fatalf("create active guest: %v", err)
	}

	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	for i := 0; i < guestQueueMaxWaiting; i++ {
		if err := client.ZAdd(context.Background(), guestQueueKey, redis.Z{Score: float64(i), Member: string(rune(i + 1))}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	queue := &GuestQueue{
		client:       client,
		sessions:     manager,
		ttl:          10 * time.Minute,
		admissionTTL: time.Minute,
		maxGuests:    1,
	}

	ticket, err := queue.CreateOrGet(context.Background(), "new-guest", "203.0.113.1:443", "")
	if err != nil {
		t.Fatalf("CreateOrGet with only expired queue members: %v", err)
	}
	if ticket.Status != "waiting" {
		t.Fatalf("ticket status = %q, want waiting while guest capacity is full", ticket.Status)
	}
	if waiting, err := client.ZCard(context.Background(), guestQueueKey).Result(); err != nil || waiting != 1 {
		t.Fatalf("live waiting members = %d, %v; want 1", waiting, err)
	}
}

func TestGuestQueueConsumeDoesNotRecreateExpiredAdmissionTicket(t *testing.T) {
	redisServer, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(redisServer.Close)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	manager := newGuestQueueTestManager(t, 2)
	queue := &GuestQueue{client: client, sessions: manager, ttl: time.Minute, admissionTTL: time.Minute, guestSessionTTL: time.Minute, maxGuests: 1}
	guest, id, token := "guest", "ticket", "admission-token"
	ctx := context.Background()
	if err := client.HSet(ctx, guestTicketKey+id, "guest", guestHash(guest), "status", "admitted", "admission", guestHash(token)).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.Expire(ctx, guestTicketKey+id, time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.Set(ctx, guestByIDKey+guestHash(guest), id, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	redisServer.FastForward(2 * time.Second)

	if _, _, err := queue.Consume(ctx, guest, token, nil); err != ErrGuestAdmissionInvalid {
		t.Fatalf("Consume expired admission error = %v, want ErrGuestAdmissionInvalid", err)
	}
	if exists, err := client.Exists(ctx, guestTicketKey+id).Result(); err != nil || exists != 0 {
		t.Fatalf("expired ticket recreated: exists=%d err=%v", exists, err)
	}
	if guests := manager.GuestCount(); guests != 0 {
		t.Fatalf("active guest sessions = %d, want 0", guests)
	}
}

func TestGuestSessionCreateReturnsCapacityExceeded(t *testing.T) {
	redisServer, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(redisServer.Close)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	manager := newGuestQueueTestManager(t, 2)
	if _, _, err := manager.Create(nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Create(nil); err != nil {
		t.Fatal(err)
	}
	queue := &GuestQueue{client: client, sessions: manager, ttl: time.Minute, admissionTTL: time.Minute, guestSessionTTL: time.Minute, maxGuests: 1}
	guest, id, token := "guest", "ticket", "admission-token"
	ctx := context.Background()
	if err := client.HSet(ctx, guestTicketKey+id, "guest", guestHash(guest), "status", "admitted", "admission", guestHash(token)).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.Expire(ctx, guestTicketKey+id, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.Set(ctx, guestByIDKey+guestHash(guest), id, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	origins, err := origin.NewConfig(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	application := New(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), manager, nil, origins, nil, nil)
	application.SetGuestQueue(queue)
	httpServer := httptest.NewServer(application.Handler())
	t.Cleanup(httpServer.Close)
	request, err := http.NewRequest(http.MethodPost, httpServer.URL+"/guest-sessions", bytes.NewBufferString(`{"admission_token":"admission-token"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: guestCookieName, Value: guest})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("POST /guest-sessions = %d: %s", response.StatusCode, body)
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != "capacity_exceeded" {
		t.Fatalf("error code = %q, want capacity_exceeded", payload.Error.Code)
	}
}

func newGuestQueueTestManager(t *testing.T, maxSessions int) *session.Manager {
	t.Helper()
	manager, err := session.NewManager(config.Config{
		PrivacyMode:    config.PrivacyModeBypass,
		FFmpegPath:     "ffmpeg",
		UDPPortMin:     42000,
		UDPPortMax:     42100,
		FrameQueueSize: 2,
		MaxSessions:    maxSessions,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	return manager
}
