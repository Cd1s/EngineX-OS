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
