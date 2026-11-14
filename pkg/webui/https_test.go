package webui

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestConsoleHTTPSCertGeneratedOnceMode0600AndReused(t *testing.T) {
	uiDir := t.TempDir()
	stateDir := t.TempDir()
	certDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("console_https=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ui1 := New(Config{
		AccessToken: "code-1234",
		UIDir:       uiDir,
		StateDir:    stateDir,
		HTTPSAddr:   "127.0.0.1:0",
		TLSCertDir:  certDir,
	}, nil)
	if !ui1.httpsActive() {
		ui1.Close()
		t.Fatal("HTTPS listener did not start when console_https=1")
	}

	certPath := filepath.Join(certDir, httpsCertFile)
	keyPath := filepath.Join(certDir, httpsKeyFile)
	for _, p := range []string{certPath, keyPath} {
		info, err := os.Stat(p)
		if err != nil {
			ui1.Close()
			t.Fatalf("stat %s: %v", p, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			ui1.Close()
			t.Fatalf("%s mode = %04o, want 0600", p, perm)
		}
	}

	pair1, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		ui1.Close()
		t.Fatalf("load generated cert: %v", err)
	}
	leaf1, err := x509.ParseCertificate(pair1.Certificate[0])
	if err != nil {
		ui1.Close()
		t.Fatalf("parse generated cert: %v", err)
	}
	if err := leaf1.CheckSignature(leaf1.SignatureAlgorithm, leaf1.RawTBSCertificate, leaf1.Signature); err != nil {
		ui1.Close()
		t.Fatalf("self-signed signature check failed: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf1)
	if _, err := leaf1.Verify(x509.VerifyOptions{Roots: roots, DNSName: "localhost"}); err != nil {
		ui1.Close()
		t.Fatalf("self-signed verify with pinned root failed: %v", err)
	}

	if !slices.Contains(leaf1.DNSNames, "localhost") {
		ui1.Close()
		t.Fatalf("DNSNames %v missing localhost", leaf1.DNSNames)
	}
	if h, err := os.Hostname(); err == nil && h != "" && net.ParseIP(h) == nil {
		if !slices.Contains(leaf1.DNSNames, h) {
			ui1.Close()
			t.Fatalf("DNSNames %v missing hostname %q", leaf1.DNSNames, h)
		}
	}
	hasIP := func(want net.IP) bool {
		for _, ip := range leaf1.IPAddresses {
			if ip.Equal(want) {
				return true
			}
		}
		return false
	}
	for _, mustIP := range []string{"127.0.0.1", "::1"} {
		if !hasIP(net.ParseIP(mustIP)) {
			ui1.Close()
			t.Fatalf("IPAddresses %v missing %s", leaf1.IPAddresses, mustIP)
		}
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP != nil && !n.IP.IsUnspecified() && !hasIP(n.IP) {
				ui1.Close()
				t.Fatalf("IPAddresses %v missing interface IP %s", leaf1.IPAddresses, n.IP)
			}
		}
	}

	certBytes1, _ := os.ReadFile(certPath)
	keyBytes1, _ := os.ReadFile(keyPath)
	ui1.Close()

	// Simulate a later boot: the stored cert/key must be reused unchanged.
	ui2 := New(Config{
		AccessToken: "code-1234",
		UIDir:       uiDir,
		StateDir:    stateDir,
		HTTPSAddr:   "127.0.0.1:0",
		TLSCertDir:  certDir,
	}, nil)
	defer ui2.Close()
	if !ui2.httpsActive() {
		t.Fatal("HTTPS listener did not start on second boot")
	}
	certBytes2, _ := os.ReadFile(certPath)
	keyBytes2, _ := os.ReadFile(keyPath)
	if !bytes.Equal(certBytes1, certBytes2) || !bytes.Equal(keyBytes1, keyBytes2) {
		t.Fatal("certificate or key was regenerated instead of reused")
	}
}

