package webui

import (
	"runtime"
	"testing"
	"time"
)

// BH-GC2-16: Server.Close left this server's Broadcaster running (worker and beacon goroutines,
// sockets) and its entries in the package maps.
func TestServerCloseStopsItsBroadcaster(t *testing.T) {
	before := runtime.NumGoroutine()
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	if err := s.SetBroadcastEnabled(true); err != nil {
		t.Fatal(err)
	}
	s.usbAudioManager()
	s.RegisterBroadcastRoutes()
	if runtime.NumGoroutine() <= before {
		t.Fatal("the broadcaster started no goroutine; the test cannot see a leak")
	}
	s.Close()
	for _, m := range []interface{ Load(any) (any, bool) }{&broadcasters, &usbAudioManagers, &broadcastRoutesRegistered} {
		if _, ok := m.Load(s); ok {
			t.Fatal("a closed server is still in a package map")
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		buf := make([]byte, 1<<16)
		t.Fatalf("%d goroutines after Close, %d before:\n%s", n, before, buf[:runtime.Stack(buf, true)])
	}
}
