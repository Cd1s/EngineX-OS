package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The gateway takes every /v1/ path before the console mux, so the /v1/sc5000
// admin routes must be registered on the gateway mux (loopback only), or they
// fall through to a Tidal-style empty list (#114, #120).
func TestGatewayServesSC5000AdminRoutes(t *testing.T) {
	mockRunner := &mockSystemRunner{outputs: make(map[string][]byte)}
	SetSystemRunner(mockRunner.Run)
	defer SetSystemRunner(nil)
	ui := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	do := func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for path, key := range map[string]string{"/v1/sc5000/ssh": `"enabled"`, "/v1/sc5000/mounts": `[]`, "/v1/sc5000/automix": `"enabled"`} {
		code, body := do("GET", path, "")
		if code != 200 || !strings.Contains(body, key) || strings.Contains(body, "totalNumberOfItems") {
			t.Fatalf("GET %s: %d %s", path, code, body)
		}
	}
	// A bad deck must reach the handler's validation (400), not the gateway's 200/404.
	code, body := do("POST", "/v1/sc5000/sampler/loop-to-pad", `{"deck":0,"pad":1,"trackPath":"/tmp/x.wav","inSample":0,"outSample":100}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "error") {
		t.Fatalf("loop-to-pad: %d %s", code, body)
	}
}
