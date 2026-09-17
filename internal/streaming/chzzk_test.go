package streaming

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"inno-live-server/internal/auth"

	"github.com/google/uuid"
)

type errTokens struct{ err error }

func (e errTokens) AccessToken(context.Context, uuid.UUID) (string, error) { return "", e.err }

// chzzkStub은 봉투 형식의 Open API 스텁이다. 실패도 HTTP 200 + 봉투 code로
// 준다 — 실제 플랫폼 동작이고, HTTP 상태로 판정하는 구현을 잡아낸다.
type chzzkStub struct {
	t          *testing.T
	requests   []*http.Request
	bodies     []map[string]any
	settingErr int // PATCH lives/setting에 돌려줄 봉투 code (0이면 200)
	keyErr     int
	setting    string // GET lives/setting content
	streamKey  string
}

func (s *chzzkStub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests = append(s.requests, r)
		if r.Header.Get("Authorization") != "Bearer token-1" {
			s.t.Errorf("unexpected Authorization header %q", r.Header.Get("Authorization"))
		}
		body := map[string]any{}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		s.bodies = append(s.bodies, body)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "PATCH /open/v1/lives/setting":
			if s.settingErr != 0 {
				_, _ = w.Write([]byte(`{"code":` + itoa(s.settingErr) + `,"message":"설정 거절"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":200,"message":null,"content":null}`))
		case "GET /open/v1/streams/key":
			if s.keyErr != 0 {
				_, _ = w.Write([]byte(`{"code":` + itoa(s.keyErr) + `,"message":"토큰 만료"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":200,"message":null,"content":{"streamKey":"` + s.streamKey + `"}}`))
		case "GET /open/v1/lives/setting":
			_, _ = w.Write([]byte(`{"code":200,"message":null,"content":` + s.setting + `}`))
		default:
			s.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func newChzzkProviderForTest(t *testing.T, stub *chzzkStub, tokens AccessTokenProvider) *ChzzkProvider {
	t.Helper()
	server := stub.server()
	t.Cleanup(server.Close)
	provider, err := NewChzzkProvider(tokens)
	if err != nil {
		t.Fatalf("NewChzzkProvider: %v", err)
	}
	provider.apiBase = server.URL
	provider.httpClient = server.Client()
	return provider
}

func TestChzzkPrepareAppliesSettingAndBuildsIngestURL(t *testing.T) {
	stub := &chzzkStub{t: t, streamKey: "key-abc"}
	provider := newChzzkProviderForTest(t, stub, stubTokens{token: "token-1"})

	prepared, err := provider.Prepare(context.Background(), uuid.New(), PrepareOptions{
		Title:        "오늘 방송",
		CategoryType: "GAME",
		CategoryID:   "League_of_Legends",
		Tags:         []string{"게임", "롤"},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Provider != auth.StreamingProviderChzzk {
		t.Fatalf("provider = %q", prepared.Provider)
	}
	if prepared.IngestURL != ChzzkIngestURL+"/key-abc" {
		t.Fatalf("ingest url = %q", prepared.IngestURL)
	}
	// 방송 객체가 없는 플랫폼이라 id는 비어 있어야 한다 — 세션 정리 경로가
	// BroadcastID로 "치울 것이 있는가"를 판단한다.
	if prepared.BroadcastID != "" || prepared.StreamID != "" {
		t.Fatalf("unexpected ids %+v", prepared)
	}
	if len(stub.requests) != 2 || stub.requests[0].Method != http.MethodPatch || stub.requests[1].URL.Path != "/open/v1/streams/key" {
		t.Fatalf("request sequence = %v", stub.requests)
	}
	body := stub.bodies[0]
	if body["defaultLiveTitle"] != "오늘 방송" || body["categoryType"] != "GAME" || body["categoryId"] != "League_of_Legends" {
		t.Fatalf("setting body = %v", body)
	}
	if tags, _ := body["tags"].([]any); len(tags) != 2 {
		t.Fatalf("tags = %v", body["tags"])
	}
}

func TestChzzkPrepareSkipsSettingWhenNothingToApply(t *testing.T) {
	stub := &chzzkStub{t: t, streamKey: "key-abc"}
	provider := newChzzkProviderForTest(t, stub, stubTokens{token: "token-1"})

	if _, err := provider.Prepare(context.Background(), uuid.New(), PrepareOptions{}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// 채널 전역 설정을 빈 값으로 덮어쓰지 않는다.
	if len(stub.requests) != 1 || stub.requests[0].URL.Path != "/open/v1/streams/key" {
		t.Fatalf("request sequence = %v", stub.requests)
	}
}

func TestChzzkPrepareRejectsNotConnectedAccount(t *testing.T) {
	stub := &chzzkStub{t: t, streamKey: "key-abc"}
	provider := newChzzkProviderForTest(t, stub, errTokens{err: auth.ErrStreamingNotConnected})

	_, err := provider.Prepare(context.Background(), uuid.New(), PrepareOptions{Title: "x"})
	if !errors.Is(err, auth.ErrStreamingNotConnected) {
		t.Fatalf("err = %v, want ErrStreamingNotConnected", err)
	}
	if len(stub.requests) != 0 {
		t.Fatalf("platform must not be called without a token, got %d requests", len(stub.requests))
	}
}

func TestChzzkPrepareFailsWhenSettingRejected(t *testing.T) {
	stub := &chzzkStub{t: t, streamKey: "key-abc", settingErr: 400}
	provider := newChzzkProviderForTest(t, stub, stubTokens{token: "token-1"})

	_, err := provider.Prepare(context.Background(), uuid.New(), PrepareOptions{Title: "x"})
	if !errors.Is(err, ErrChzzkPlatform) {
		t.Fatalf("err = %v, want ErrChzzkPlatform", err)
	}
	if !strings.Contains(err.Error(), "설정 거절") {
		t.Fatalf("platform message missing: %v", err)
	}
	// 설정이 거절되면 스트림키를 조회하지 않는다.
	if len(stub.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(stub.requests))
	}
}

func TestChzzkPrepareMapsUnauthorizedToReconnectRequired(t *testing.T) {
	stub := &chzzkStub{t: t, keyErr: 401}
	provider := newChzzkProviderForTest(t, stub, stubTokens{token: "token-1"})

	_, err := provider.Prepare(context.Background(), uuid.New(), PrepareOptions{})
	if !errors.Is(err, auth.ErrStreamingReconnectRequired) {
		t.Fatalf("err = %v, want ErrStreamingReconnectRequired", err)
	}
}

func TestChzzkPrepareErrorNeverContainsStreamKey(t *testing.T) {
	// content가 비정상이라 디코드 실패로 가는 경로 — 에러 문자열에 본문이
	// 실리지 않아야 한다.
	stub := &chzzkStub{t: t}
	provider := newChzzkProviderForTest(t, stub, stubTokens{token: "token-1"})
	stub.streamKey = `"` // content가 {"streamKey":"""}가 되어 JSON이 깨진다

	_, err := provider.Prepare(context.Background(), uuid.New(), PrepareOptions{})
	if err == nil {
		t.Fatal("expected decode error")
	}
	if strings.Contains(err.Error(), "streamKey") {
		t.Fatalf("error leaks response body: %v", err)
	}
}

func TestChzzkDefaultsReadsChannelSetting(t *testing.T) {
	stub := &chzzkStub{t: t, setting: `{"defaultLiveTitle":"채널 제목","category":{"categoryType":"ETC","categoryId":"talk","categoryValue":"토크"},"tags":["a","b"]}`}
	provider := newChzzkProviderForTest(t, stub, stubTokens{token: "token-1"})

	defaults, err := provider.Defaults(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Defaults: %v", err)
	}
	if defaults.Title != "채널 제목" || defaults.CategoryType != "ETC" || defaults.CategoryID != "talk" || len(defaults.Tags) != 2 {
		t.Fatalf("defaults = %+v", defaults)
	}
}

func TestChzzkDefaultsHandlesNullCategory(t *testing.T) {
	stub := &chzzkStub{t: t, setting: `{"defaultLiveTitle":"","category":null,"tags":[]}`}
	provider := newChzzkProviderForTest(t, stub, stubTokens{token: "token-1"})

	defaults, err := provider.Defaults(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Defaults: %v", err)
	}
	if defaults.CategoryType != "" || defaults.CategoryID != "" {
		t.Fatalf("category should be empty, got %+v", defaults)
	}
	if defaults.Title != defaultBroadcastTitle {
		t.Fatalf("title = %q, want fallback", defaults.Title)
	}
}

func TestChzzkLifecycleCallsAreNoOps(t *testing.T) {
	stub := &chzzkStub{t: t}
	provider := newChzzkProviderForTest(t, stub, stubTokens{token: "token-1"})
	prepared := PreparedBroadcast{Provider: auth.StreamingProviderChzzk}
	ctx := context.Background()
	if err := provider.GoLive(ctx, uuid.New(), prepared); err != nil {
		t.Fatalf("GoLive: %v", err)
	}
	if err := provider.EndLive(ctx, uuid.New(), prepared); err != nil {
		t.Fatalf("EndLive: %v", err)
	}
	if err := provider.Stop(ctx, uuid.New(), prepared); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(stub.requests) != 0 {
		t.Fatalf("no platform call expected, got %d", len(stub.requests))
	}
}
