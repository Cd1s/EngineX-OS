package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// BH2-A5-7: "Log out" in the console closed the session but left the saved login of
// every service except Spotify, and the account was back after the next restart.
func TestSigningAnOwnerOutRemovesTheSavedLogin(t *testing.T) {
	s, dir := newTestServer(t)
	saved := map[string]string{
		"deezer":     `{"UserID":"42","ARL":"cookie"}`,
		"netease":    `{"MUSIC_U":"cookie"}`,
		"applemusic": `{"token":"user-token"}`,
		"beatport":   `{"access_token":"a","refresh_token":"r"}`,
		"soundcloud": `{"UserID":7,"Username":"dj","ClientID":"app-id","Secret":"app-secret","Token":"user-token"}`,
	}
	file := func(provider string) string { return filepath.Join(dir, provider+"-owner.json") }
	signOut := func(id string) {
		t.Helper()
		req := httptest.NewRequest("DELETE", "/api/sessions/"+id, nil)
		req.Header.Set("Authorization", "Bearer test-token-1234")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE session %s: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	for provider, text := range saved {
		if err := os.WriteFile(file(provider), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// A guest session of a service ends without touching the owner's saved login.
	s.mu.Lock()
	s.sessions["guest"] = &Session{ID: "guest", Provider: "deezer", Guest: true, Ready: true, p: fixtureProvider{}, tracks: map[string]providers.Track{}, expires: time.Now().Add(time.Hour)}
	s.mu.Unlock()
	signOut("guest")
	if _, err := os.Stat(file("deezer")); err != nil {
		t.Fatalf("a guest session took the owner's saved Deezer login with it: %v", err)
	}
	for provider := range saved {
		s.mu.Lock()
		s.sessions["own-"+provider] = &Session{ID: "own-" + provider, Provider: provider, Ready: true, p: fixtureProvider{}, tracks: map[string]providers.Track{}, expires: time.Now().Add(time.Hour)}
		s.mu.Unlock()
		signOut("own-" + provider)
	}
	for _, provider := range []string{"deezer", "netease", "applemusic", "beatport"} {
		if b, err := os.ReadFile(file(provider)); err == nil {
			t.Errorf("%s: the saved login is still there after signing out: %s", provider, b)
		}
	}
	if s.beatport.Ready() {
		t.Error("beatport: still signed in in memory")
	}
	// SoundCloud: the account goes, the deck's app credentials stay.
	var sc soundCloudOwner
	b, err := os.ReadFile(file("soundcloud"))
	if err != nil || json.Unmarshal(b, &sc) != nil {
		t.Fatalf("soundcloud: app credentials file gone or unreadable: %v", err)
	}
	if sc.Token != "" || sc.UserID != 0 || sc.Username != "" || sc.ClientID != "app-id" || sc.Secret != "app-secret" {
		t.Errorf("soundcloud after signing out: %+v, want only the app credentials", sc)
	}
}
