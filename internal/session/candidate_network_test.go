package session

import (
	"strings"
	"testing"
)

func TestCandidateNetworkSeparatesVersionsAndNetworks(t *testing.T) {
	v4, hashA := candidateNetwork("203.0.113.7")
	if v4 != "v4" || len(hashA) != 8 {
		t.Fatalf("v4 = %q %q", v4, hashA)
	}
	if _, hashB := candidateNetwork("203.0.113.8"); hashB == hashA {
		t.Fatal("different IPv4 addresses share a hash")
	}
	if strings.Contains(hashA, "203") {
		t.Fatalf("hash leaks address: %q", hashA)
	}

	// 같은 /64의 다른 기기 주소는 같은 망으로 본다.
	v6, hashC := candidateNetwork("2001:db8:1:2::10")
	_, hashD := candidateNetwork("2001:db8:1:2:abcd::99")
	_, hashE := candidateNetwork("2001:db8:1:3::10")
	if v6 != "v6" || hashC != hashD || hashC == hashE {
		t.Fatalf("v6 = %q, same /64 %q/%q, other /64 %q", v6, hashC, hashD, hashE)
	}

	// IPv4-mapped IPv6는 IPv4로 본다.
	if version, hash := candidateNetwork("::ffff:203.0.113.7"); version != "v4" || hash != hashA {
		t.Fatalf("mapped = %q %q, want v4 %q", version, hash, hashA)
	}
}

func TestCandidateNetworkIgnoresNonAddresses(t *testing.T) {
	for _, address := range []string{"", "a1b2c3.local"} {
		if version, hash := candidateNetwork(address); version != "" || hash != "" {
			t.Fatalf("candidateNetwork(%q) = %q %q", address, version, hash)
		}
	}
}

// 로그용 요약은 candidate 종류·프로토콜·버전·망 해시만 남기고 주소를 담지 않는다(#345).
func TestCandidateSummaryDropsAddresses(t *testing.T) {
	relay := "candidate:2248413281 1 udp 58401279 203.0.113.7 50004 typ relay raddr 198.51.100.9 rport 46783 generation 0 ufrag q2om"
	candidateType, protocol, version, network := candidateSummary(relay)
	_, want := candidateNetwork("203.0.113.7")
	if candidateType != "relay" || protocol != "udp" || version != "v4" || network != want {
		t.Fatalf("summary = %q %q %q %q", candidateType, protocol, version, network)
	}
	for _, value := range []string{candidateType, protocol, version, network} {
		if strings.Contains(value, "203.0.113") || strings.Contains(value, "198.51.100") {
			t.Fatalf("summary leaks an address: %q", value)
		}
	}

	if _, _, version, network := candidateSummary("a=candidate:1 1 UDP 2122 0f1e2d3c-aaaa-bbbb-cccc-111122223333.local 5000 typ host"); version != "mdns" || network != "" {
		t.Fatalf("mdns = %q %q", version, network)
	}
	// 형식이 다르면 아무것도 남기지 않는다 — 원문 일부라도 흘리지 않는다.
	if candidateType, protocol, version, network := candidateSummary("garbage 203.0.113.7"); candidateType+protocol+version+network != "" {
		t.Fatalf("malformed = %q %q %q %q", candidateType, protocol, version, network)
	}
}
