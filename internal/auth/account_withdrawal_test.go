package auth

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

type stubWithdrawalStore struct {
	credential    *appleRevocationCredential
	credentialErr error
	markErr       error
	marked        uuid.UUID
	missing       bool
	userStatus    UserStatus
	stateErr      error
}

func (s *stubWithdrawalStore) AppleRevocationCredential(_ context.Context, _ uuid.UUID) (*appleRevocationCredential, error) {
	return s.credential, s.credentialErr
}

func (s *stubWithdrawalStore) UserState(_ context.Context, userID uuid.UUID) (WithdrawalUserState, error) {
	if s.stateErr != nil {
		return WithdrawalUserUnknown, s.stateErr
	}
	if s.missing || s.marked == userID {
		return WithdrawalUserMissing, nil
	}
	if s.userStatus != "" && s.userStatus != UserStatusActive {
		return WithdrawalUserInactive, nil
	}
	return WithdrawalUserActive, nil
}

func (s *stubWithdrawalStore) MarkUserDeleted(_ context.Context, userID uuid.UUID, _ time.Time) error {
	s.marked = userID
	return s.markErr
}

type stubAppleRevoker struct {
	err  error
	seen string
}

func (s *stubAppleRevoker) Revoke(_ context.Context, refreshToken string) error {
	s.seen = refreshToken
	return s.err
}

