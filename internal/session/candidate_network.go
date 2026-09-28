package session

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
)

// candidateNetworkKey는 후보 주소를 해시할 때 쓰는 프로세스 수명 키다. 키 없는
// 해시는 IPv4 공간(2^32)을 전부 대입하면 주소가 복원되므로 키를 섞는다.
// 재시작하면 키가 바뀌어 배포 전후 해시는 비교할 수 없다(#304).
var candidateNetworkKey = func() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return key
}()

// candidateNetwork는 후보 주소의 IP 버전과 망 식별 해시를 돌려준다. 망이
// 바뀌었는지만 가리면 되므로 IPv6는 기기마다 바뀌는 하위 64비트를 버리고
// /64 프리픽스를 해시한다. 주소가 아니면(mDNS 이름 등) 빈 값이다.
func candidateNetwork(address string) (version, hash string) {
	addr, err := netip.ParseAddr(address)
	if err != nil {
		return "", ""
	}
	addr = addr.Unmap()
	version = "v6"
	if addr.Is4() {
		version = "v4"
	} else if prefix, err := addr.Prefix(64); err == nil {
		addr = prefix.Addr()
	}
	mac := hmac.New(sha256.New, candidateNetworkKey)
	mac.Write(addr.AsSlice())
	return version, hex.EncodeToString(mac.Sum(nil))[:8]
}
