package server

import (
	"net"
	"testing"
)

func TestGuestQueueClientIPUsesNearestUntrustedForwardedAddress(t *testing.T) {
	_, proxyNetwork, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	queue := &GuestQueue{trustedProxies: []*net.IPNet{proxyNetwork}}

	// The client prepended a spoofed value, then the trusted proxy appended
	// the real client address it observed. The rightmost untrusted value wins.
	got := queue.clientIP("10.0.0.10:443", "198.51.100.99, 203.0.113.25")
	if got != "203.0.113.25" {
		t.Fatalf("client IP = %q, want real nearest address", got)
	}

	// Intermediate trusted proxies are not considered clients either.
	got = queue.clientIP("10.0.0.10:443", "203.0.113.25, 10.0.0.20")
	if got != "203.0.113.25" {
		t.Fatalf("client IP through trusted proxy chain = %q", got)
	}
}

func TestGuestQueueClientIPIgnoresForwardedHeaderFromUntrustedPeer(t *testing.T) {
	_, proxyNetwork, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	queue := &GuestQueue{trustedProxies: []*net.IPNet{proxyNetwork}}

	got := queue.clientIP("203.0.113.25:443", "198.51.100.99")
	if got != "203.0.113.25" {
		t.Fatalf("client IP = %q, want direct peer", got)
	}
}
