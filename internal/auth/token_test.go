package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type memoryRefreshStore struct {
	mu           sync.Mutex
	records      map[[sha256.Size]byte]refreshSessionRecord
	userStatuses map[uuid.UUID]UserStatus
}

func newMemoryRefreshStore() *memoryRefreshStore {
	return &memoryRefreshStore{
		records:      make(map[[sha256.Size]byte]refreshSessionRecord),
		userStatuses: make(map[uuid.UUID]UserStatus),
	}
}

func (s *memoryRefreshStore) userStatus(userID uuid.UUID) UserStatus {
	status, ok := s.userStatuses[userID]
	if !ok {
		return UserStatusActive
	}
	return status
}

func (s *memoryRefreshStore) setUserStatus(userID uuid.UUID, status UserStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userStatuses[userID] = status
}

func (s *memoryRefreshStore) Create(_ context.Context, record refreshSessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.userStatus(record.UserID) != UserStatusActive {
		return ErrUserInactive
	}
	s.records[hashKey(record.TokenHash)] = record
	return nil
}

func (s *memoryRefreshStore) Rotate(_ context.Context, hash []byte, now time.Time, next refreshRotation) (refreshSessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := hashKey(hash)
	current, ok := s.records[key]
	if !ok {
		return refreshSessionRecord{}, ErrInvalidRefreshToken
	}
	if current.ReplacedByID != nil {
		copyNow := now
		current.LastUsedAt = &copyNow
		s.records[key] = current
		s.revokeFamily(current.FamilyID, now, revokeReasonReuseDetected)
		return refreshSessionRecord{}, ErrRefreshTokenReused
	}
	if current.RevokedAt != nil {
		return refreshSessionRecord{}, ErrRefreshTokenRevoked
	}
	if !now.Before(current.ExpiresAt) {
		s.revokeRecord(key, current, now, revokeReasonExpired, true)
		return refreshSessionRecord{}, ErrRefreshTokenExpired
	}
	if s.userStatus(current.UserID) != UserStatusActive {
		copyNow := now
		current.LastUsedAt = &copyNow
		s.records[key] = current
		s.revokeFamily(current.FamilyID, now, revokeReasonUserInactive)
		return refreshSessionRecord{}, ErrUserInactive
	}

	familyStartedAt := current.CreatedAt
	for _, candidate := range s.records {
		if candidate.FamilyID == current.FamilyID && candidate.CreatedAt.Before(familyStartedAt) {
			familyStartedAt = candidate.CreatedAt
		}
	}
	nextExpiresAt := earlierTime(next.IdleExpiresAt, familyStartedAt.Add(next.AbsoluteTTL))
	if !now.Before(nextExpiresAt) {
		s.revokeRecord(key, current, now, revokeReasonExpired, true)
		return refreshSessionRecord{}, ErrRefreshTokenExpired
	}

	nextRecord := refreshSessionRecord{
		ID:        next.ID,
		UserID:    current.UserID,
		FamilyID:  current.FamilyID,
		TokenHash: next.TokenHash,
		UserAgent: tokenOptionalString(next.Client.UserAgent),
		IPAddress: tokenOptionalString(next.Client.IPAddress),
		ExpiresAt: nextExpiresAt,
		CreatedAt: now,
	}
	s.records[hashKey(next.TokenHash)] = nextRecord

	copyNow := now
	reason := revokeReasonRotated
	current.LastUsedAt = &copyNow
	current.RevokedAt = &copyNow
	current.RevokeReason = &reason
	current.ReplacedByID = &next.ID
	s.records[key] = current
	return nextRecord, nil
}

func (s *memoryRefreshStore) RevokeFamilyByHash(_ context.Context, hash []byte, now time.Time) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := hashKey(hash)
	current, ok := s.records[key]
	if !ok {
		return uuid.Nil, ErrInvalidRefreshToken
	}
	if current.RevokedAt != nil {
		return uuid.Nil, ErrRefreshTokenRevoked
	}
	if !now.Before(current.ExpiresAt) {
		return uuid.Nil, ErrRefreshTokenExpired
	}
	copyNow := now
	current.LastUsedAt = &copyNow
	s.records[key] = current
	s.revokeFamily(current.FamilyID, now, revokeReasonLogout)
	return current.UserID, nil
}

