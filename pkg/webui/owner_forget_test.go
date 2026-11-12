package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
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

// BH2-A5-7: a SoundCloud account signed in through the relay has its tokens in
// the store shared with the app's token, and a pasted token is also copied to
// the lookup keys. Signing out left both, and the deck went on using them.
func TestSigningSoundCloudOutDropsTheAccountTokens(t *testing.T) {
	dir := t.TempDir()
	jobs := NewJobs(Pipeline{Guard: resourceguard.New(resourceguard.Config{Disabled: true})})
	t.Cleanup(func() { jobs.Close() })
	bpm := &providers.BPMStore{Path: filepath.Join(dir, "bpm.json"), ConfigPath: filepath.Join(dir, "bpm-config.json")}
	s := New(Config{AccessToken: "test-token-1234", StateDir: dir, BPM: bpm}, jobs)
	tokenFile, ownerFile := filepath.Join(dir, "soundcloud-token.json"), filepath.Join(dir, "soundcloud-owner.json")
	signOut := func() {
		t.Helper()
		s.mu.Lock()
		s.sessions["own"] = &Session{ID: "own", Provider: "soundcloud", Ready: true, p: fixtureProvider{}, tracks: map[string]providers.Track{}, expires: time.Now().Add(time.Hour)}
		s.mu.Unlock()
		req := httptest.NewRequest("DELETE", "/api/sessions/own", nil)
		req.Header.Set("Authorization", "Bearer test-token-1234")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE session: %d %s", rec.Code, rec.Body.String())
		}
	}

	// Signed in through the relay: the app's token and the account's, one file.
	app := `{"client_id":"app-id","access_token":"app-access","expiry":"2099-01-01T00:00:00Z"}`
	if err := os.WriteFile(tokenFile, []byte(app), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.soundCloudAuth.SetUser("account-access", "account-refresh", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveJSON(ownerFile, soundCloudOwner{7, "dj", "app-id", "app-secret", ""}); err != nil {
		t.Fatal(err)
	}
	signOut()
	if s.soundCloudAuth.HasUser() {
		t.Error("the account's token is still held in memory after signing out")
	}
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "account-access") || strings.Contains(string(b), "account-refresh") {
		t.Errorf("the account's tokens are still in the token file after signing out: %s", b)
	}
	if !strings.Contains(string(b), `"app-access"`) || !strings.Contains(string(b), `"app-id"`) {
		t.Errorf("the app's token went with the account: %s", b)
	}

	// Linked with a pasted token: the copy in the lookup keys goes, the app's keys stay.
	if err := bpm.SaveConfig(providers.BPMConfig{SoundCloudClientID: "app-id", SoundCloudSecret: "app-secret", SoundCloudToken: "pasted-token"}); err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveJSON(ownerFile, soundCloudOwner{7, "dj", "app-id", "app-secret", "pasted-token"}); err != nil {
		t.Fatal(err)
	}
	signOut()
	if keys := bpm.Config(); keys.SoundCloudToken != "" || keys.SoundCloudClientID != "app-id" || keys.SoundCloudSecret != "app-secret" {
		t.Errorf("lookup keys after signing out: %+v, want the app's keys without the account's token", keys)
	}
	if p := s.ownerSoundCloudProvider(); p.Token != "" || p.UserID != 0 || p.ClientID != "app-id" {
		t.Errorf("the next SoundCloud session would start with token %q, user %d, client %q", p.Token, p.UserID, p.ClientID)
	}

	// A token that was only ever entered as a lookup key is a setting of the deck.
	if err := bpm.SaveConfig(providers.BPMConfig{SoundCloudClientID: "app-id", SoundCloudSecret: "app-secret", SoundCloudToken: "lookup-token"}); err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveJSON(ownerFile, soundCloudOwner{7, "dj", "app-id", "app-secret", ""}); err != nil {
		t.Fatal(err)
	}
	signOut()
	if keys := bpm.Config(); keys.SoundCloudToken != "lookup-token" {
		t.Errorf("a lookup key that was not the account's login was removed: %+v", keys)
	}
}