func TestAccountWithdrawalRevokesAppleTokenBeforeDeletingUser(t *testing.T) {
	cipher := &ProviderTokenCipher{key: []byte("0123456789abcdef0123456789abcdef")}
	ciphertext, version, err := cipher.Encrypt("apple-refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	store := &stubWithdrawalStore{credential: &appleRevocationCredential{Ciphertext: ciphertext, Version: version}}
	revoker := &stubAppleRevoker{}
	var closed uuid.UUID
	service, err := NewAccountWithdrawalService(store, cipher, revoker, func(userID uuid.UUID) { closed = userID })
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.New()
	if err := service.Withdraw(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	if revoker.seen != "apple-refresh-token" || store.marked != userID || closed != userID {
		t.Fatalf("withdrawal calls: revoked=%q marked=%s closed=%s", revoker.seen, store.marked, closed)
	}
}

func TestAccountWithdrawalDoesNotDeleteWhenAppleRevokeFails(t *testing.T) {
	cipher := &ProviderTokenCipher{key: []byte("0123456789abcdef0123456789abcdef")}
	ciphertext, version, err := cipher.Encrypt("apple-refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	store := &stubWithdrawalStore{credential: &appleRevocationCredential{Ciphertext: ciphertext, Version: version}}
	service, err := NewAccountWithdrawalService(store, cipher, &stubAppleRevoker{err: errors.New("Apple unavailable")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Withdraw(context.Background(), uuid.New()); err == nil {
		t.Fatal("withdrawal unexpectedly succeeded")
	}
	if store.marked != uuid.Nil {
		t.Fatalf("user was marked deleted after revoke failure: %s", store.marked)
	}
}

func TestAccountWithdrawalSupportsNonAppleUser(t *testing.T) {
	store := &stubWithdrawalStore{}
	service, err := NewAccountWithdrawalService(store, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.New()
	if err := service.Withdraw(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	if store.marked != userID {
		t.Fatalf("marked user = %s, want %s", store.marked, userID)
	}
}

func TestAccountWithdrawalClosesOperationGateAfterCommit(t *testing.T) {
	store := &stubWithdrawalStore{}
	service, err := NewAccountWithdrawalService(store, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.New()
	if err := service.Withdraw(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	if _, admitted := service.BeginOperation(userID); admitted {
		t.Fatal("operation admitted after withdrawal committed")
	}
}

func TestAccountWithdrawalRetriesAfterPostRevokeFailure(t *testing.T) {
	cipher := &ProviderTokenCipher{key: []byte("0123456789abcdef0123456789abcdef")}
	ciphertext, version, err := cipher.Encrypt("apple-refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	store := &stubWithdrawalStore{credential: &appleRevocationCredential{Ciphertext: ciphertext, Version: version}}
	revoker := &stubAppleRevoker{}
	clearCalls := 0
	service, err := NewAccountWithdrawalService(store, cipher, revoker, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.SetCleanup(WithdrawalCleanup{
		ClearReferenceData: func(context.Context, uuid.UUID) error {
			clearCalls++
			if clearCalls == 1 {
				return errors.New("AI worker unavailable")
			}
			return nil
		},
	})
	userID := uuid.New()
	if err := service.Withdraw(context.Background(), userID); err == nil {
		t.Fatal("first withdrawal unexpectedly succeeded")
	}
	if store.marked != uuid.Nil {
		t.Fatal("user was marked deleted after post-revoke cleanup failure")
	}
	if revoker.seen != "apple-refresh-token" {
		t.Fatalf("Apple revoke token = %q", revoker.seen)
	}
	if err := service.Withdraw(context.Background(), userID); err != nil {
		t.Fatalf("retry withdrawal failed: %v", err)
	}
	if clearCalls != 2 {
		t.Fatalf("reference cleanup calls = %d, want 2", clearCalls)
	}
	if store.marked != userID {
		t.Fatalf("marked user = %s, want %s", store.marked, userID)
	}
}

func TestAccountWithdrawalHTTPDeletesAuthenticatedUser(t *testing.T) {
	store := &stubWithdrawalStore{}
	withdrawal, err := NewAccountWithdrawalService(store, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tokens := testTokenService(newMemoryRefreshStore())
	userID := uuid.New()
	pair, err := tokens.IssuePair(context.Background(), userID, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	config, err := NewTokenHTTPConfig(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := MountAuthHTTPWithWithdrawal(http.NotFoundHandler(), tokens, nil, nil, withdrawal, slog.New(slog.NewTextHandler(io.Discard, nil)), config)
	request := httptest.NewRequest(http.MethodDelete, "/auth/me", nil)
	request.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || store.marked != userID {
		t.Fatalf("status=%d marked=%s want=%s", response.Code, store.marked, userID)
	}
}

func TestAccountWithdrawalHTTPIsIdempotentAfterUserDeletion(t *testing.T) {
	store := &stubWithdrawalStore{}
	withdrawal, err := NewAccountWithdrawalService(store, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tokens := testTokenService(newMemoryRefreshStore())
	userID := uuid.New()
	pair, err := tokens.IssuePair(context.Background(), userID, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	config, err := NewTokenHTTPConfig(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := MountAuthHTTPWithWithdrawal(http.NotFoundHandler(), tokens, nil, nil, withdrawal, slog.New(slog.NewTextHandler(io.Discard, nil)), config)
	request := httptest.NewRequest(http.MethodDelete, "/auth/me", nil)
	request.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("initial status=%d, want %d", response.Code, http.StatusNoContent)
	}

	cleanupCalls := 0
	restarted, err := NewAccountWithdrawalService(store, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	restarted.SetCleanup(WithdrawalCleanup{
		ClearReferenceData: func(context.Context, uuid.UUID) error {
			cleanupCalls++
			return errors.New("must not run for deleted user")
		},
	})
	handler = MountAuthHTTPWithWithdrawal(http.NotFoundHandler(), tokens, nil, nil, restarted, slog.New(slog.NewTextHandler(io.Discard, nil)), config)
	request = httptest.NewRequest(http.MethodDelete, "/auth/me", nil)
	request.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || cleanupCalls != 0 {
		t.Fatalf("retry status=%d cleanup calls=%d, want 204 and 0", response.Code, cleanupCalls)
	}
}

func TestAccountWithdrawalHTTPDoesNotTreatInactiveUserAsDeleted(t *testing.T) {
	store := &stubWithdrawalStore{userStatus: UserStatusDisabled}
	handler, accessToken := withdrawalHTTPFixture(t, store, slog.New(slog.NewTextHandler(io.Discard, nil)))

	request := httptest.NewRequest(http.MethodDelete, "/auth/me", nil)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized || store.marked != uuid.Nil {
		t.Fatalf("status=%d marked=%s, want 401 and no deletion", response.Code, store.marked)
	}
	if !bytes.Contains(response.Body.Bytes(), []byte(`"code":"unauthorized"`)) {
		t.Fatalf("body=%s, want unauthorized code", response.Body.String())
	}
}

func TestAccountWithdrawalHTTPReportsStateLookupFailure(t *testing.T) {
	store := &stubWithdrawalStore{stateErr: errors.New("database unavailable")}
	var logs bytes.Buffer
	handler, accessToken := withdrawalHTTPFixture(t, store, slog.New(slog.NewJSONHandler(&logs, nil)))

	request := httptest.NewRequest(http.MethodDelete, "/auth/me", nil)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if !bytes.Contains(response.Body.Bytes(), []byte(`"code":"withdrawal_unavailable"`)) {
		t.Fatalf("body=%s, want withdrawal_unavailable code", response.Body.String())
	}
	if !bytes.Contains(logs.Bytes(), []byte("account withdrawal state lookup failed")) ||
		!bytes.Contains(logs.Bytes(), []byte("database unavailable")) {
		t.Fatalf("missing lookup failure log: %s", logs.String())
	}
}

func TestAccountWithdrawalHTTPAcceptsExpiredTokenOnlyAfterDeletion(t *testing.T) {
	store := &stubWithdrawalStore{}
	withdrawal, err := NewAccountWithdrawalService(store, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tokens := testTokenService(newMemoryRefreshStore())
	userID := uuid.New()
	pair, err := tokens.IssuePair(context.Background(), userID, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	config, err := NewTokenHTTPConfig(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := MountAuthHTTPWithWithdrawal(http.NotFoundHandler(), tokens, nil, nil, withdrawal, slog.New(slog.NewTextHandler(io.Discard, nil)), config)

	request := httptest.NewRequest(http.MethodDelete, "/auth/me", nil)
	request.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("initial status=%d, want %d", response.Code, http.StatusNoContent)
	}

	expiredToken, err := tokens.issueAccessToken(
		userID,
		uuid.New(),
		time.Now().UTC().Add(-tokens.cfg.AccessTTL-time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodDelete, "/auth/me", nil)
	request.Header.Set("Authorization", "Bearer "+expiredToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("retry status=%d, want %d", response.Code, http.StatusNoContent)
	}

	activeUserToken, err := tokens.issueAccessToken(
		uuid.New(),
		uuid.New(),
		time.Now().UTC().Add(-tokens.cfg.AccessTTL-time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodDelete, "/auth/me", nil)
	request.Header.Set("Authorization", "Bearer "+activeUserToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("active user status=%d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestAccountWithdrawalHTTPRejectsFutureTokenForDeletedUser(t *testing.T) {
	store := &stubWithdrawalStore{missing: true}
	withdrawal, err := NewAccountWithdrawalService(store, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tokens := testTokenService(newMemoryRefreshStore())
	userID := uuid.New()
	futureToken, err := tokens.issueAccessToken(
		userID,
		uuid.New(),
		time.Now().UTC().Add(time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	config, err := NewTokenHTTPConfig(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := MountAuthHTTPWithWithdrawal(http.NotFoundHandler(), tokens, nil, nil, withdrawal, slog.New(slog.NewTextHandler(io.Discard, nil)), config)

	request := httptest.NewRequest(http.MethodDelete, "/auth/me", nil)
	request.Header.Set("Authorization", "Bearer "+futureToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d", response.Code, http.StatusUnauthorized)
	}
	if !bytes.Contains(response.Body.Bytes(), []byte(`"code":"unauthorized"`)) {
		t.Fatalf("body=%s, want unauthorized code", response.Body.String())
	}
}

func withdrawalHTTPFixture(t *testing.T, store *stubWithdrawalStore, logger *slog.Logger) (http.Handler, string) {
	t.Helper()
	withdrawal, err := NewAccountWithdrawalService(store, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tokens := testTokenService(newMemoryRefreshStore())
	pair, err := tokens.IssuePair(context.Background(), uuid.New(), ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	config, err := NewTokenHTTPConfig(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return MountAuthHTTPWithWithdrawal(http.NotFoundHandler(), tokens, nil, nil, withdrawal, logger, config), pair.AccessToken
}
