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
