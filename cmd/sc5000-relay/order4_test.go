package main

import (
	"io"
	"net/http"
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
