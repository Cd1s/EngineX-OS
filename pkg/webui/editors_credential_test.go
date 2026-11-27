package webui

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// WebDAV round 4 (item 3's class: every editor that keeps a saved credential): the
// console's Apple Music helper editor kept the saved helper token when the helper's
// address changed and the token field was left empty, so the old token went to the new
// address (its status check, then every helper call), and a restart gave the
// configured default token to whatever helper the DJ had saved. The token now stays
// only with the same helper (scheme, host and port), and the configured default goes
// only to the configured helper.
func TestAppleMusicHelperTokenStaysWithItsHelper(t *testing.T) {
	const token, envToken = "am-helper-r4-marker", "am-environment-r4-marker"
	type seen struct {
		mu   sync.Mutex
		auth []string
	}
	helper := func() (*httptest.Server, *seen) {
		v := &seen{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			v.mu.Lock()
			v.auth = append(v.auth, r.Header.Get("Authorization"))
			v.mu.Unlock()
			if r.URL.Path == "/healthz" {
				w.Write([]byte("ok"))
				return
			}
			w.Write([]byte(`{"state":"ready"}`))
		}))
		t.Cleanup(srv.Close)
		return srv, v
	}
	sent := func(v *seen) []string {
		v.mu.Lock()
		defer v.mu.Unlock()
		return slices.Clone(v.auth)
	}
	a, _ := helper()
	b, seenB := helper()
	ui := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	t.Cleanup(ui.Close)
	save := func(helperURL, tok string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"helperUrl": helperURL, "helperToken": tok})
		w := httptest.NewRecorder()
		ui.appleMusicHelperSave(w, httptest.NewRequest(http.MethodPost, "/api/applemusic", bytes.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("save helper %s: HTTP %d %s", helperURL, w.Code, w.Body.String())
		}
	}
	save(a.URL, token)
	for _, same := range []string{a.URL + "/", a.URL + "/api"} {
		save(same, "")
		if ui.appleMusic.HelperConfig().Token != token {
			t.Errorf("the same helper saved as %s lost its token", same)
		}
	}
	for _, moved := range []string{b.URL, "http://localhost:" + strconv.Itoa(a.Listener.Addr().(*net.TCPAddr).Port), "https://" + a.Listener.Addr().String()} {
		save(a.URL, token)
		before := len(sent(seenB))
		save(moved, "")
		if ui.appleMusic.HelperConfig().Token != "" {
			t.Errorf("the helper moved to %s kept the old token", moved)
		}
		for _, auth := range sent(seenB)[before:] {
			if auth != "" {
				t.Errorf("the helper moved to %s was sent the old token", moved)
			}
		}
	}

	// The configured default token goes only to the configured helper.
	state := t.TempDir()
	if err := providers.SaveJSON(filepath.Join(state, "applemusic-helper.json"), map[string]string{"url": b.URL}); err != nil {
		t.Fatal(err)
	}
	other := New(Config{StateDir: state, UIDir: t.TempDir(), AppleMusicHelperURL: a.URL, AppleMusicHelperToken: envToken}, nil)
	t.Cleanup(other.Close)
	if other.appleMusic.HelperConfig().Token != "" {
		t.Error("a restart gave the configured default token to another helper the DJ saved")
	}
	// A helper address cleared: no saved token waits for the next one, and the
	// configured helper gets its own token from the next start.
	save("", "")
	if ui.appleMusic.HelperConfig().Token != "" {
		t.Error("the cleared helper address kept the old token for the next address")
	}
}
