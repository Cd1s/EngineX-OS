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
	routes := []string{"/v1/enginex/ui"}
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

func TestUSBPreparingStatusAndErrorLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preparing")
	errPath := path + ".err"
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir(), USBPreparingPath: path}, nil)
	defer s.Close()

	get := func(route string) (map[string]any, string) {
		t.Helper()
		r := httptest.NewRequest("GET", route, nil)
		r.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", route, w.Code, w.Body.String())
		}
		var v map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v, w.Header().Get("X-UI-Digest")
	}

	base, baseDigest := get("/v1/enginex/ui")
	if _, ok := base["usbPrepare"]; ok {
		t.Fatalf("unexpected usbPrepare when idle: %v", base)
	}

	if err := os.WriteFile(path, []byte(`{"state":"loading","kind":"sd","done":1200,"total":21352}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/v1/enginex/ui"} {
		v, d := get(route)
		if v["usbPreparing"] != true {
			t.Fatalf("%s: want usbPreparing=true, got %v", route, v["usbPreparing"])
		}
		if d == baseDigest {
			t.Fatalf("%s: digest did not change", route)
		}
		up, ok := v["usbPrepare"].(map[string]any)
		if !ok || up["state"] != "loading" || up["kind"] != "sd" || up["done"] != float64(1200) || up["total"] != float64(21352) {
			t.Fatalf("%s: unexpected usbPrepare=%v", route, v["usbPrepare"])
		}
	}

	// #301: a first import that goes on after the mount, then waits for the next mount.
	for _, st := range []string{"importing", "ready"} {
		if err := os.WriteFile(path, []byte(`{"state":"`+st+`","kind":"usb","done":3200,"total":20000}`), 0o644); err != nil {
			t.Fatal(err)
		}
		v, _ := get("/v1/enginex/ui")
		if up, ok := v["usbPrepare"].(map[string]any); v["usbPreparing"] != true || !ok || up["state"] != st {
			t.Fatalf("%s: usbPreparing=%v usbPrepare=%v", st, v["usbPreparing"], v["usbPrepare"])
		}
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(errPath, []byte(`{"state":"error","kind":"usb","error":"bridge failed: corrupt pdb"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	v, _ := get("/v1/enginex/ui")
	if _, ok := v["usbPreparing"]; ok {
		t.Fatalf("error state must not set usbPreparing: %v", v)
	}
	up, ok := v["usbPrepare"].(map[string]any)
	if !ok || up["state"] != "error" || up["kind"] != "usb" || up["error"] != "bridge failed: corrupt pdb" {
		t.Fatalf("unexpected error usbPrepare=%v", v["usbPrepare"])
	}

	expired := time.Now().Add(-9 * time.Second)
	if err := os.Chtimes(errPath, expired, expired); err != nil {
		t.Fatal(err)
	}
	v, d := get("/v1/enginex/ui")
	if _, ok := v["usbPrepare"]; ok || d != baseDigest {
		t.Fatalf("expired error must disappear (got %v)", v["usbPrepare"])
	}
}
