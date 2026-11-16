package webui

import (
"github.com/Cd1s/EngineX-OS/internal/migration"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skip2/go-qrcode"
)

// #218: the console QR code pointed at legacy.lan (enginex.lan since 5.3.0.5, #234), which most
// routers do not resolve, so phones opened a blank page.
func TestConsoleQRPointsWherePhonesCanReach(t *testing.T) {
	lan := "http://" + lanHost() + ":8080"
	for host, want := range map[string]string{
		"192.168.5.34:8080":         "http://192.168.5.34:8080",
		"enginex.local:8080":        "http://enginex.local:8080",
		"127.0.0.1:8080":            lan,
		"localhost:8080":            lan,
		"enginex.lan:8080":          lan,
		migration.PlaceholderHost + ":8080":           lan,
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
	for _, def := range []string{"http://enginex.lan:8080", "http://" + migration.PlaceholderHost + ":8080"} { // a saved one from before 5.3.0.5 too
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

func TestConsoleQRAndTunnelPrecedence(t *testing.T) {
	stateDir := t.TempDir()
	uiDir := t.TempDir()
	certDir := t.TempDir()
	binDir := t.TempDir()

	fakeBin := filepath.Join(binDir, "cloudflared")
	fakeScript := "#!/bin/sh\n" +
		"echo \"2026-11-15T00:00:00Z INF https://remote-deck-4321.trycloudflare.com\"\n" +
		"exec sleep 30\n"
	if err := os.WriteFile(fakeBin, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	oldPaths := cloudflaredSearchPaths
	oldLook := cloudflaredLookPath
	oldInstall := cloudflaredInstallPath
	cloudflaredSearchPaths = []string{fakeBin}
	cloudflaredLookPath = func(string) (string, error) { return "", os.ErrNotExist }
	cloudflaredInstallPath = fakeBin
	defer func() {
		cloudflaredSearchPaths = oldPaths
		cloudflaredLookPath = oldLook
		cloudflaredInstallPath = oldInstall
	}()

	s := New(Config{
		StateDir:    stateDir,
		UIDir:       uiDir,
		AccessToken: "K7M9-P4X2",
		HTTPSAddr:   "127.0.0.1:0",
		TLSCertDir:  certDir,
	}, nil)
	defer func() {
		s.TunnelManager().Stop()
		tunnelManagers.Delete(s)
		s.stopHTTPS(false)
	}()

	lanHTTP := "http://" + lanHost() + ":8080"
	wantLANPNG, err := qrcode.Encode(lanHTTP, qrcode.Medium, 320)
	if err != nil {
		t.Fatal(err)
	}
	wantTunnelURL := "https://remote-deck-4321.trycloudflare.com"
	wantTunnelPNG, err := qrcode.Encode(wantTunnelURL, qrcode.Medium, 320)
	if err != nil {
		t.Fatal(err)
	}
	wantTokenLeakPNG, _ := qrcode.Encode(wantTunnelURL+"?token=K7M9-P4X2", qrcode.Medium, 320)

	// 1. Tunnel off: deck-local /v1/enginex/console-qr answers locally and encodes LAN URL without token.
	for _, path := range []string{"/v1/enginex/console-qr", migration.APIPrefix + "/console-qr"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Host = "127.0.0.1:8080"
		r.RemoteAddr = "127.0.0.1:50000"
		s.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s local status = %d (%s)", path, w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); ct != "image/png" {
			t.Fatalf("%s content-type = %q", path, ct)
		}
		if !bytes.Equal(w.Body.Bytes(), wantLANPNG) {
			t.Fatalf("%s off-state QR bytes did not match LAN URL %s", path, lanHTTP)
		}
	}

	// External or relayed requests to /v1/enginex/console-qr and /v1/enginex/tunnel must be refused (403).
	for _, tc := range []struct {
		method, path, remote, cfIP string
	}{
		{http.MethodGet, "/v1/enginex/console-qr", "192.0.2.50:1234", ""},
		{http.MethodGet, "/v1/enginex/console-qr", "127.0.0.1:1234", "203.0.113.9"},
		{http.MethodPost, "/v1/enginex/tunnel", "192.0.2.50:1234", ""},
		{http.MethodPost, "/v1/enginex/tunnel", "127.0.0.1:1234", "203.0.113.9"},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"enabled":true}`))
		r.RemoteAddr = tc.remote
		if tc.cfIP != "" {
			r.Header.Set("Cf-Connecting-Ip", tc.cfIP)
		}
		s.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s %s from remote=%q cfIP=%q got %d, want 403", tc.method, tc.path, tc.remote, tc.cfIP, w.Code)
		}
	}

	// /api/console-qr requires console auth.
	unauth := httptest.NewRecorder()
	s.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/console-qr", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("/api/console-qr without auth got %d, want 401", unauth.Code)
	}

	// 2. Toggle HTTPS on while tunnel is off -> qrTarget uses https://<lan>:<port>.
	wHTTPS := httptest.NewRecorder()
	rHTTPS := httptest.NewRequest(http.MethodPost, "/v1/enginex/ui?consoleHttps=1", nil)
	rHTTPS.RemoteAddr = "127.0.0.1:50001"
	s.ServeHTTP(wHTTPS, rHTTPS)
	if wHTTPS.Code != http.StatusOK {
		t.Fatalf("enable HTTPS failed: %d %s", wHTTPS.Code, wHTTPS.Body.String())
	}
	wantHTTPS := "https://" + lanHost() + ":" + s.httpsPortStr()
	rLAN := httptest.NewRequest(http.MethodGet, "/v1/enginex/console-qr", nil)
	rLAN.Host = "127.0.0.1:8080"
	if got := s.qrTarget(rLAN); got != wantHTTPS {
		t.Fatalf("https off-tunnel qrTarget = %q, want %q", got, wantHTTPS)
	}

	// 3. Start tunnel from deck-local POST /v1/enginex/tunnel -> qrTarget, /v1/enginex/console-qr, /api/console-qr, and /v1/enginex/ui follow tunnel URL.
	wStart := httptest.NewRecorder()
	rStart := httptest.NewRequest(http.MethodPost, "/v1/enginex/tunnel", strings.NewReader(`{"enabled":true}`))
	rStart.Host = "127.0.0.1:8080"
	rStart.RemoteAddr = "127.0.0.1:50002"
	s.ServeHTTP(wStart, rStart)
	if wStart.Code != http.StatusOK {
		t.Fatalf("deck tunnel start failed: %d %s", wStart.Code, wStart.Body.String())
	}
	var st TunnelStatus
	_ = json.Unmarshal(wStart.Body.Bytes(), &st)
	if !st.Enabled || st.Status != "running" || st.PublicURL != wantTunnelURL {
		t.Fatalf("unexpected tunnel status: %+v", st)
	}

	if got := s.qrTarget(rLAN); got != wantTunnelURL {
		t.Fatalf("tunnel-on qrTarget = %q, want %q", got, wantTunnelURL)
	}

	// Deck-local /v1/enginex/console-qr matches tunnel URL (no access code / token).
	wDeckQR := httptest.NewRecorder()
	rDeckQR := httptest.NewRequest(http.MethodGet, "/v1/enginex/console-qr", nil)
	rDeckQR.Host = "127.0.0.1:8080"
	rDeckQR.RemoteAddr = "127.0.0.1:50003"
	s.ServeHTTP(wDeckQR, rDeckQR)
	if wDeckQR.Code != http.StatusOK || !bytes.Equal(wDeckQR.Body.Bytes(), wantTunnelPNG) || bytes.Equal(wDeckQR.Body.Bytes(), wantTokenLeakPNG) {
		t.Fatalf("/v1/enginex/console-qr tunnel mismatch (code=%d len=%d)", wDeckQR.Code, wDeckQR.Body.Len())
	}

	// Web console /api/console-qr follows the tunnel too.
	wWebQR := httptest.NewRecorder()
	rWebQR := httptest.NewRequest(http.MethodGet, "/api/console-qr", nil)
	rWebQR.Host = "192.168.5.34:8080"
	rWebQR.Header.Set("Authorization", "Bearer K7M9-P4X2")
	s.ServeHTTP(wWebQR, rWebQR)
	if wWebQR.Code != http.StatusOK || !bytes.Equal(wWebQR.Body.Bytes(), wantTunnelPNG) {
		t.Fatalf("/api/console-qr tunnel mismatch (code=%d)", wWebQR.Code)
	}

	// /v1/enginex/ui consoleUrl also switches to tunnel URL.
	wUI := httptest.NewRecorder()
	rUI := httptest.NewRequest(http.MethodGet, "/v1/enginex/ui", nil)
	rUI.Host = "127.0.0.1:8080"
	rUI.RemoteAddr = "127.0.0.1:50004"
	s.ServeHTTP(wUI, rUI)
	if wUI.Code != http.StatusOK || !strings.Contains(wUI.Body.String(), `"consoleUrl":"`+wantTunnelURL+`"`) {
		t.Fatalf("/v1/enginex/ui consoleUrl did not follow tunnel: %d %s", wUI.Code, wUI.Body.String())
	}
	var view struct {
		Tunnel TunnelStatus `json:"tunnel"`
	}
	if json.Unmarshal(wUI.Body.Bytes(), &view) != nil || !view.Tunnel.Enabled || view.Tunnel.Status != "running" {
		t.Fatal("existing UI polling does not carry the shared tunnel state")
	}

	// 4. Explicit PublicURL wins over active tunnel.
	sWithPublic := &Server{cfg: Config{PublicURL: "https://deck.example.org", StateDir: stateDir, AccessToken: "K7M9-P4X2"}}
	tunnelManagers.Store(sWithPublic, s.TunnelManager())
	defer tunnelManagers.Delete(sWithPublic)
	if got := sWithPublic.qrTarget(rLAN); got != "https://deck.example.org" {
		t.Fatalf("explicit PublicURL must beat tunnel, got %q", got)
	}

	// 5. Stop tunnel from deck -> /v1/enginex/console-qr and /api/console-qr return to HTTPS LAN URL.
	wStop := httptest.NewRecorder()
	rStop := httptest.NewRequest(http.MethodPost, "/v1/enginex/tunnel", strings.NewReader(`{"enabled":false}`))
	rStop.RemoteAddr = "127.0.0.1:50005"
	s.ServeHTTP(wStop, rStop)
	if wStop.Code != http.StatusOK {
		t.Fatalf("deck tunnel stop failed: %d %s", wStop.Code, wStop.Body.String())
	}
	if got := s.qrTarget(rLAN); got != wantHTTPS {
		t.Fatalf("post-stop qrTarget = %q, want %q", got, wantHTTPS)
	}
	wantHTTPSPNG, _ := qrcode.Encode(wantHTTPS, qrcode.Medium, 320)
	wAfterStop := httptest.NewRecorder()
	rAfterStop := httptest.NewRequest(http.MethodGet, "/v1/enginex/console-qr", nil)
	rAfterStop.Host = "127.0.0.1:8080"
	rAfterStop.RemoteAddr = "127.0.0.1:50006"
	s.ServeHTTP(wAfterStop, rAfterStop)
	if wAfterStop.Code != http.StatusOK || !bytes.Equal(wAfterStop.Body.Bytes(), wantHTTPSPNG) {
		t.Fatalf("post-stop /v1/enginex/console-qr mismatch (code=%d)", wAfterStop.Code)
	}
}

func TestConsoleQRPublicURLNeverCarriesCredentials(t *testing.T) {
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir(), AccessToken: "fixture-access-code",
		PublicURL: "https://user:fixture-password@deck.example.org/console?token=fixture-token&cookie=fixture-cookie#fixture-access-code"}, nil)
	defer s.Close()
	want := "https://deck.example.org/console"
	r := httptest.NewRequest("GET", "/v1/enginex/console-qr?token=ignored-request-token", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	if got := s.qrTarget(r); got != want {
		t.Fatalf("unsafe QR target: %q", got)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	png, _ := qrcode.Encode(want, qrcode.Medium, 320)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), png) {
		t.Fatal("deck QR does not encode the safe console URL")
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/v1/enginex/ui", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	s.ServeHTTP(w, r)
	var body struct {
		ConsoleURL string `json:"consoleUrl"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.ConsoleURL != want {
		t.Fatalf("unsafe About URL: %q", body.ConsoleURL)
	}
}

func TestDeckTunnelStatusRedactsNamedCredential(t *testing.T) {
	m := &TunnelManager{enabled: true, status: "error", token: "fixture-named-token", lastErr: "failure: fixture-named-token"}
	if strings.Contains(m.Status("").Error, "fixture-named-token") {
		t.Fatal("named credential appears in deck error text")
	}
}
