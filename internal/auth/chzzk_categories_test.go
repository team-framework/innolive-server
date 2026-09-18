package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestChzzkSearchCategoriesReturnsPairs: 정상 경로 — 치지직이 주는
// categoryType·categoryId 쌍이 그대로 올라온다. posterImageUrl이 null인
// 항목도 결과에서 빠지지 않는다(실호출에서 실제로 온다).
func TestChzzkSearchCategoriesReturnsPairs(t *testing.T) {
	stub := newChzzkStub(t)

	categories, err := stub.client().SearchCategories(context.Background(), "리그", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(categories) != 2 {
		t.Fatalf("categories = %v, want both rows", categories)
	}
	if categories[0].Type != "GAME" || categories[0].ID != "League_of_Legends" {
		t.Fatalf("first category = %+v, want the type and id pair", categories[0])
	}
	if categories[1].PosterImageURL != "" {
		t.Fatalf("null posterImageUrl must decode as empty, got %q", categories[1].PosterImageURL)
	}
	if stub.lastSearchQuery != "리그" || stub.lastSearchSize != "5" {
		t.Fatalf("query = %q, size = %q, want the request parameters forwarded", stub.lastSearchQuery, stub.lastSearchSize)
	}
}

// TestChzzkSearchCategoriesReportsEnvelopeFailure: 치지직은 실패도 HTTP 200에
// 실어 보낸다. 봉투 code를 보지 않으면 빈 목록을 성공으로 오인한다.
func TestChzzkSearchCategoriesReportsEnvelopeFailure(t *testing.T) {
	stub := newChzzkStub(t)
	stub.rejectSearch = true

	_, err := stub.client().SearchCategories(context.Background(), "리그", 5)
	if !errors.Is(err, ErrChzzkPlatformUnavailable) {
		t.Fatalf("error = %v, want ErrChzzkPlatformUnavailable", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want the platform code reported", err)
	}
}

// TestChzzkSearchCategoriesRejectsInvalidArguments: 플랫폼 왕복 전에 거른다.
func TestChzzkSearchCategoriesRejectsInvalidArguments(t *testing.T) {
	stub := newChzzkStub(t)
	client := stub.client()

	if _, err := client.SearchCategories(context.Background(), "  ", 5); err == nil {
		t.Fatal("an empty query must be rejected")
	}
	if _, err := client.SearchCategories(context.Background(), "리그", MaxChzzkCategorySearchSize+1); err == nil {
		t.Fatal("a size above the platform limit must be rejected")
	}
	if stub.searches != 0 {
		t.Fatalf("search calls = %d, want no platform round-trip for invalid arguments", stub.searches)
	}
}

func getChzzkCategories(t *testing.T, handler http.Handler, accessToken, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/auth/chzzk/categories?"+rawQuery, nil)
	if accessToken != "" {
		request.Header.Set("Authorization", "Bearer "+accessToken)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// TestChzzkCategoriesEndpointRequiresBearer: 치지직 자격증명과 우리 쿼터로
// 나가는 호출이라 익명에게 열지 않는다.
func TestChzzkCategoriesEndpointRequiresBearer(t *testing.T) {
	_, handler, _ := testChzzkHandler(t, newChzzkStub(t), newMemoryStreamingAccountStore())
	if response := getChzzkCategories(t, handler, "", "query=리그"); response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

// TestChzzkCategoriesEndpointReturnsResults: 치지직 계정을 연결하지 않은
// 사용자도 검색할 수 있다 — Client 인증이라 사용자 토큰이 필요 없다.
func TestChzzkCategoriesEndpointReturnsResults(t *testing.T) {
	stub := newChzzkStub(t)
	store := newMemoryStreamingAccountStore()
	tokens, handler, _ := testChzzkHandler(t, stub, store)
	pair, err := tokens.IssuePair(context.Background(), uuid.New(), ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}

	response := getChzzkCategories(t, handler, pair.AccessToken, "query=리그&size=5")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	payload := struct {
		Categories []struct {
			Type           string `json:"category_type"`
			ID             string `json:"category_id"`
			Value          string `json:"category_value"`
			PosterImageURL string `json:"poster_image_url"`
		} `json:"categories"`
	}{}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Categories) != 2 || payload.Categories[0].ID != "League_of_Legends" {
		t.Fatalf("categories = %+v, want the searched pairs", payload.Categories)
	}
	if strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("the response must not expose credentials: %s", response.Body.String())
	}
}

// TestChzzkCategoriesEndpointRejectsBadRequest: query 누락과 범위 밖 size는
// 플랫폼에 닿기 전에 400으로 끊는다.
func TestChzzkCategoriesEndpointRejectsBadRequest(t *testing.T) {
	stub := newChzzkStub(t)
	tokens, handler, _ := testChzzkHandler(t, stub, newMemoryStreamingAccountStore())
	pair, err := tokens.IssuePair(context.Background(), uuid.New(), ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}

	for _, rawQuery := range []string{"", "query=%20", "query=리그&size=0", "query=리그&size=51", "query=리그&size=all"} {
		if response := getChzzkCategories(t, handler, pair.AccessToken, rawQuery); response.Code != http.StatusBadRequest {
			t.Fatalf("query %q: status = %d, want 400", rawQuery, response.Code)
		}
	}
	if stub.searches != 0 {
		t.Fatalf("search calls = %d, want no platform round-trip for rejected requests", stub.searches)
	}
}

// TestChzzkCategoriesEndpointReportsPlatformFailure: 치지직 쪽 실패는 우리
// 결함이 아니므로 502다. 사용자 메시지에 플랫폼 사유를 싣지 않는다.
func TestChzzkCategoriesEndpointReportsPlatformFailure(t *testing.T) {
	stub := newChzzkStub(t)
	stub.rejectSearch = true
	tokens, handler, _ := testChzzkHandler(t, stub, newMemoryStreamingAccountStore())
	pair, err := tokens.IssuePair(context.Background(), uuid.New(), ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}

	response := getChzzkCategories(t, handler, pair.AccessToken, "query=리그")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.Code)
	}
	if strings.Contains(response.Body.String(), "INVALID_CLIENT") {
		t.Fatalf("the platform reason must stay in the logs: %s", response.Body.String())
	}
}
