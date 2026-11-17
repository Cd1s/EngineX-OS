package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAppleMusicLibraryCatalogID(t *testing.T) {
	for _, tc := range []struct{ params, relationship, want string }{{"12", "34", "12"}, {"", "34", "34"}, {"", "", ""}} {
		var r amResource
		r.ID, r.Type, r.Attributes.Name = "i.library", "library-songs", "Song"
		r.Attributes.PlayParams.CatalogID = tc.params
		if tc.relationship != "" {
			var c amResource
			c.ID = tc.relationship
			r.Relationships.Catalog.Data = []amResource{c}
		}
		got, ok := r.track()
		if !ok || got.ID != "i.library" || got.CatalogID != tc.want {
			t.Fatalf("catalog mapping: %+v", got)
		}
	}
}

func TestAppleMusicDownloadToCatalogAtomicAndFailure(t *testing.T) {
	for _, tc := range []struct {
		name, catalog string
		broken        bool
	}{{"library lookup", "", false}, {"catalog hint", "123", false}, {"interrupted download", "123", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", filepath.Join(dir, "unavailable-system-temp"))
			final := filepath.Join(dir, "applemusic-i.library.flac")
			var lookup, downloads int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/me/library/songs/i.library/catalog":
					lookup++
					if r.Header.Get("Media-User-Token") != "fixture-user" {
						t.Error("missing library authorization")
					}
					io.WriteString(w, `{"data":[{"id":"123","type":"songs"}]}`)
				case "/v1/download":
					downloads++
					if r.Header.Get("Authorization") != "Bearer fixture-helper" {
						t.Error("helper authorization missing")
					}
					var in map[string]string
					json.NewDecoder(r.Body).Decode(&in)
					if in["url"] != "https://music.apple.com/us/song/123" {
						t.Error("helper received a library ID")
					}
					io.WriteString(w, `{"file":"/file","quality":{"lossless":true}}`)
				case "/file":
					if _, err := os.Stat(final); !os.IsNotExist(err) {
						t.Error("partial file published")
					}

					if tc.broken {
						w.Header().Set("Content-Length", "1000")
					}
					io.WriteString(w, "finished-flac-fixture")
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			am := &AppleMusic{BaseURL: srv.URL, HelperURL: srv.URL, HelperToken: "fixture-helper", owner: AppleMusicOwner{UserToken: "fixture-user", DevToken: "fixture-dev"}}
			out, err := am.DownloadTo(context.Background(), Track{ID: "i.library", CatalogID: tc.catalog}, dir, "applemusic-i.library")
			if tc.broken {
				if err == nil || out != nil {
					t.Fatal("accepted interrupted download")
				}
				if _, err := os.Stat(final); !os.IsNotExist(err) {
					t.Fatal("published failed download")
				}
			} else {
				if err != nil || out.Path != final {
					t.Fatalf("download: %+v %v", out, err)
				}
				b, _ := os.ReadFile(final)
				info, _ := os.Stat(final)
				if string(b) != "finished-flac-fixture" || info.Mode().Perm() != 0o644 {
					t.Fatal("file content or mode")
				}
			}
			parts, _ := filepath.Glob(filepath.Join(dir, ".part-am-*"))
			if len(parts) != 0 {
				t.Fatal("temporary files leaked")
			}
			if downloads != 1 || (tc.catalog == "" && lookup != 1) || (tc.catalog != "" && lookup != 0) {
				t.Fatal("incorrect catalog lookup count")
			}
		})
	}
}

