package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// BH-GC2-3: Server.ServeHTTP hands every /v1/ path to the gateway's mux, so a
// /v1/ route registered only on s.mux (unlock-gen2, uninstall-gen2, tailscale)
// answered 200 with the gateway's empty page and did nothing. Every /v1/ route
// of s.mux must also be on the gateway.
func TestEveryV1RouteReachesTheGateway(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	server := regexp.MustCompile(`s\.mux\.HandleFunc\("([A-Z]+ /v1/[^"]+)"`)
	gateway := map[string]bool{}
	var routes []string
	for _, f := range files {
		if filepath.Ext(f) != ".go" || len(f) > 8 && f[len(f)-8:] == "_test.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`g\.mux\.HandleFunc\("([A-Z]+ /v1/[^"]+)"`).FindAllSubmatch(b, -1) {
			gateway[string(m[1])] = true
		}
		for _, m := range server.FindAllSubmatch(b, -1) {
			routes = append(routes, string(m[1]))
		}
	}
	if len(routes) == 0 {
		t.Fatal("no /v1/ routes found")
	}
	for _, r := range routes {
		if !gateway[r] {
			t.Errorf("%s is registered on s.mux only and never reached", r)
		}
	}
}

func TestGen2UnlockStatusThroughServeHTTP(t *testing.T) {
	s := New(Config{AccessToken: "t", MediaRoot: t.TempDir(), UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	for _, path := range []string{"/v1/enginex/unlock-gen2", "/v1/enginex/tailscale"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:43210"
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if _, empty := out["totalNumberOfItems"]; empty {
			t.Fatalf("%s answered with the gateway's empty page: %s", path, rec.Body.String())
		}
	}
}
