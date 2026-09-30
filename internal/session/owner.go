package session

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// ErrUnauthorized는 호출자가 내민 소유자 토큰이 대상 세션과 맞지 않을 때 돌려준다.
// 호출자는 이를 HTTP 403(세션은 있지만 토큰이 틀림)으로, ErrNotFound는 404로 옮긴다.
var ErrUnauthorized = errUnauthorized{}

type errUnauthorized struct{}

func (errUnauthorized) Error() string { return "session owner token mismatch" }

// newOwnerToken은 256비트 무작위 소유자 토큰과 그 SHA-256 다이제스트를 만든다. 평문은
// 만든 쪽에 정확히 한 번 돌려주고 Session에는 다이제스트만 두므로, 서버 상태가
// 메모리·로그로 새어도 토큰은 드러나지 않는다.
func newOwnerToken() (token string, digest [32]byte, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", [32]byte{}, err
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	digest = sha256.Sum256([]byte(token))
	return token, digest, nil
}

// verifyOwnerToken은 내민 값이 세션에 저장한 다이제스트와 맞는지다. 타이밍으로
// 다이제스트가 새지 않게 상수 시간 비교를 쓴다.
func (s *Session) verifyOwnerToken(presented string) bool {
	got := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(got[:], s.ownerHash[:]) == 1
}