func (s *memoryRefreshStore) revokeRecord(
	key [sha256.Size]byte,
	record refreshSessionRecord,
	now time.Time,
	reasonValue string,
	touch bool,
) {
	copyNow := now
	if touch {
		record.LastUsedAt = &copyNow
	}
	if record.RevokedAt == nil {
		record.RevokedAt = &copyNow
		reason := reasonValue
		record.RevokeReason = &reason
	}
	s.records[key] = record
}

func (s *memoryRefreshStore) revokeFamily(familyID uuid.UUID, now time.Time, reasonValue string) {
	for key, candidate := range s.records {
		if candidate.FamilyID != familyID || candidate.RevokedAt != nil {
			continue
		}
		copyNow := now
		reason := reasonValue
		candidate.RevokedAt = &copyNow
		candidate.RevokeReason = &reason
		s.records[key] = candidate
	}
}

func (s *memoryRefreshStore) recordByRawToken(t *testing.T, raw string) refreshSessionRecord {
	t.Helper()
	hash, err := hashRefreshToken(raw)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[hashKey(hash)]
	if !ok {
		t.Fatal("refresh session record not found")
	}
	return record
}

func hashKey(value []byte) [sha256.Size]byte {
	var key [sha256.Size]byte
	copy(key[:], value)
	return key
}

func testTokenService(store refreshStore) *TokenService {
	return newTokenService(store, TokenConfig{
		AccessKey:          []byte("0123456789abcdef0123456789abcdef"),
		AccessTTL:          15 * time.Minute,
		RefreshTTL:         30 * 24 * time.Hour,
		RefreshAbsoluteTTL: 90 * 24 * time.Hour,
		Issuer:             "innolive-server",
		Audience:           "innolive-api",
		ClockSkew:          0,
	})
}