func TestAppleMusicHelperStatusLoginAndRedaction(t *testing.T) {
	state := "signed-out"
	helper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-helper" {
			t.Error("missing helper auth")
		}
		switch r.URL.Path {
		case "/healthz":
			io.WriteString(w, "ok")
			return
		case "/v1/login":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			if in["username"] != "fixture-user" || in["password"] != "fixture-password" {
				t.Error("login payload")
			}
			state = "2fa"
		case "/v1/2fa":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			if in["code"] != "123456" {
				t.Error("code payload")
			}
			state = "ready"
		}
		json.NewEncoder(w).Encode(map[string]string{"state": state, "message": "fixture-password fixture-helper fixture-user"})
	}))
	am := &AppleMusic{}
	if am.HelperStatus(context.Background()).Configured {
		t.Fatal("unconfigured status")
	}
	am.SetHelper(helper.URL, "", "fixture-helper")
	status := am.HelperStatus(context.Background())
	if !status.Configured || !status.Online || status.SignedIn {
		t.Fatal("signed-out status")
	}
	status, err := am.HelperLoginBegin(context.Background(), "fixture-user", "fixture-password")
	if err != nil || status.LoginState != "2fa" {
		t.Fatal("2FA transition", err)
	}
	status, err = am.HelperLogin2FA(context.Background(), "123456")
	if err != nil || !status.SignedIn || !am.Ready() || !am.HelperStatus(context.Background()).SignedIn {
		t.Fatal("ready transition", err)
	}
	if _, err = am.HelperLogin2FA(context.Background(), "abcdef"); err == nil {
		t.Fatal("accepted invalid code")
	}
	b, _ := json.Marshal(status)
	if strings.Contains(string(b), "fixture-") {
		t.Fatal("helper echoed credentials")
	}
	helper.Close()
	if am.HelperStatus(context.Background()).Online || am.Ready() {
		t.Fatal("offline status")
	}
}

func TestAppleMusicHelperConcurrentConfig(t *testing.T) {
	am := &AppleMusic{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				am.SetHelper("http://helper.invalid", "", "fixture")
				_ = am.HelperConfig()
			}
		}()
	}
	wg.Wait()
}

func TestAppleMusicHelperDoesNotForwardCredentials(t *testing.T) {
	var leaked bool
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer external.Close()
	helper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/download" {
			fmt.Fprintf(w, `{"file":%q}`, external.URL+"/file")
			return
		}
		http.Redirect(w, r, external.URL, 302)
	}))
	defer helper.Close()
	am := &AppleMusic{HelperURL: helper.URL, HelperToken: "fixture"}
	if _, err := am.DownloadTo(context.Background(), Track{ID: "123"}, t.TempDir(), "track"); err == nil {
		t.Fatal("accepted foreign file URL")
	}
	if am.HelperStatus(context.Background()).Online || leaked {
		t.Fatal("followed credential-bearing redirect")
	}
}

func TestAppleMusicHelperProbeTimeout(t *testing.T) {
	helper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer helper.Close()
	am := &AppleMusic{HelperURL: helper.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if am.HelperStatus(ctx).Online {
		t.Fatal("timed out helper online")
	}
}

func TestAppleMusicHelperCredentialsSyncAndDevTokenFallback(t *testing.T) {
	devTok := fakeJWT(time.Now().Add(30 * 24 * time.Hour))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			io.WriteString(w, "ok")
		case "/v1/login/status":
			json.NewEncoder(w).Encode(map[string]any{"state": "ready", "signedIn": true})
		case "/v1/login/credentials":
			if r.Header.Get("Authorization") != "Bearer fixture-helper" {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"signedIn":       true,
				"mediaUserToken": "synced-user-token",
				"developerToken": devTok,
				"storefront":     "TH",
			})
		case "/us/browse":
			w.WriteHeader(http.StatusBadGateway)
		case "/v1/catalog/th/search":
			if r.Header.Get("Authorization") != "Bearer "+devTok {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"results": map[string]any{
					"songs": map[string]any{"data": []any{song("338349243", "Sandstorm", "FI123")}},
				},
			})
		case "/v1/me/library/playlists":
			if r.Header.Get("Authorization") != "Bearer "+devTok || r.Header.Get("Media-User-Token") != "synced-user-token" {
				w.WriteHeader(403)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": []any{map[string]any{"id": "p.lib1", "type": "library-playlists", "attributes": map[string]any{"name": "My Sets"}}},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	ownerPath := filepath.Join(t.TempDir(), "applemusic-owner.json")
	am := &AppleMusic{BaseURL: srv.URL, WebURL: srv.URL, OwnerPath: ownerPath, HelperURL: srv.URL, HelperToken: "fixture-helper"}

	st := am.HelperStatus(t.Context())
	if !st.SignedIn || !am.Ready() {
		t.Fatalf("expected signedIn & Ready, got %+v", st)
	}
	owner := am.Owner()
	if owner.UserToken != "synced-user-token" || owner.Storefront != "th" {
		t.Fatalf("credentials not synced: %+v", owner)
	}

	// Persisted owner file restores login without needing helper probe on restart.
	reloaded := &AppleMusic{BaseURL: srv.URL, WebURL: srv.URL, OwnerPath: ownerPath}
	if err := reloaded.Load(); err != nil || !reloaded.Ready() || reloaded.Owner().UserToken != "synced-user-token" {
		t.Fatalf("reloaded login lost: err=%v owner=%+v", err, reloaded.Owner())
	}

	// Web scrape fails (502), so API calls fall back to the helper developer token.
	tracks, err := am.Search(t.Context(), "sandstorm")
	if err != nil || len(tracks) != 1 || tracks[0].ID != "338349243" {
		t.Fatalf("search fallback failed: %+v %v", tracks, err)
	}
	lists, err := am.Playlists(t.Context())
	if err != nil || len(lists) != 1 || lists[0].ID != "lib:p.lib1" {
		t.Fatalf("playlists fallback failed: %+v %v", lists, err)
	}

	// Forget clears memory and removes owner file while keeping HelperConfig intact.
	if err := am.Forget(); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if am.Ready() || am.Owner().UserToken != "" || am.HelperConfig().URL != srv.URL {
		t.Fatalf("Forget left stale state or cleared helper: ready=%v owner=%+v helper=%+v", am.Ready(), am.Owner(), am.HelperConfig())
	}
	if _, err := os.Stat(ownerPath); !os.IsNotExist(err) {
		t.Fatalf("owner file still exists after Forget: %v", err)
	}

	// Cold start with saved UserToken + blocked WebURL syncs helperDevToken on demand.
	cold := &AppleMusic{BaseURL: srv.URL, WebURL: srv.URL, HelperURL: srv.URL, HelperToken: "fixture-helper", owner: AppleMusicOwner{UserToken: "synced-user-token", Storefront: "th"}}
	tracks, err = cold.Search(t.Context(), "sandstorm")
	if err != nil || len(tracks) != 1 {
		t.Fatalf("cold developerToken sync fallback failed: %+v %v", tracks, err)
	}
}