func TestConsoleHTTPSToggleLifecycleAndBothListeners(t *testing.T) {
	mockRunner := &mockSystemRunner{outputs: make(map[string][]byte)}
	SetSystemRunner(mockRunner.Run)
	defer SetSystemRunner(nil)

	uiDir := t.TempDir()
	stateDir := t.TempDir()
	certDir := t.TempDir()

	ui := New(Config{
		AccessToken: "code-1234",
		UIDir:       uiDir,
		StateDir:    stateDir,
		HTTPSAddr:   "127.0.0.1:0",
		TLSCertDir:  certDir,
	}, nil)
	defer ui.Close()

	httpSrv := httptest.NewServer(ui)
	defer httpSrv.Close()

	// Default is off: no TLS listener, no cert generated, HTTP QR/link only.
	if ui.httpsActive() {
		t.Fatal("HTTPS listener started while consoleHttps is off")
	}
	if _, err := os.Stat(filepath.Join(certDir, httpsCertFile)); !os.IsNotExist(err) {
		t.Fatalf("cert generated before HTTPS was enabled: %v", err)
	}

	httpClient := httpSrv.Client()
	get := func(client *http.Client, base, path string, hdr map[string]string) (int, string, http.Header) {
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}

	code, body, _ := get(httpClient, httpSrv.URL, "/v1/sc5000/ui", nil)
	if code != 200 || !strings.Contains(body, `"consoleHttps":false`) || !strings.Contains(body, `"consoleUrl":"http://`) {
		t.Fatalf("off state /v1/sc5000/ui: %d %s", code, body)
	}
	lanReq := httptest.NewRequest(http.MethodGet, "/api/console-qr", nil)
	lanReq.Host = "192.168.5.34:8080"
	if got := ui.qrTarget(lanReq); got != "http://192.168.5.34:8080" {
		t.Fatalf("off state qrTarget = %q, want http://192.168.5.34:8080", got)
	}

	// Bad values must be rejected.
	badReq, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/ui?consoleHttps=maybe", nil)
	badReq.Header.Set("Authorization", "Bearer code-1234")
	badResp, err := httpClient.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != 400 || ui.httpsActive() {
		t.Fatalf("bad consoleHttps status=%d active=%v", badResp.StatusCode, ui.httpsActive())
	}

	// Turn HTTPS on via /api/ui.
	onReq, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/ui?consoleHttps=1", nil)
	onReq.Header.Set("Authorization", "Bearer code-1234")
	onResp, err := httpClient.Do(onReq)
	if err != nil {
		t.Fatal(err)
	}
	var st uiSettings
	if err := json.NewDecoder(onResp.Body).Decode(&st); err != nil {
		onResp.Body.Close()
		t.Fatal(err)
	}
	onResp.Body.Close()
	if onResp.StatusCode != 200 || !st.ConsoleHTTPS || st.HTTPSPort <= 0 || !ui.httpsActive() {
		t.Fatalf("enable HTTPS failed: status=%d settings=%+v active=%v", onResp.StatusCode, st, ui.httpsActive())
	}

	httpsBase := fmt.Sprintf("https://127.0.0.1:%d", st.HTTPSPort)
	tlsClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	// QR and consoleUrl now offer https.
	wantQR := fmt.Sprintf("https://192.168.5.34:%d", st.HTTPSPort)
	if got := ui.qrTarget(lanReq); got != wantQR {
		t.Fatalf("on state qrTarget = %q, want %q", got, wantQR)
	}
	loopQR := httptest.NewRequest(http.MethodGet, "/api/console-qr", nil)
	loopQR.Host = fmt.Sprintf("127.0.0.1:%d", st.HTTPSPort)
	loopQR.TLS = &tls.ConnectionState{}
	if got, want := ui.qrTarget(loopQR), fmt.Sprintf("https://%s:%d", lanHost(), st.HTTPSPort); got != want {
		t.Fatalf("on state loopback TLS qrTarget = %q, want %q", got, want)
	}
	cfQR := httptest.NewRequest(http.MethodGet, "/api/console-qr", nil)
	cfQR.Host = "abc-def.trycloudflare.com"
	if got := ui.qrTarget(cfQR); got != "https://abc-def.trycloudflare.com" {
		t.Fatalf("tunnel qrTarget = %q, want https://abc-def.trycloudflare.com", got)
	}

	// Both HTTP and HTTPS serve /v1/sc5000 and /v1/enginex identically.
	httpCodeSC, httpBodySC, httpHdrSC := get(httpClient, httpSrv.URL, "/v1/sc5000/ui", nil)
	httpCodeEX, httpBodyEX, httpHdrEX := get(httpClient, httpSrv.URL, "/v1/enginex/ui", nil)
	httpsCodeSC, httpsBodySC, httpsHdrSC := get(tlsClient, httpsBase, "/v1/sc5000/ui", nil)
	httpsCodeEX, httpsBodyEX, httpsHdrEX := get(tlsClient, httpsBase, "/v1/enginex/ui", nil)
	if httpCodeSC != 200 || httpCodeEX != 200 || httpsCodeSC != 200 || httpsCodeEX != 200 {
		t.Fatalf("status codes http=(%d,%d) https=(%d,%d)", httpCodeSC, httpCodeEX, httpsCodeSC, httpsCodeEX)
	}
	if httpBodySC != httpBodyEX || httpsBodySC != httpsBodyEX || httpBodySC != httpsBodySC {
		t.Fatalf("mismatch across ports/prefixes:\nhttp:  %s\nhttps: %s", httpBodySC, httpsBodySC)
	}
	if httpHdrSC.Get("X-UI-Digest") != httpsHdrSC.Get("X-UI-Digest") || httpsHdrSC.Get("X-UI-Digest") != httpsHdrEX.Get("X-UI-Digest") || httpHdrSC.Get("X-UI-Digest") != httpHdrEX.Get("X-UI-Digest") {
		t.Fatalf("digest mismatch across ports/prefixes")
	}
	if !strings.Contains(httpsBodyEX, fmt.Sprintf(`"consoleUrl":"https://%s:%d"`, lanHost(), st.HTTPSPort)) {
		t.Fatalf("consoleUrl did not switch to https: %s", httpsBodyEX)
	}

	for _, prefix := range []string{"/v1/sc5000", "/v1/enginex"} {
		for _, p := range []string{"/ssh", "/mounts", "/automix"} {
			hc, hb, _ := get(httpClient, httpSrv.URL, prefix+p, nil)
			tc, tb, _ := get(tlsClient, httpsBase, prefix+p, nil)
			if hc != 200 || tc != 200 || hb != tb {
				t.Fatalf("%s%s mismatch: http=(%d %q) https=(%d %q)", prefix, p, hc, hb, tc, tb)
			}
		}
		if tc, tb, _ := get(tlsClient, httpsBase, prefix+"/ui", map[string]string{"Cf-Connecting-Ip": "198.51.100.1"}); tc != 403 || strings.Contains(tb, "code-1234") {
			t.Fatalf("HTTPS %s/ui with tunnel header: %d %s", prefix, tc, tb)
		}
	}

	// Sign in over HTTPS and verify Secure cookie + authenticated console access.
	loginResp, err := tlsClient.PostForm(httpsBase+"/login", url.Values{"token": {"code-1234"}})
	if err != nil {
		t.Fatal(err)
	}
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("HTTPS login status = %d", loginResp.StatusCode)
	}
	var sessionCookie *http.Cookie
	for _, c := range loginResp.Cookies() {
		if c.Name == "stream_sync" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil || !sessionCookie.Secure {
		t.Fatalf("expected Secure stream_sync cookie, got %+v", sessionCookie)
	}

	// Turn HTTPS off over the HTTPS connection itself (must not deadlock).
	offReq, _ := http.NewRequest(http.MethodPost, httpsBase+"/api/ui?consoleHttps=0", nil)
	offReq.AddCookie(sessionCookie)
	offResp, err := tlsClient.Do(offReq)
	if err != nil {
		t.Fatal(err)
	}
	var stOff uiSettings
	json.NewDecoder(offResp.Body).Decode(&stOff)
	offResp.Body.Close()
	if offResp.StatusCode != 200 || stOff.ConsoleHTTPS || stOff.HTTPSPort != 0 {
		t.Fatalf("disable HTTPS failed: status=%d settings=%+v", offResp.StatusCode, stOff)
	}
	if ui.httpsActive() {
		t.Fatal("HTTPS listener still active after toggle off")
	}

	// The TLS port must refuse new connections immediately.
	conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", st.HTTPSPort), time.Second)
	if dialErr == nil {
		conn.Close()
		t.Fatal("TLS port still accepts connections after toggle off")
	}

	// HTTP listener keeps working unchanged and reverts QR / consoleUrl to http://.
	code, body, _ = get(httpClient, httpSrv.URL, "/v1/sc5000/ui", nil)
	if code != 200 || !strings.Contains(body, `"consoleHttps":false`) || !strings.Contains(body, `"consoleUrl":"http://`) {
		t.Fatalf("after off /v1/sc5000/ui: %d %s", code, body)
	}
	if got := ui.qrTarget(lanReq); got != "http://192.168.5.34:8080" {
		t.Fatalf("after off qrTarget = %q, want http://192.168.5.34:8080", got)
	}
}

func TestConsoleHTTPSRollbackWhenPortUnavailable(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	uiDir := t.TempDir()
	ui := New(Config{
		AccessToken: "code-1234",
		UIDir:       uiDir,
		StateDir:    t.TempDir(),
		HTTPSAddr:   occupied.Addr().String(),
		TLSCertDir:  t.TempDir(),
	}, nil)
	defer ui.Close()

	srv := httptest.NewServer(ui)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/ui?consoleHttps=1", nil)
	req.Header.Set("Authorization", "Bearer code-1234")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("occupied port status = %d, want 500", resp.StatusCode)
	}
	ui.uiMu.Lock()
	persisted := ui.loadUI().ConsoleHTTPS
	ui.uiMu.Unlock()
	if persisted {
		t.Fatal("consoleHttps remained enabled after listener failure")
	}
}
