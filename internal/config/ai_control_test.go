package config

import (
	"strings"
	"testing"
)

// 워커 교체 제어 API는 같은 호스트의 배포 스크립트만 부른다(#515).
func TestValidateAIControlAddrRequiresLoopbackAndToken(t *testing.T) {
	token := strings.Repeat("t", 32)
	for _, tc := range []struct {
		name  string
		addr  string
		token string
		ok    bool
	}{
		{"disabled", "", "", true},
		{"ipv4 loopback", "127.0.0.1:9091", token, true},
		{"ipv6 loopback", "[::1]:9091", token, true},
		{"all interfaces", "0.0.0.0:9091", token, false},
		{"public address", "10.0.0.5:9091", token, false},
		{"host name", "localhost:9091", token, false},
		{"missing port", "127.0.0.1", token, false},
		{"short token", "127.0.0.1:9091", "short", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.AIControlAddr, cfg.AIControlToken = tc.addr, tc.token
			if err := cfg.Validate(); (err == nil) != tc.ok {
				t.Fatalf("Validate() error = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
