package session

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// ErrUnauthorized is returned when a caller presents an owner token that does
// not match the session it targets. Callers map this to HTTP 403 (the session
// exists but the token is wrong) while ErrNotFound maps to 404.
var ErrUnauthorized = errUnauthorized{}

type errUnauthorized struct{}

func (errUnauthorized) Error() string { return "session owner token mismatch" }

// newOwnerToken mints a 256-bit random owner token and its SHA-256 digest. The
// plaintext is returned to the creator exactly once; only the digest is kept on
// the Session, so a memory/log leak of server state cannot reveal the token.
func newOwnerToken() (token string, digest [32]byte, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", [32]byte{}, err
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	digest = sha256.Sum256([]byte(token))
	return token, digest, nil
}

// verifyOwnerToken reports whether presented matches the session's stored
// digest, using a constant-time comparison to avoid leaking the digest through
// timing.
func (s *Session) verifyOwnerToken(presented string) bool {
	got := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(got[:], s.ownerHash[:]) == 1
}
