package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func jsonHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestDeezerPrivateARLAndQuotaException(t *testing.T) {
	var apiPlaylistsCalls atomic.Int32
	origTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = origTransport })

	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Host == "www.deezer.com" && strings.Contains(r.URL.RawQuery, "deezer.getUserData"):
			return jsonHTTPResponse(200, `{"error":[],"results":{"checkForm":"tok","USER":{"USER_ID":777888,"BLOG_NAME":"PrivateDJ","OPTIONS":{"license_token":"lic","web_lossless":true}}}}`), nil
		case r.URL.Host == "api.deezer.com" && r.URL.Path == "/user/777888/playlists":
			apiPlaylistsCalls.Add(1)
			return jsonHTTPResponse(200, `{"error":{"type":"AccessDeniedException","message":"Private profile","code":200}}`), nil
		case r.URL.Host == "api.deezer.com" && r.URL.Path == "/user/999000/playlists":
			return jsonHTTPResponse(200, `{"error":{"type":"QuotaException","message":"Quota limit exceeded","code":4}}`), nil
		case r.URL.Host == "api.deezer.com" && r.URL.Path == "/user/404404/playlists":
			return jsonHTTPResponse(200, `{"error":{"type":"DataException","message":"no data","code":800}}`), nil
		default:
			return jsonHTTPResponse(404, `{"error":{"message":"not found","code":800}}`), nil
		}
	})

	s := newTestServer(t)
	h := s.routes()
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	arl := strings.Repeat("b", 192)

	// 1. Private Deezer account logging in with valid ARL must succeed without calling api.deezer.com.
	tokARL := "deezerprivatearl0001"
	w := do(h, "POST", "/m/deezer?t="+tokARL, url.Values{"arl": {arl}}.Encode(), form)
	if w.Code != 200 {
		t.Fatalf("private ARL login rejected: code=%d body=%s", w.Code, w.Body.String())
	}
	if got := apiPlaylistsCalls.Load(); got != 0 {
		t.Fatalf("ARL login queried api.deezer.com %d times, want 0", got)
	}
	poll := do(h, "GET", "/api/relay/session?t="+tokARL, "", nil).Body.String()
	if !strings.Contains(poll, `"userId":"777888"`) {
		t.Fatalf("unexpected session poll: %s", poll)
	}

	// 2. Public profile ID login must not fail solely due to api.deezer.com IP QuotaException.
	tokQuota := "deezerquotatoken0001"
	w = do(h, "POST", "/m/deezer?t="+tokQuota, url.Values{"user": {"999000"}}.Encode(), form)
	if w.Code != 200 {
		t.Fatalf("public user login rejected on QuotaException: code=%d body=%s", w.Code, w.Body.String())
	}

	// 3. Non-existent public user ID (DataException 800) must still be rejected.
	tokMissing := "deezermissingtok0001"
	w = do(h, "POST", "/m/deezer?t="+tokMissing, url.Values{"user": {"404404"}}.Encode(), form)
	if w.Code == 200 {
		t.Fatalf("non-existent Deezer user ID should be rejected, got 200")
	}
}

func TestSoundCloudPublicProfileLoginWithoutCredentials(t *testing.T) {
	s := newTestServer(t)
	h := s.routes()
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}

	// Empty submission and half-filled app credentials must be rejected.
	tokBad := "scpublicbadtoken0001"
	if w := do(h, "POST", "/m/soundcloud?t="+tokBad, "", form); w.Code == 200 {
		t.Fatal("empty SoundCloud submission accepted")
	}
	if w := do(h, "POST", "/m/soundcloud?t="+tokBad, "user=https://soundcloud.com/four-tet&clientId=only-id", form); w.Code == 200 {
		t.Fatal("half-filled ClientID without ClientSecret accepted")
	}

	// Credential-free public profile URL / username advertised on mobile page must succeed.
	tokPub := "scpublicoktoken00001"
	w := do(h, "POST", "/m/soundcloud?t="+tokPub, url.Values{"user": {"https://soundcloud.com/four-tet"}}.Encode(), form)
	if w.Code != 200 {
		t.Fatalf("credential-free SoundCloud profile rejected: code=%d body=%s", w.Code, w.Body.String())
	}
	poll := do(h, "GET", "/api/relay/session?t="+tokPub, "", nil).Body.String()
	if !strings.Contains(poll, `"status":"done"`) || !strings.Contains(poll, `"user":"https://soundcloud.com/four-tet"`) {
		t.Fatalf("unexpected poll response: %s", poll)
	}
}

func TestMobileFormResubmissionOverwritesCompletedSession(t *testing.T) {
	bpAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer bp-tok-1" && auth != "Bearer bp-tok-2" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[]}`))
	}))
	defer bpAPI.Close()

	s := newTestServer(t)
	s.beatportURL = bpAPI.URL
	h := s.routes()
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}

	// Trusted SoundCloud session re-submission updates both session and relay config.
	scTok := "scresubmittoken00001"
	do(h, "POST", "/api/relay/session", `{"token":"`+scTok+`","provider":"soundcloud"}`, map[string]string{"X-Deemix-Password": defaultPassword})
	if w := do(h, "POST", "/m/soundcloud?t="+scTok, "user=typo-dj&token=tok-1", form); w.Code != 200 {
		t.Fatalf("first SoundCloud submit failed: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", "/m/soundcloud?t="+scTok, "user=real-dj&token=tok-2", form); w.Code != 200 {
		t.Fatalf("second SoundCloud submit failed: %d %s", w.Code, w.Body.String())
	}
	scPoll := do(h, "GET", "/api/relay/session?t="+scTok, "", nil).Body.String()
	if !strings.Contains(scPoll, `"user":"real-dj"`) || !strings.Contains(scPoll, `"token":"tok-2"`) {
		t.Fatalf("SoundCloud re-submission silently dropped: %s", scPoll)
	}
	if got := s.cfg.get().SoundCloudToken; got != "tok-2" {
		t.Fatalf("trusted SoundCloud config token = %q, want tok-2", got)
	}

	// Beatport session re-submission updates stored Beatport token.
	bpTok := "bpresubmittoken00001"
	if w := do(h, "POST", "/m/beatport?t="+bpTok, "token=bp-tok-1", form); w.Code != 200 {
		t.Fatalf("first Beatport submit failed: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", "/m/beatport?t="+bpTok, "token=bp-tok-2", form); w.Code != 200 {
		t.Fatalf("second Beatport submit failed: %d %s", w.Code, w.Body.String())
	}
	bpPoll := do(h, "GET", "/api/relay/session?t="+bpTok, "", nil).Body.String()
	if !strings.Contains(bpPoll, `"access_token":"bp-tok-2"`) {
		t.Fatalf("Beatport re-submission silently dropped: %s", bpPoll)
	}
}
