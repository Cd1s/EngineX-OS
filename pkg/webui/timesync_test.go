package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func seedZoneinfoFixture(t *testing.T) string {
	t.Helper()
	z := t.TempDir()
	for _, rel := range []string{
		"Asia/Shanghai",
		"Asia/Tokyo",
		"America/New_York",
		"America/Argentina/Buenos_Aires",
		"Etc/UTC",
		"Universal",
		"UTC",
	} {
		p := filepath.Join(z, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("TZif2fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return z
}

func TestTimeZonePersistenceNormalizesSpacesSavesExplicitUTCAndRejectsInvalid(t *testing.T) {
	stateDir := t.TempDir()
	uiDir := t.TempDir()
	zoneinfo := seedZoneinfoFixture(t)

	srv := httptest.NewServer(New(Config{
		AccessToken: "123456",
		StateDir:    stateDir,
		UIDir:       uiDir,
		ZoneinfoDir: zoneinfo,
	}, nil))
	defer srv.Close()

	postTZ := func(val string) (saved bool, current string) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/enginex/ui?timezone="+val, "", nil)
		if err != nil {
			t.Fatalf("POST timezone=%q: %v", val, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST timezone=%q status = %d", val, resp.StatusCode)
		}
		var out struct {
			TimeZoneSaved bool   `json:"timezoneSaved"`
			TimeZone      string `json:"timezone"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode reply: %v", err)
		}
		return out.TimeZoneSaved, out.TimeZone
	}

	// 1. Engine's getSystemTimeZone() returns spaces for underscores ("America/New York").
	saved, cur := postTZ("America%2FNew%20York")
	if !saved || cur != "America/New_York" {
		t.Fatalf("space-formatted IANA zone: saved=%v cur=%q", saved, cur)
	}
	gotBytes, err := os.ReadFile(filepath.Join(stateDir, "timezone"))
	if err != nil || string(gotBytes) != "America/New_York\n" {
		t.Fatalf("state/timezone = %q, err=%v", string(gotBytes), err)
	}

	assertGETTimeZone := func(want string) {
		t.Helper()
		getResp, err := http.Get(srv.URL + "/v1/enginex/ui")
		if err != nil {
			t.Fatal(err)
		}
		defer getResp.Body.Close()
		if getResp.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/enginex/ui status = %d", getResp.StatusCode)
		}
		var status struct {
			TimeZone string `json:"timezone"`
		}
		if err := json.NewDecoder(getResp.Body).Decode(&status); err != nil {
			t.Fatal(err)
		}
		if status.TimeZone != want {
			t.Fatalf("GET /v1/enginex/ui timezone = %q, want %q", status.TimeZone, want)
		}
	}

	// 2. Manual selection of Asia/Shanghai updates state/timezone and appears in GET /v1/enginex/ui.
	saved, cur = postTZ("Asia%2FShanghai")
	if !saved || cur != "Asia/Shanghai" {
		t.Fatalf("Asia/Shanghai: saved=%v cur=%q", saved, cur)
	}
	assertGETTimeZone("Asia/Shanghai")

	// 3. Explicit user-chosen UTC and Etc/UTC are respected and saved like any zone.
	for _, utcZone := range []struct{ query, want string }{
		{"UTC", "UTC"},
		{"Etc%2FUTC", "Etc/UTC"},
	} {
		saved, cur = postTZ(utcZone.query)
		if !saved || cur != utcZone.want {
			t.Fatalf("explicit %s: saved=%v cur=%q, want %q", utcZone.query, saved, cur, utcZone.want)
		}
		gotBytes, err = os.ReadFile(filepath.Join(stateDir, "timezone"))
		if err != nil || string(gotBytes) != utcZone.want+"\n" {
			t.Fatalf("state/timezone after %s = %q, err=%v", utcZone.query, string(gotBytes), err)
		}
		assertGETTimeZone(utcZone.want)
	}

	// 4. Switch back to Asia/Shanghai, then verify invalid/empty/Universal/traversal never clobber it.
	saved, cur = postTZ("Asia%2FShanghai")
	if !saved || cur != "Asia/Shanghai" {
		t.Fatalf("Asia/Shanghai restore: saved=%v cur=%q", saved, cur)
	}
	for _, bad := range []string{
		"",
		"Universal",
		"..%2F..%2Fetc%2Fpasswd",
		"Mars%2FOlympus",
	} {
		saved, cur = postTZ(bad)
		if saved || cur != "Asia/Shanghai" {
			t.Fatalf("bad timezone %q: saved=%v cur=%q, want saved=false cur=Asia/Shanghai", bad, saved, cur)
		}
	}
	gotBytes, err = os.ReadFile(filepath.Join(stateDir, "timezone"))
	if err != nil || string(gotBytes) != "Asia/Shanghai\n" {
		t.Fatalf("state/timezone clobbered: %q err=%v", string(gotBytes), err)
	}
}