func TestAppleMusicProbeHelperOnlineVsOfflineCooldown(t *testing.T) {
	var mu sync.Mutex
	online := false
	signedIn := false
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		up := online
		in := signedIn
		mu.Unlock()
		if !up {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/v1/login/status":
			st := "signed-out"
			if in {
				st = "ready"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"state": st, "signedIn": in})
		case "/v1/login/credentials":
			_ = json.NewEncoder(w).Encode(map[string]any{"signedIn": in, "mediaUserToken": "probe-tok", "developerToken": "eyJabcde12345.eyJabcde123456789012345.eyJabcde123456789012345", "storefront": "us"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	am := &AppleMusic{HelperURL: srv.URL}
	// Offline helper backs off for the full 30s cooldown.
	st := am.ProbeHelper(t.Context(), 30*time.Second)
	if st.Online || st.SignedIn || hits != 1 {
		t.Fatalf("expected 1 offline probe, got st=%+v hits=%d", st, hits)
	}
	_ = am.ProbeHelper(t.Context(), 30*time.Second)
	if hits != 1 {
		t.Fatalf("offline helper should stay on cooldown, got hits=%d", hits)
	}

	// Once online (via direct HelperStatus), ProbeHelper uses a short 2s cooldown instead of 30s.
	mu.Lock()
	online = true
	mu.Unlock()
	if st = am.HelperStatus(t.Context()); !st.Online || st.SignedIn {
		t.Fatalf("expected online signed-out status, got %+v", st)
	}
	mu.Lock()
	signedIn = true
	beforeHits := hits
	mu.Unlock()
	// Backdate helperProbedAt by 2.1s (< 30s, > 2s) to verify online 2s cooldown without sleeping.
	am.mu.Lock()
	am.helperProbedAt = time.Now().Add(-2100 * time.Millisecond)
	am.mu.Unlock()
	st = am.ProbeHelper(t.Context(), 30*time.Second)
	mu.Lock()
	afterHits := hits
	mu.Unlock()
	if !st.SignedIn || !am.Ready() || afterHits <= beforeHits {
		t.Fatalf("online helper did not re-probe after 2s: st=%+v ready=%v hits=%d->%d", st, am.Ready(), beforeHits, afterHits)
	}
}