func TestAccessTokenRoundTrip(t *testing.T) {
	service := testTokenService(newMemoryRefreshStore())
	userID := uuid.New()
	pair, err := service.IssuePair(context.Background(), userID, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := service.ValidateAccessToken(pair.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != userID.String() {
		t.Fatalf("subject = %q, want %q", claims.Subject, userID)
	}
	if claims.TokenType != "access" {
		t.Fatalf("token_type = %q", claims.TokenType)
	}
	if _, err := uuid.Parse(claims.SessionID); err != nil {
		t.Fatalf("session ID is not UUID: %v", err)
	}
	if _, err := uuid.Parse(claims.ID); err != nil {
		t.Fatalf("JWT ID is not UUID: %v", err)
	}
}

func TestAccessTokenRejectsInvalidJWTID(t *testing.T) {
	service := testTokenService(newMemoryRefreshStore())
	now := time.Now().UTC()
	claims := AccessClaims{
		SessionID: uuid.NewString(),
		TokenType: "access",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    service.cfg.Issuer,
			Subject:   uuid.NewString(),
			Audience:  jwt.ClaimStrings{service.cfg.Audience},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
			NotBefore: jwt.NewNumericDate(now),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        "not-a-uuid",
		},
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(service.cfg.AccessKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ValidateAccessToken(raw); !errors.Is(err, ErrInvalidAccessToken) {
		t.Fatalf("error = %v, want ErrInvalidAccessToken", err)
	}
}

func TestRefreshRotationAndReuseDetection(t *testing.T) {
	store := newMemoryRefreshStore()
	service := testTokenService(store)
	userID := uuid.New()
	first, err := service.IssuePair(context.Background(), userID, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Rotate(context.Background(), first.RefreshToken, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh token was not rotated")
	}
	firstRecord := store.recordByRawToken(t, first.RefreshToken)
	if firstRecord.RevokeReason == nil || *firstRecord.RevokeReason != revokeReasonRotated {
		t.Fatalf("first revoke reason = %v", firstRecord.RevokeReason)
	}
	if firstRecord.LastUsedAt == nil {
		t.Fatal("first token last_used_at was not recorded")
	}

	if _, err := service.Rotate(context.Background(), first.RefreshToken, ClientInfo{}); !errors.Is(err, ErrRefreshTokenReused) {
		t.Fatalf("reuse error = %v, want ErrRefreshTokenReused", err)
	}
	if _, err := service.Rotate(context.Background(), second.RefreshToken, ClientInfo{}); !errors.Is(err, ErrRefreshTokenRevoked) {
		t.Fatalf("family token error = %v, want ErrRefreshTokenRevoked", err)
	}
	secondRecord := store.recordByRawToken(t, second.RefreshToken)
	if secondRecord.RevokeReason == nil || *secondRecord.RevokeReason != revokeReasonReuseDetected {
		t.Fatalf("second revoke reason = %v", secondRecord.RevokeReason)
	}
}

func TestRefreshAbsoluteExpiryDoesNotSlide(t *testing.T) {
	store := newMemoryRefreshStore()
	service := testTokenService(store)
	service.cfg.RefreshTTL = 24 * time.Hour
	service.cfg.RefreshAbsoluteTTL = 48 * time.Hour
	startedAt := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return startedAt }

	first, err := service.IssuePair(context.Background(), uuid.New(), ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return startedAt.Add(20 * time.Hour) }
	second, err := service.Rotate(context.Background(), first.RefreshToken, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return startedAt.Add(40 * time.Hour) }
	third, err := service.Rotate(context.Background(), second.RefreshToken, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if third.RefreshExpiresIn != int64((8*time.Hour)/time.Second) {
		t.Fatalf("refresh_expires_in = %d, want %d", third.RefreshExpiresIn, int64((8*time.Hour)/time.Second))
	}
	service.now = func() time.Time { return startedAt.Add(49 * time.Hour) }
	if _, err := service.Rotate(context.Background(), third.RefreshToken, ClientInfo{}); !errors.Is(err, ErrRefreshTokenExpired) {
		t.Fatalf("error = %v, want ErrRefreshTokenExpired", err)
	}
}

func TestInactiveUserCannotIssueOrRefresh(t *testing.T) {
	store := newMemoryRefreshStore()
	service := testTokenService(store)
	userID := uuid.New()

	store.setUserStatus(userID, UserStatusDisabled)
	if _, err := service.IssuePair(context.Background(), userID, ClientInfo{}); !errors.Is(err, ErrUserInactive) {
		t.Fatalf("issue error = %v, want ErrUserInactive", err)
	}

	store.setUserStatus(userID, UserStatusActive)
	pair, err := service.IssuePair(context.Background(), userID, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	store.setUserStatus(userID, UserStatusDisabled)
	if _, err := service.Rotate(context.Background(), pair.RefreshToken, ClientInfo{}); !errors.Is(err, ErrUserInactive) {
		t.Fatalf("rotate error = %v, want ErrUserInactive", err)
	}
	record := store.recordByRawToken(t, pair.RefreshToken)
	if record.RevokeReason == nil || *record.RevokeReason != revokeReasonUserInactive {
		t.Fatalf("revoke reason = %v", record.RevokeReason)
	}
}

func TestLogoutRevokesRefreshFamily(t *testing.T) {
	store := newMemoryRefreshStore()
	service := testTokenService(store)
	userID := uuid.New()
	pair, err := service.IssuePair(context.Background(), userID, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	loggedOutUserID, err := service.LogoutUser(context.Background(), pair.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if loggedOutUserID != userID {
		t.Fatalf("logout user ID = %s, want %s", loggedOutUserID, userID)
	}
	if _, err := service.Rotate(context.Background(), pair.RefreshToken, ClientInfo{}); !errors.Is(err, ErrRefreshTokenRevoked) {
		t.Fatalf("rotate error = %v, want ErrRefreshTokenRevoked", err)
	}
	record := store.recordByRawToken(t, pair.RefreshToken)
	if record.RevokeReason == nil || *record.RevokeReason != revokeReasonLogout {
		t.Fatalf("revoke reason = %v", record.RevokeReason)
	}
}

func TestMalformedRefreshTokenRejected(t *testing.T) {
	service := testTokenService(newMemoryRefreshStore())
	if _, err := service.Rotate(context.Background(), "not-a-token", ClientInfo{}); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("error = %v, want ErrInvalidRefreshToken", err)
	}
}
