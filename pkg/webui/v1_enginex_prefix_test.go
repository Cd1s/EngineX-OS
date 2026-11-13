package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// /v1/enginex/ is the deck API under the product's name; /v1/sc5000/ stays for older installers,
// UI packs and scripts. Both reach the same handlers behind the same rules.
func TestEngineXPrefixReachesTheSameRoutes(t *testing.T) {
	mockRunner := &mockSystemRunner{outputs: make(map[string][]byte)}
	SetSystemRunner(mockRunner.Run)
	defer SetSystemRunner(nil)
	ui := New(Config{AccessToken: "code-1234", StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	defer ui.Close()
	srv := httptest.NewServer(ui) // a loopback client, as Engine is
	defer srv.Close()
	do := func(method, path, body string, hdr map[string]string) (int, string, http.Header) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}

	// The UI status Engine polls: the same answer, digest included.
	oldCode, oldBody, oldHdr := do("GET", "/v1/sc5000/ui", "", nil)
	newCode, newBody, newHdr := do("GET", "/v1/enginex/ui", "", nil)
	if oldCode != 200 || newCode != 200 || newBody != oldBody || newHdr.Get("X-UI-Digest") != oldHdr.Get("X-UI-Digest") || !strings.Contains(newBody, `"accessCode":"code-1234"`) {
		t.Fatalf("/v1/enginex/ui: %d %q (digest %s), /v1/sc5000/ui: %d (digest %s)", newCode, newBody, newHdr.Get("X-UI-Digest"), oldCode, oldHdr.Get("X-UI-Digest"))
	}
	// Admin routes registered on the gateway, and a handler's own validation.
	for path, key := range map[string]string{"/ssh": `"enabled"`, "/mounts": `[]`, "/automix": `"enabled"`} {
		for _, prefix := range []string{"/v1/sc5000", "/v1/enginex"} {
			code, body, _ := do("GET", prefix+path, "", nil)
			if code != 200 || !strings.Contains(body, key) || strings.Contains(body, "totalNumberOfItems") {
				t.Fatalf("GET %s%s: %d %s", prefix, path, code, body)
			}
		}
	}
	if code, body, _ := do("POST", "/v1/enginex/sampler/loop-to-pad", `{"deck":0,"pad":1,"trackPath":"/tmp/x.wav","inSample":0,"outSample":100}`, nil); code != http.StatusBadRequest || !strings.Contains(body, "error") {
		t.Fatalf("loop-to-pad under /v1/enginex: %d %s", code, body)
	}
	// Only the deck itself: a tunnel edge's headers are refused under both prefixes.
	for _, prefix := range []string{"/v1/sc5000", "/v1/enginex"} {
		if code, body, _ := do("GET", prefix+"/ui", "", map[string]string{"Cf-Connecting-Ip": "198.51.100.1"}); code != 403 || strings.Contains(body, "code-1234") {
			t.Fatalf("%s/ui through a tunnel: %d", prefix, code)
		}
	}
	// Names that only look alike are not the prefix.
	if code, body, _ := do("GET", "/v1/enginexx/ui", "", nil); code == 200 && strings.Contains(body, "accessCode") {
		t.Fatal("/v1/enginexx/ reached the deck API")
	}
}

func TestEngineXPrefixKeepsTheLocalOnlyRules(t *testing.T) {
	ui := New(Config{AccessToken: "code-1234", StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	defer ui.Close()
	for _, c := range []struct {
		remote, path string
		code         int
	}{
		{"192.0.2.9:7", "/v1/enginex/ui", 403},
		{"192.0.2.9:7", "/v1/sc5000/ui", 403},
		{"[2001:db8::1]:7", "/v1/enginex/clock", 403},
		{"127.0.0.1:7", "/v1/enginex/ui", 200},
	} {
		r := httptest.NewRequest("GET", c.path, nil)
		r.RemoteAddr = c.remote
		w := httptest.NewRecorder()
		ui.ServeHTTP(w, r)
		if w.Code != c.code || (c.code != 200 && strings.Contains(w.Body.String(), "code-1234")) {
			t.Fatalf("GET %s from %s: %d, want %d", c.path, c.remote, w.Code, c.code)
		}
	}
	// The bare prefix answers as the old one does.
	bare := func(p string) (int, string) {
		r := httptest.NewRequest("GET", p, nil)
		r.RemoteAddr = "[::1]:7"
		w := httptest.NewRecorder()
		ui.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	oc, ob := bare("/v1/sc5000")
	if nc, nb := bare("/v1/enginex"); nc != oc || nb != ob {
		t.Fatalf("/v1/enginex: %d %q; /v1/sc5000: %d %q", nc, nb, oc, ob)
	}
	// The remote console forward has no route to either prefix.
	fw := httptest.NewServer(ui.ForwardHandler())
	defer fw.Close()
	for _, p := range []string{"/v1/enginex/ui", "/v1/enginex/../sc5000/ui", "//v1/enginex/ui"} {
		resp, err := http.Get(fw.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 404 || strings.Contains(string(b), "code-1234") {
			t.Fatalf("forwarded %s: %d", p, resp.StatusCode)
		}
	}
	// The boot readiness token is checked the same way under the new prefix.
	s, dir, _ := bootTestServer(t)
	putBootTestFile(t, dir, "boot-token", "attempt-one\n")
	if w := bootRequest(s, "POST", "/v1/enginex/boot/ready", `{"token":"older"}`, "127.0.0.1:7", ""); w.Code != 409 {
		t.Fatalf("stale token under /v1/enginex: %d", w.Code)
	}
	if w := bootRequest(s, "POST", "/v1/enginex/boot/ready", `{"token":"attempt-one"}`, "192.0.2.9:7", ""); w.Code != 403 {
		t.Fatalf("remote ready under /v1/enginex: %d", w.Code)
	}
	if w := bootRequest(s, "POST", "/v1/enginex/boot/ready", `{"token":"attempt-one"}`, "127.0.0.1:7", ""); w.Code != 200 {
		t.Fatalf("current ready under /v1/enginex: %d", w.Code)
	}
}
