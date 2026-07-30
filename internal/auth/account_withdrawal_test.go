package auth

import (
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
}

func (s *stubWithdrawalStore) AppleRevocationCredential(_ context.Context, _ uuid.UUID) (*appleRevocationCredential, error) {
	return s.credential, s.credentialErr
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
