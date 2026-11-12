package main

import (
	"net/http/httptest"
	"testing"
)

// A LAN client is limited by its own address: forwarding headers count only from a proxy on
// the same host or one listed with -trusted-proxies, and only when they carry an address.
func TestForwardedAddressOnlyFromTrustedProxies(t *testing.T) {
	saved := trustedProxies
	defer func() { trustedProxies = saved }()
	ip := func(remote, xff, real string) string {
		r := httptest.NewRequest("GET", "/api/config", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		if real != "" {
			r.Header.Set("X-Real-IP", real)
		}
		return clientIP(r)
	}
	trustedProxies = nil
	for _, c := range []struct{ remote, xff, real, want string }{
		{"192.168.50.2:12345", "198.51.100.7", "", "192.168.50.2"},
		{"10.1.2.3:12345", "", "198.51.100.7", "10.1.2.3"},
		{"100.64.1.2:12345", "198.51.100.7", "", "100.64.1.2"},
		{"[fd00::2]:12345", "198.51.100.7", "", "fd00::2"},
		{"127.0.0.1:12345", "203.0.113.1, 198.51.100.7", "", "198.51.100.7"},
		{"127.0.0.1:12345", "", "198.51.100.8", "198.51.100.8"},
		{"127.0.0.1:12345", "not-an-address", "", "127.0.0.1"},
		{"[::1]:12345", "", "x", "::1"},
	} {
		if got := ip(c.remote, c.xff, c.real); got != c.want {
			t.Errorf("default trust, %s with X-Forwarded-For %q X-Real-IP %q: %s, want %s", c.remote, c.xff, c.real, got, c.want)
		}
	}
	var err error
	if trustedProxies, err = parseTrustedProxies("10.0.0.0/8"); err != nil {
		t.Fatal(err)
	}
	if got := ip("10.1.2.3:12345", "198.51.100.7", ""); got != "198.51.100.7" {
		t.Errorf("listed proxy: %s", got)
	}
	if got := ip("192.168.50.2:12345", "198.51.100.7", ""); got != "192.168.50.2" {
		t.Errorf("unlisted LAN peer: %s", got)
	}
}

// A LAN client changing X-Forwarded-For on every guess still reaches the failure limit, and it
// cannot spend another client's failures by naming that client's address.
func TestLANClientCannotChooseItsLimiterKey(t *testing.T) {
	saved := trustedProxies
	trustedProxies = nil
	defer func() { trustedProxies = saved }()
	h := newTestServer(t).routes()
	call := func(remote, xff, pw string) int {
		r := httptest.NewRequest("GET", "/api/config", nil)
		r.RemoteAddr = remote
		r.Header.Set("X-Deemix-Password", pw)
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < failMax; i++ {
		call("192.168.50.2:12345", "198.51.100."+string(rune('1'+i%9)), "wrong")
	}
	if code := call("192.168.50.2:12345", "198.51.100.200", defaultPassword); code != 429 {
		t.Fatalf("after %d failures with a new X-Forwarded-For each: %d, want 429", failMax, code)
	}
	for i := 0; i < failMax; i++ {
		call("192.168.50.3:12345", "192.168.50.4", "wrong")
	}
	if code := call("192.168.50.4:12345", "", defaultPassword); code != 200 {
		t.Fatalf("the named client was locked out: %d", code)
	}
}
