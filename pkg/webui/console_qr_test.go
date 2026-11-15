package webui

import (
	"bytes"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

// #218: the console QR code pointed at sc5000.lan (enginex.lan since 5.3.0.5, #234), which most
// routers do not resolve, so phones opened a blank page.
func TestConsoleQRPointsWherePhonesCanReach(t *testing.T) {
	lan := "http://" + lanHost() + ":8080"
	for host, want := range map[string]string{
		"192.168.5.34:8080":         "http://192.168.5.34:8080",
		"enginex.local:8080":        "http://enginex.local:8080",
		"127.0.0.1:8080":            lan,
		"localhost:8080":            lan,
		"enginex.lan:8080":          lan,
		"sc5000.lan:8080":           lan,
		"[::1]:8080":                lan,
		"abc-def.trycloudflare.com": "https://abc-def.trycloudflare.com",
	} {
		r := httptest.NewRequest("GET", "/api/console-qr", nil)
		r.Host = host
		if got := consoleURL(r); got != want {
			t.Errorf("Host %s: %s, want %s", host, got, want)
		}
	}
	r := httptest.NewRequest("GET", "/api/console-qr", nil)
	r.Host = "192.168.5.34:8080"
	for _, def := range []string{"http://enginex.lan:8080", "http://sc5000.lan:8080"} { // a saved one from before 5.3.0.5 too
		if got := (&Server{cfg: Config{PublicURL: def}}).qrTarget(r); got != "http://192.168.5.34:8080" {
			t.Errorf("default public URL %s: %s", def, got)
		}
	}
	if got := (&Server{cfg: Config{PublicURL: "https://deck.example.org"}}).qrTarget(r); got != "https://deck.example.org" {
		t.Errorf("configured public URL: %s", got)
	}
	if hintTitle("netease") == "" || lanHost() == "" {
		t.Fatal("empty hint")
	}
}

func TestConsoleQRTarget(t *testing.T) {
	lan := "http://" + lanHost() + ":8080"
	ui := New(Config{StateDir: t.TempDir()}, nil)

	t.Run("ForwardedProtoHTTPS", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/api/console-qr", nil)
		r.Host = "deck.local:8080"
		r.Header.Set("X-Forwarded-Proto", "https")
		if got := ui.qrTarget(r); got != "https://deck.local:8080" {
			t.Fatalf("target = %q, want https://deck.local:8080", got)
		}

		rMulti := httptest.NewRequest("GET", "/api/console-qr", nil)
		rMulti.Host = "deck.local:8080"
		rMulti.Header.Set("X-Forwarded-Proto", "https, http")
		if got := ui.qrTarget(rMulti); got != "https://deck.local:8080" {
			t.Fatalf("multi proto target = %q, want https://deck.local:8080", got)
		}

		r = httptest.NewRequest("GET", "/api/console-qr", nil)
		r.Host = "deck.example"
		if got := ui.qrTarget(r); got != "http://deck.example" {
			t.Fatalf("target = %q, want http://deck.example", got)
		}
	})

	t.Run("IPv6NonLoopbackHost", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/api/console-qr", nil)
		r.Host = "[2001:db8::1]:8080"
		if got := ui.qrTarget(r); got != "http://[2001:db8::1]:8080" {
			t.Fatalf("target = %q, want http://[2001:db8::1]:8080", got)
		}

		r = httptest.NewRequest("GET", "/api/console-qr", nil)
		r.Host = "[2001:db8::1]"
		if got := ui.qrTarget(r); got != "http://[2001:db8::1]" {
			t.Fatalf("target = %q, want http://[2001:db8::1]", got)
		}
	})

	t.Run("ProxiedLoopbackLANScheme", func(t *testing.T) {
		rProxy := httptest.NewRequest("GET", "/api/console-qr", nil)
		rProxy.Host = "localhost:8080"
		rProxy.Header.Set("X-Forwarded-Proto", "https")
		if got := ui.qrTarget(rProxy); got != lan {
			t.Fatalf("proxied loopback target = %q, want %s", got, lan)
		}
	})

	t.Run("DirectNonLoopbackIPForwardedHTTPS", func(t *testing.T) {
		rIPHTTPS := httptest.NewRequest("GET", "/api/console-qr", nil)
		rIPHTTPS.Host = "10.0.0.5:9090"
		rIPHTTPS.Header.Set("X-Forwarded-Proto", "https")
		if got := ui.qrTarget(rIPHTTPS); got != "https://10.0.0.5:9090" {
			t.Fatalf("ip https target = %q, want https://10.0.0.5:9090", got)
		}
	})

	t.Run("IPv6ZoneIdentifierRFC6874", func(t *testing.T) {
		rZoneHTTPS := httptest.NewRequest("GET", "/api/console-qr", nil)
		rZoneHTTPS.Host = "[fe80::1%eth0]:8080"
		rZoneHTTPS.Header.Set("X-Forwarded-Proto", "https")
		if got := ui.qrTarget(rZoneHTTPS); got != "https://[fe80::1%25eth0]:8080" {
			t.Fatalf("zone ipv6 https target = %q, want https://[fe80::1%%25eth0]:8080", got)
		}

		rZone25 := httptest.NewRequest("GET", "/api/console-qr", nil)
		rZone25.Host = "[fe80::1%25eth0]:8080"
		if got := ui.qrTarget(rZone25); got != "http://[fe80::1%2525eth0]:8080" {
			t.Fatalf("zone starting with 25 target = %q, want http://[fe80::1%%2525eth0]:8080", got)
		}
	})

	t.Run("DirectTLSConnection", func(t *testing.T) {
		rDirectTLS := httptest.NewRequest("GET", "/api/console-qr", nil)
		rDirectTLS.Host = "10.0.0.5:9090"
		rDirectTLS.TLS = &tls.ConnectionState{}
		if got := ui.qrTarget(rDirectTLS); got != "https://10.0.0.5:9090" {
			t.Fatalf("direct tls target = %q, want https://10.0.0.5:9090", got)
		}
	})

	t.Run("LoopbackPortForward", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/api/console-qr", nil)
		r.Host = "localhost:18080"
		if got := ui.qrTarget(r); got != lan {
			t.Fatalf("port forward target = %q, want %s", got, lan)
		}
	})

	t.Run("IPv6LoopbackWithoutPort", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/api/console-qr", nil)
		r.Host = "[::1]"
		if got := ui.qrTarget(r); got != lan {
			t.Fatalf("bracketed ipv6 loopback target = %q, want %s", got, lan)
		}
	})

	t.Run("EndpointPNG", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/console-qr", nil)
		r.AddCookie(&http.Cookie{Name: "stream_sync", Value: ui.AccessToken()})
		ui.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("console-qr status: %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "image/png" {
			t.Fatalf("content-type = %q, want image/png", ct)
		}
		if !bytes.HasPrefix(w.Body.Bytes(), []byte("\x89PNG")) {
			t.Fatalf("console-qr response is not a valid PNG")
		}
	})
}
