package server

import (
	"net"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"
)

// Opt-in localhost-only backend for the attached Android media probe. No real auth
// token is needed and no public broadcast/provider is created.
func TestLocalDeviceMediaProbe(t *testing.T) {
	if os.Getenv("INNOLIVE_LOCAL_DEVICE_PROBE") != "1" {
		t.Skip("explicit device probe opt-in required")
	}
	ip := net.ParseIP(os.Getenv("INNOLIVE_LOCAL_ICE_IP"))
	if ip == nil || !ip.IsPrivate() {
		t.Fatal("private LAN ICE IP required")
	}
	cfg := testServerConfig()
	cfg.AnnouncedIP = ip.String()
	cfg.RequireSessionAuth = false
	cfg.UDPMuxPort = 42061
	cfg.DisconnectedGracePeriod = 10 * time.Second
	app, manager := newTestApplicationWithConfig(t, cfg, nil)
	defer manager.CloseAll()
	listener, err := net.Listen("tcp", "127.0.0.1:18644")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewUnstartedServer(app.Handler())
	httpServer.Listener.Close()
	httpServer.Listener = listener
	httpServer.Start()
	defer httpServer.Close()
	t.Log("LOCAL_DEVICE_BACKEND_READY: localhost HTTP and private-LAN encrypted ICE only")
	deadline := time.Now().Add(12 * time.Minute)
	stop := os.Getenv("INNOLIVE_LOCAL_PROBE_STOP_FILE")
	for time.Now().Before(deadline) {
		if stop != "" {
			if _, err := os.Stat(stop); err == nil {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatal("device probe deadline exceeded: " + strconv.Itoa(12) + " minutes")
}
