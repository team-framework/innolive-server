// Package clientip는 요청의 클라이언트 IP를 한 가지 규칙으로 판정한다. 직접 연결한
// 상대가 신뢰 프록시 대역일 때만 X-Forwarded-For를 읽는다(P1-17). 인증·게스트 큐·
// 시그널링이 같은 판정을 써야 한 경로만 프록시 주소 하나로 모든 사용자를 묶거나
// spoof된 헤더를 믿는 일이 없다.
package clientip

import (
	"context"
	"net"
	"net/http"
	"strings"
)

// Resolver는 신뢰 프록시 대역을 들고 클라이언트 IP를 판정한다. 대역이 비어 있으면
// X-Forwarded-For를 쓰지 않고 TCP peer 주소만 쓴다.
type Resolver struct {
	trusted []*net.IPNet
}

// NewResolver는 CIDR 목록으로 Resolver를 만든다. 형식이 틀린 항목은 config 검증이
// 기동 전에 거절하므로 여기서는 건너뛴다.
func NewResolver(cidrs []string) Resolver {
	trusted := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, block, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		trusted = append(trusted, block)
	}
	return Resolver{trusted: trusted}
}

// IP는 remoteAddr와 X-Forwarded-For 값으로 클라이언트 IP를 고른다.
func (r Resolver) IP(remoteAddr, forwardedFor string) string {
	host := peerHost(remoteAddr)
	remote := net.ParseIP(host)
	if remote == nil || !r.trustedIP(remote) {
		return host
	}

	// 신뢰된 proxy는 기존 XFF 체인 뒤에 자신이 관찰한 주소를 붙인다. 가장 가까운
	// 주소부터 클라이언트 방향으로 순회하며 신뢰된 proxy 대역만 건너뛴다. 왼쪽 첫
	// 값을 읽으면 클라이언트가 임의의 spoofed 주소를 앞에 넣어 우회할 수 있다.
	parts := strings.Split(forwardedFor, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := net.ParseIP(strings.TrimSpace(parts[i]))
		if candidate != nil && !r.trustedIP(candidate) {
			return candidate.String()
		}
	}
	return host
}

// RemoteTrusted는 요청의 RemoteAddr(직접 연결한 상대)가 신뢰 프록시 대역에
// 속하는지 판단한다. per-IP 상한을 적용해도 되는지 가리는 데 쓴다.
func (r Resolver) RemoteTrusted(remoteAddr string) bool {
	ip := net.ParseIP(peerHost(remoteAddr))
	return ip != nil && r.trustedIP(ip)
}

func (r Resolver) trustedIP(ip net.IP) bool {
	for _, proxy := range r.trusted {
		if proxy != nil && proxy.Contains(ip) {
			return true
		}
	}
	return false
}

type contextKey struct{}

// Middleware는 판정한 클라이언트 IP를 요청 context에 싣는다. 핸들러는
// FromRequest로 읽는다.
func (r Resolver) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ip := r.IP(req.RemoteAddr, req.Header.Get("X-Forwarded-For"))
		next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), contextKey{}, ip)))
	})
}

// FromRequest는 Middleware가 실은 클라이언트 IP를 돌려준다. Middleware를 거치지
// 않은 요청(테스트·직접 조립)은 TCP peer 주소를 쓴다.
func FromRequest(req *http.Request) string {
	if ip, ok := req.Context().Value(contextKey{}).(string); ok {
		return ip
	}
	return peerHost(req.RemoteAddr)
}

func peerHost(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
