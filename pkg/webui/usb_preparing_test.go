package webui

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUSBPreparingLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preparing")
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir(), USBPreparingPath: path}, nil)
	defer s.Close()
	poll := func(route string, active bool) string {
		t.Helper()
		r := httptest.NewRequest("GET", route, nil)
		r.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		start := time.Now()
		s.ServeHTTP(w, r)
		if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
			t.Fatalf("poll took %v", elapsed)
		}
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", route, w.Code, w.Body.String())
		}
		var v map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if string(v["sources"]) == "null" || string(v["loggedIn"]) == "null" {
			t.Fatalf("null array field: %s", w.Body.String())
		}
		if active {
			if string(v["usbPreparing"]) != "true" {
				t.Fatalf("missing active state: %s", w.Body.String())
			}
		} else if _, ok := v["usbPreparing"]; ok {
			t.Fatal("inactive state must be omitted")
		}
		digest := w.Header().Get("X-UI-Digest")
		if digest == "" {
			t.Fatal("missing digest")
		}
		return digest
	}
	routes := []string{"/v1/enginex/ui", "/v1/sc5000/ui"}
	baseline := poll(routes[0], false)
	for _, route := range routes {
		if poll(route, false) != baseline {
			t.Fatal("baseline differs by route")
		}
	}
	if err := os.WriteFile(path, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	preparing := poll(routes[0], true)
	if preparing == baseline {
		t.Fatal("state did not change digest")
	}
	for _, route := range routes {
		for i := 0; i < 2; i++ {
			if poll(route, true) != preparing {
				t.Fatal("preparing digest is unstable")
			}
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		if poll(route, false) != baseline {
			t.Fatal("baseline not restored")
		}
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, age := range []time.Duration{-31 * time.Second, 6 * time.Second} {
		stamp := time.Now().Add(age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		for _, route := range routes {
			if poll(route, false) != baseline {
				t.Fatal("invalid timestamp changed digest")
			}
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "fresh")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		if poll(route, false) != baseline {
			t.Fatal("symlink changed digest")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		poll(route, false)
	}
}
