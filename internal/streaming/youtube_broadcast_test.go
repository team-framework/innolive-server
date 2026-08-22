package streaming

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestPrepareAppliesDescriptionCategoryAndThumbnail: 설명은 insert에,
// 카테고리는 videos.update에, 썸네일은 업로드 경로에 실려야 한다.
func TestPrepareAppliesDescriptionCategoryAndThumbnail(t *testing.T) {
	stub := &youtubeAPIStub{}
	store := newMemoryStore()
	userID := uuid.New()
	connectedAccount(t, store, userID)
	provider := testProviderWith(t, stub, store)

	prepared, err := provider.Prepare(context.Background(), userID, PrepareOptions{
		Title:       "내 방송",
		Description: "설명 #태그",
		MadeForKids: boolPtr(false),
		CategoryID:  "20",
		Thumbnail:   &Thumbnail{MIME: "image/png", Data: []byte("\x89PNG-bytes")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", prepared.Warnings)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	snippet := stub.lastBroadcast["snippet"].(map[string]any)
	if snippet["description"] != "설명 #태그" {
		t.Fatalf("insert description = %v", snippet["description"])
	}
	if stub.videoUpdates != 1 {
		t.Fatalf("videos.update calls = %d, want 1", stub.videoUpdates)
	}
	updated := stub.lastVideoUpdate["snippet"].(map[string]any)
	// snippet은 통째 교체라 제목·설명이 함께 실려야 지워지지 않는다.
	if updated["categoryId"] != "20" || updated["title"] != "내 방송" || updated["description"] != "설명 #태그" {
		t.Fatalf("videos.update snippet = %v, want full snippet with category", updated)
	}
	if stub.lastVideoUpdate["id"] != "broadcast-id-1" {
		t.Fatalf("videos.update id = %v", stub.lastVideoUpdate["id"])
	}
	if stub.thumbnailUploads != 1 {
		t.Fatalf("thumbnails.set calls = %d, want 1", stub.thumbnailUploads)
	}
	if string(stub.lastThumbnailBody) != "\x89PNG-bytes" || stub.lastThumbnailType != "image/png" {
		t.Fatalf("thumbnail upload body/type = %q/%q", stub.lastThumbnailBody, stub.lastThumbnailType)
	}
}

// TestPrepareSkipsOptionalCallsWhenUnset: 카테고리·썸네일이 없으면 추가 호출을
// 하지 않아야 한다(할당량 낭비 방지).
func TestPrepareSkipsOptionalCallsWhenUnset(t *testing.T) {
	stub := &youtubeAPIStub{}
	store := newMemoryStore()
	userID := uuid.New()
	connectedAccount(t, store, userID)
	provider := testProviderWith(t, stub, store)

	if _, err := provider.Prepare(context.Background(), userID, PrepareOptions{MadeForKids: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.videoUpdates != 0 || stub.thumbnailUploads != 0 {
		t.Fatalf("optional calls = %d/%d, want 0/0", stub.videoUpdates, stub.thumbnailUploads)
	}
}

// TestPrepareReturnsWarningsOnOptionalFailure: 선택 항목이 실패해도 방송 준비는
// 성공하고 경고만 붙어야 한다. 썸네일 403은 채널 인증 안내로 바뀐다.
func TestPrepareReturnsWarningsOnOptionalFailure(t *testing.T) {
	stub := &youtubeAPIStub{videoUpdateStatus: 400, thumbnailSetStatus: 403}
	store := newMemoryStore()
	userID := uuid.New()
	connectedAccount(t, store, userID)
	provider := testProviderWith(t, stub, store)

	prepared, err := provider.Prepare(context.Background(), userID, PrepareOptions{
		MadeForKids: boolPtr(false),
		CategoryID:  "999",
		Thumbnail:   &Thumbnail{MIME: "image/jpeg", Data: []byte("jpeg-bytes")},
	})
	if err != nil {
		t.Fatalf("prepare failed on optional settings: %v", err)
	}
	if prepared.BroadcastID != "broadcast-id-1" || prepared.IngestURL == "" {
		t.Fatalf("prepared = %+v, want a usable broadcast", prepared)
	}
	codes := make([]string, 0, len(prepared.Warnings))
	for _, warning := range prepared.Warnings {
		codes = append(codes, warning.Code)
	}
	if len(codes) != 2 || codes[0] != "category_not_applied" || codes[1] != "thumbnail_forbidden" {
		t.Fatalf("warning codes = %v", codes)
	}
	if !strings.Contains(prepared.Warnings[1].Message, ThumbnailHelpURL) {
		t.Fatalf("thumbnail warning = %q, want channel verification guidance", prepared.Warnings[1].Message)
	}
}

// TestPrepareDoesNotAutoStart: 준비만으로 방송이 시청자에게 나가면 안 되므로
// enableAutoStart는 꺼져 있어야 한다(#142). 종료는 계속 autoStop이 맡는다.
func TestPrepareDoesNotAutoStart(t *testing.T) {
	stub := &youtubeAPIStub{}
	store := newMemoryStore()
	userID := uuid.New()
	connectedAccount(t, store, userID)
	provider := testProviderWith(t, stub, store)

	if _, err := provider.Prepare(context.Background(), userID, PrepareOptions{MadeForKids: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	contentDetails := stub.lastBroadcast["contentDetails"].(map[string]any)
	if contentDetails["enableAutoStart"] != false {
		t.Fatalf("enableAutoStart = %v, want false", contentDetails["enableAutoStart"])
	}
	if contentDetails["enableAutoStop"] != true {
		t.Fatalf("enableAutoStop = %v, want true", contentDetails["enableAutoStop"])
	}
}

// TestGoLiveTransitionsBroadcast: GoLive는 준비된 방송 id로 transition(live)을
// 호출해야 한다.
func TestGoLiveTransitionsBroadcast(t *testing.T) {
	stub := &youtubeAPIStub{}
	store := newMemoryStore()
	userID := uuid.New()
	connectedAccount(t, store, userID)
	provider := testProviderWith(t, stub, store)

	if err := provider.GoLive(context.Background(), userID, PreparedBroadcast{BroadcastID: "broadcast-id-1"}); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.transitions != 1 {
		t.Fatalf("transition calls = %d, want 1", stub.transitions)
	}
	if !strings.Contains(stub.transitionQuery, "broadcastStatus=live") || !strings.Contains(stub.transitionQuery, "id=broadcast-id-1") {
		t.Fatalf("transition query = %q", stub.transitionQuery)
	}
}

// TestGoLiveReportsNotReady: 송출 프레임이 아직 플랫폼에 도착하지 않은 상태는
// 재시도로 풀리므로 일반 실패와 구분돼야 한다.
func TestGoLiveReportsNotReady(t *testing.T) {
	stub := &youtubeAPIStub{
		transitionStatus: 403,
		transitionBody:   `{"error":{"code":403,"message":"The broadcast is not ready.","errors":[{"reason":"errorStreamInactive"}]}}`,
	}
	store := newMemoryStore()
	userID := uuid.New()
	connectedAccount(t, store, userID)
	provider := testProviderWith(t, stub, store)

	err := provider.GoLive(context.Background(), userID, PreparedBroadcast{BroadcastID: "broadcast-id-1"})
	if !errors.Is(err, ErrBroadcastNotReady) {
		t.Fatalf("GoLive() error = %v, want ErrBroadcastNotReady", err)
	}
}

// TestStopDeletesBroadcast: autoStart를 끈 뒤로 라이브가 되지 못한 방송은
// 아무도 정리해주지 않으므로 Stop이 지운다.
func TestStopDeletesBroadcast(t *testing.T) {
	stub := &youtubeAPIStub{}
	store := newMemoryStore()
	userID := uuid.New()
	connectedAccount(t, store, userID)
	provider := testProviderWith(t, stub, store)

	if err := provider.Stop(context.Background(), userID, PreparedBroadcast{BroadcastID: "broadcast-id-1"}); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.deletes != 1 || !strings.Contains(stub.deleteQuery, "id=broadcast-id-1") {
		t.Fatalf("deletes = %d, query = %q, want the broadcast removed", stub.deletes, stub.deleteQuery)
	}
}

// TestStopWithoutBroadcastIsNoOp: 준비된 방송이 없으면 플랫폼을 부르지 않는다.
func TestStopWithoutBroadcastIsNoOp(t *testing.T) {
	stub := &youtubeAPIStub{}
	store := newMemoryStore()
	userID := uuid.New()
	connectedAccount(t, store, userID)
	provider := testProviderWith(t, stub, store)

	if err := provider.Stop(context.Background(), userID, PreparedBroadcast{}); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.deletes != 0 {
		t.Fatalf("deletes = %d, want 0", stub.deletes)
	}
}

// TestPrepareAutoStartOptIn: 제거 예정인 stream/start 경로만 autoStart를 켠다.
func TestPrepareAutoStartOptIn(t *testing.T) {
	stub := &youtubeAPIStub{}
	store := newMemoryStore()
	userID := uuid.New()
	connectedAccount(t, store, userID)
	provider := testProviderWith(t, stub, store)

	if _, err := provider.Prepare(context.Background(), userID, PrepareOptions{
		MadeForKids: boolPtr(false),
		AutoStart:   true,
	}); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	contentDetails := stub.lastBroadcast["contentDetails"].(map[string]any)
	if contentDetails["enableAutoStart"] != true {
		t.Fatalf("enableAutoStart = %v, want true for the legacy path", contentDetails["enableAutoStart"])
	}
}
