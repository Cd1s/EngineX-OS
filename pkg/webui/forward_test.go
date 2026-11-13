package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Quick Tunnel and the Tailscale console forward carry callers from outside the deck into
// a local listener, so they arrive from loopback, like Engine. Through them, the console is
// reachable behind its login, and the deck-local streaming gateway is not.
func TestRemoteConsoleForwardHasNoGateway(t *testing.T) {
	s, _ := newTestServer(t)
	const code = "test-token-1234"

	if got := s.TunnelManager().localURL; got != "http://"+ForwardAddr {
		t.Fatalf("the tunnel forwards to %s, want the remote console listener %s", got, ForwardAddr)
	}

	forward := httptest.NewServer(s.ForwardHandler()) // a loopback listener, as the tunnel reaches it
	defer forward.Close()
	get := func(base, p string, hdr map[string]string) (int, string) {
		req, err := http.NewRequest("GET", base+p, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, e := resp.Body.Read(buf)
			b.WriteString(string(buf[:n]))
			if e != nil {
				break
			}
		}
		return resp.StatusCode, b.String()
	}
	for _, p := range []string{"/v1/sc5000/ui", "//v1/sc5000/ui", "/v1/../v1/sc5000/ui", "/%76%31/sc5000/ui", "/v1", "/au/oauth2/token", "/v1/sc5000/boot"} {
		status, body := get(forward.URL, p, nil)
		if status != http.StatusNotFound || strings.Contains(body, code) {
			t.Fatalf("forwarded GET %s: %d %q, want 404 without the access code", p, status, body)
		}
	}
	if status, _ := get(forward.URL, "/api/ui", nil); status != http.StatusUnauthorized {
		t.Fatalf("forwarded console without a login: %d, want 401", status)
	}
	if status, _ := get(forward.URL, "/api/ui", map[string]string{"Authorization": "Bearer " + code}); status != http.StatusOK {
		t.Fatalf("forwarded console with the access code: %d, want 200", status)
	}

	// A tunnel set up to reach the main listener: the edge's headers mark it as from outside.
	main := httptest.NewServer(s)
	defer main.Close()
	if status, body := get(main.URL, "/v1/sc5000/ui", nil); status != http.StatusOK || !strings.Contains(body, code) {
		t.Fatalf("Engine's own request to the gateway: %d", status)
	}
	for _, h := range []string{"Cf-Connecting-Ip", "Cf-Ray", "Cdn-Loop"} {
		if status, body := get(main.URL, "/v1/sc5000/ui", map[string]string{h: "x"}); status != http.StatusForbidden || strings.Contains(body, code) {
			t.Fatalf("gateway request carrying %s: %d %q, want 403", h, status, body)
		}
	}
}
