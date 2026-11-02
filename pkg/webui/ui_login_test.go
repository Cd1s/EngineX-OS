package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func deezerProfiles(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": 7, "title": "Mine", "nb_tracks": 1}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPhoneLoginNeedsTokenAndLinksDeezer(t *testing.T) {
	state := t.TempDir()
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: state, DeezerURL: deezerProfiles(t).URL}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/sc5000/login?provider=deezer", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	flow := ui.logins["deezer"]
	if flow == nil || !regexp.MustCompile(`:8080/m/deezer\?t=`).MatchString(flow.target) {
		t.Fatalf("flow %+v", flow)
	}
	qr, err := http.Get(srv.URL + "/v1/sc5000/login/qr?provider=deezer")
	if err != nil {
		t.Fatal(err)
	}
	qr.Body.Close()
	if qr.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("qr type %s", qr.Header.Get("Content-Type"))
	}
	bad, err := http.PostForm(srv.URL+"/m/deezer?t=wrong", url.Values{"user": {"123"}})
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != 403 {
		t.Fatalf("wrong token: %d", bad.StatusCode)
	}
	token := flow.token
	ok, err := http.PostForm(srv.URL+"/m/deezer?t="+token, url.Values{"user": {"https://www.deezer.com/profile/123?x=1"}})
	if err != nil {
		t.Fatal(err)
	}
	ok.Body.Close()
	data, err := os.ReadFile(filepath.Join(state, "deezer-owner.json"))
	if err != nil || !strings.Contains(string(data), "123") {
		t.Fatalf("not saved: %s %v", data, err)
	}
	// The token works once, and the deck learns that the login finished.
	again, err := http.PostForm(srv.URL+"/m/deezer?t="+token, url.Values{"user": {"456"}})
	if err != nil {
		t.Fatal(err)
	}
	again.Body.Close()
	if again.StatusCode != 403 {
		t.Fatalf("reused token: %d", again.StatusCode)
	}
	status, err := http.Get(srv.URL + "/v1/sc5000/login?provider=deezer")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(status.Body)
	status.Body.Close()
	if !strings.Contains(string(body), `"status":"done"`) {
		t.Fatalf("status %s", body)
	}
}

func TestSpotifyPhonePageAsksForClientIDFirst(t *testing.T) {
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/sc5000/login?provider=spotify", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	page, err := http.Get(srv.URL + "/m/spotify?t=" + ui.logins["spotify"].token)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(body), "Client ID") {
		t.Fatalf("page %s", body)
	}
}
