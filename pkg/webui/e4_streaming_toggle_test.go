package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// TestE4ConnectedStreamingToggle verifies that toggling streaming services in QML while a
// runtime-discovered rekordbox player is present in uiSettings.Sources never sends the
// non-configurable "rekordbox" ID to POST /v1/enginex/ui (which returns HTTP 400), persists
// the toggled configurable services, and keeps "rekordbox" visible in both QML and server state.
func TestE4ConnectedStreamingToggle(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required to execute extracted QML scSetSource helper")
	}
	e := newRbxEnv(t, nil)
	var ui uiSettings
	for i := 0; i < 100; i++ {
		tidalGet(t, e.base, "/v1/enginex/ui", 200, &ui)
		if slices.Contains(ui.Sources, "rekordbox") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !slices.Contains(ui.Sources, "rekordbox") {
		t.Fatal("fixture did not discover the rekordbox player")
	}
	if !slices.Contains(ui.Sources, "deezer") || !slices.Contains(ui.Sources, "soundcloud") {
		t.Fatalf("fixture needs deezer and soundcloud initially enabled, got %v", ui.Sources)
	}
	src, err := os.ReadFile("../../tools/ui-mod/build_packs.py")
	if err != nil {
		t.Fatal(err)
	}
	fn := regexp.MustCompile(`(?ms)^\tfunction scSetSource\(.*?^\t}`).Find(src)
	if len(fn) == 0 {
		t.Fatal("missing scSetSource in build_packs.py")
	}
	ids, _ := json.Marshal(ui.Sources)
	script := `
let scUiLoaded = true, scSources = ` + string(ids) + `;
const queries = [];
function scCall(q) { queries.push(q); }
` + string(fn) + `
scSetSource('soundcloud', false);
const afterFirst = scSources.slice();
scSetSource('deezer', false);
console.log(JSON.stringify({queries, afterFirst, afterSecond: scSources}));
`
	out, err := exec.Command("node", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	var res struct {
		Queries     []string `json:"queries"`
		AfterFirst  []string `json:"afterFirst"`
		AfterSecond []string `json:"afterSecond"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("decode node output %q: %v", out, err)
	}
	if len(res.Queries) != 2 {
		t.Fatalf("expected 2 queries, got %v", res.Queries)
	}
	wantQueries := []string{"source_soundcloud=0", "source_deezer=0"}
	if !slices.Equal(res.Queries, wantQueries) {
		t.Fatalf("expected per-provider delta queries %v, got %v", wantQueries, res.Queries)
	}
	if !slices.Contains(res.AfterFirst, "rekordbox") || slices.Contains(res.AfterFirst, "soundcloud") {
		t.Fatalf("optimistic state after first toggle wrong: %v", res.AfterFirst)
	}
	if !slices.Contains(res.AfterSecond, "rekordbox") || slices.Contains(res.AfterSecond, "soundcloud") || slices.Contains(res.AfterSecond, "deezer") {
		t.Fatalf("optimistic state after second toggle wrong: %v", res.AfterSecond)
	}
	for i, q := range res.Queries {
		if strings.Contains(q, "rekordbox") {
			t.Fatalf("query[%d] leaked runtime-only rekordbox source: %q", i, q)
		}
		code, after := uiPost(t, e.base, q)
		if code != 200 {
			t.Fatalf("query[%d] %q returned HTTP %d", i, q, code)
		}
		if !slices.Contains(after.Sources, "rekordbox") {
			t.Fatalf("query[%d] lost discovered rekordbox visibility: %v", i, after.Sources)
		}
		if i == 1 && (slices.Contains(after.Sources, "soundcloud") || slices.Contains(after.Sources, "deezer")) {
			t.Fatalf("final persisted sources still contain toggled-off service: %v", after.Sources)
		}
	}
}

// TestE4LateCancelledSave verifies that a timed-out/cancelled settings POST held before handler
// entry cannot overwrite a newer acknowledged setting once released.
func TestE4LateCancelledSave(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	entered, release, applied := make(chan struct{}), make(chan struct{}), make(chan struct{})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Query().Get("ledBrightness") == "80" {
			close(entered)
			<-release
			defer close(applied)
		}
		s.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.URL+"/v1/enginex/ui?ledBrightness=80", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldDone := make(chan error, 1)
	go func() {
		res, err := endpoint.Client().Do(old)
		if res != nil {
			res.Body.Close()
		}
		oldDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first POST did not arrive")
	}
	cancel()
	if err := <-oldDone; err == nil {
		t.Fatal("first POST was not cancelled")
	}
	code, saved := uiPost(t, endpoint.URL, "ledBrightness=60")
	if code != http.StatusOK || saved.LedBrightness != 60 {
		t.Fatalf("replacement: %d, %d", code, saved.LedBrightness)
	}
	close(release)
	released = true
	select {
	case <-applied:
	case <-time.After(3 * time.Second):
		t.Fatal("old handler did not finish")
	}
	var final uiSettings
	tidalGet(t, endpoint.URL, "/v1/enginex/ui", http.StatusOK, &final)
	if final.LedBrightness != 60 {
		t.Fatalf("new value 60 was acknowledged; GET after the expired handler returned %d", final.LedBrightness)
	}
}

// TestE4WriteSeqRejectsStaleWrites verifies out-of-order X-UI-Client-Id / X-UI-Write-Seq POSTs
// are rejected with 409 Conflict across /v1/enginex/ui, /v1/enginex/bluetooth, and /v1/enginex/tunnel
// without overwriting newer committed values, while unheadered callers remain unaffected and
// client tracking stays bounded at 64 entries.
func TestE4WriteSeqRejectsStaleWrites(t *testing.T) {
	asDeck(t, "inmusic,jc11")

	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()

	postWithSeq := func(path, body, clientID string, seq int) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if clientID != "" {
			req.Header.Set("X-UI-Client-Id", clientID)
			req.Header.Set("X-UI-Write-Seq", strconv.Itoa(seq))
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	// 1. /v1/enginex/ui: seq=2 commits ledBrightness=60; delayed seq=1 (ledBrightness=80) returns 409.
	if code := postWithSeq("/v1/enginex/ui?ledBrightness=60", "", "qml-a", 2); code != http.StatusOK {
		t.Fatalf("ui seq=2 status=%d, want 200", code)
	}
	if code := postWithSeq("/v1/enginex/ui?ledBrightness=80", "", "qml-a", 1); code != http.StatusConflict {
		t.Fatalf("ui stale seq=1 status=%d, want 409", code)
	}
	if code := postWithSeq("/v1/enginex/ui?ledBrightness=80", "", "qml-a", 2); code != http.StatusConflict {
		t.Fatalf("ui duplicate seq=2 status=%d, want 409", code)
	}
	var ui uiSettings
	tidalGet(t, srv.URL, "/v1/enginex/ui", http.StatusOK, &ui)
	if ui.LedBrightness != 60 {
		t.Fatalf("ui ledBrightness=%d after stale seq, want 60", ui.LedBrightness)
	}
	// Unheadered caller still succeeds (backwards compatibility).
	if code := postWithSeq("/v1/enginex/ui?ledBrightness=40", "", "", 0); code != http.StatusOK {
		t.Fatalf("ui unheadered status=%d, want 200", code)
	}
	// Bounded client map eviction (64 entries): 64 newer clients evict qml-a so qml-a seq=1 succeeds again.
	for i := 0; i < 64; i++ {
		if code := postWithSeq("/v1/enginex/ui?autoGain=0", "", "client-"+strconv.Itoa(i), 1); code != http.StatusOK {
			t.Fatalf("eviction write %d status=%d, want 200", i, code)
		}
	}
	if code := postWithSeq("/v1/enginex/ui?autoGain=0", "", "qml-a", 1); code != http.StatusOK {
		t.Fatalf("evicted client qml-a seq=1 status=%d, want 200 (proves 64-entry bound)", code)
	}

	// 2. /v1/enginex/bluetooth: seq=2 commits Headphones; stale seq=1 (Booth) returns 409.
	if code := postWithSeq("/v1/enginex/bluetooth?assignLayer=Headphones", "", "qml-bt", 2); code != http.StatusOK {
		t.Fatalf("bt seq=2 status=%d, want 200", code)
	}
	if code := postWithSeq("/v1/enginex/bluetooth?assignLayer=Booth", "", "qml-bt", 1); code != http.StatusConflict {
		t.Fatalf("bt stale seq=1 status=%d, want 409", code)
	}
	var bt bluetoothStatus
	tidalGet(t, srv.URL, "/v1/enginex/bluetooth", http.StatusOK, &bt)
	if bt.AssignLayer != "Headphones" {
		t.Fatalf("bt assignLayer=%q after stale seq, want Headphones", bt.AssignLayer)
	}

	// 3. /v1/enginex/tunnel: seq=2 commits enabled=false; stale seq=1 (enabled=true) returns 409.
	if code := postWithSeq("/v1/enginex/tunnel", `{"enabled":false}`, "qml-tun", 2); code != http.StatusOK {
		t.Fatalf("tunnel seq=2 status=%d, want 200", code)
	}
	if code := postWithSeq("/v1/enginex/tunnel", `{"enabled":true}`, "qml-tun", 1); code != http.StatusConflict {
		t.Fatalf("tunnel stale seq=1 status=%d, want 409", code)
	}
	var tun TunnelStatus
	tidalGet(t, srv.URL, "/v1/enginex/tunnel", http.StatusOK, &tun)
	if tun.Enabled {
		t.Fatalf("tunnel enabled=true after stale seq, want false")
	}
}

type r3ObservedContext struct {
	context.Context
	observe func(error) error
}

func (c r3ObservedContext) Err() error { return c.observe(c.Context.Err()) }

// TestR3CancelledTunnelWhileWaitingForManager verifies that a tunnel POST cancelled while
// waiting on TunnelManager.mu does not mutate or persist tunnel state once the lock is released.
func TestR3CancelledTunnelWhileWaitingForManager(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	manager := s.TunnelManager()
	manager.mu.Lock()
	manager.enabled = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checked := make(chan struct{})
	var once sync.Once
	observed := r3ObservedContext{ctx, func(err error) error { once.Do(func() { close(checked) }); return err }}
	req := httptest.NewRequest(http.MethodPost, "/v1/enginex/tunnel", strings.NewReader(`{"enabled":false}`)).WithContext(observed)
	done := make(chan struct{})
	go func() { defer close(done); s.tunnelPost(httptest.NewRecorder(), req) }()
	select {
	case <-checked:
	case <-time.After(3 * time.Second):
		manager.mu.Unlock()
		<-done
		t.Fatal("did not reach first context check")
	}
	cancel()
	manager.mu.Unlock()
	<-done
	if !manager.Status("").Enabled {
		t.Fatal("cancelled request disabled tunnel after manager unlock")
	}
}

// TestE4UIStatusExplicitFeaturesOffAndPausedFollowDigest verifies that E4 POST /v1/enginex/ui
// replies delegate cleanly to uiStatus, carry matching X-UI-Digest headers against GET /v1/enginex/ui,
// and reflect hook feature transitions (featuresOff and shadowPausedFollow) across writes and digests.
func TestE4UIStatusExplicitFeaturesOffAndPausedFollowDigest(t *testing.T) {
	dir := t.TempDir()
	origState := hookFeatureStatePath
	origDisable := hookDisablePath
	origAlive := engineAlive
	hookFeatureStatePath = filepath.Join(dir, "hook-features")
	hookDisablePath = filepath.Join(dir, "hook-disable.conf")
	engineAlive = func(pid int) bool { return pid == 4242 }
	defer func() {
		hookFeatureStatePath = origState
		hookDisablePath = origDisable
		engineAlive = origAlive
	}()

	s := New(Config{UIDir: filepath.Join(dir, "ui"), StateDir: filepath.Join(dir, "state")}, nil)
	defer s.Close()

	type statusReply struct {
		AutoGain           bool     `json:"autoGain"`
		MasterLimiter      bool     `json:"masterLimiter"`
		LoggedIn           []string `json:"loggedIn"`
		FeaturesOff        []string `json:"featuresOff"`
		ShadowPausedFollow bool     `json:"shadowPausedFollow"`
	}
	call := func(method, query, seq string) (statusReply, string) {
		t.Helper()
		u := "/v1/enginex/ui"
		if query != "" {
			u += "?" + query
		}
		req := httptest.NewRequest(method, u, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		if seq != "" {
			req.Header.Set("X-UI-Client-Id", "e4-features-test")
			req.Header.Set("X-UI-Write-Seq", seq)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s returned HTTP %d: %s", method, u, rec.Code, rec.Body.String())
		}
		digest := rec.Header().Get("X-UI-Digest")
		if digest == "" {
			t.Fatalf("%s %s missing X-UI-Digest", method, u)
		}
		var out statusReply
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s decode: %v", method, u, err)
		}
		if out.LoggedIn == nil {
			t.Fatalf("%s %s omitted loggedIn array", method, u)
		}
		return out, digest
	}

	if err := os.WriteFile(hookFeatureStatePath, []byte("pid=4242\nbuild=stems,stems-eq,shadow-paused-follow\noff=stems\non=shadow-paused-follow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	post1, digPost1 := call(http.MethodPost, "autoGain=1", "1")
	get1, digGet1 := call(http.MethodGet, "", "")
	if !post1.AutoGain || !post1.ShadowPausedFollow || !slices.Contains(post1.FeaturesOff, "stems") {
		t.Fatalf("POST 1 unexpected state: %+v", post1)
	}
	if digPost1 != digGet1 || !get1.ShadowPausedFollow || !slices.Contains(get1.FeaturesOff, "stems") {
		t.Fatalf("POST/GET 1 digest or hook state mismatch: post=%q get=%q get=%+v", digPost1, digGet1, get1)
	}

	// Hook transitions: stems re-enabled, shadow-paused-follow turned off.
	if err := os.WriteFile(hookFeatureStatePath, []byte("pid=4242\nbuild=stems,stems-eq,shadow-paused-follow\noff=\non=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	post2, digPost2 := call(http.MethodPost, "masterLimiter=1", "2")
	get2, digGet2 := call(http.MethodGet, "", "")
	if !post2.MasterLimiter || post2.ShadowPausedFollow || len(post2.FeaturesOff) != 0 {
		t.Fatalf("POST 2 did not reflect updated hook features: %+v", post2)
	}
	if digPost2 != digGet2 || digPost2 == digPost1 || get2.ShadowPausedFollow || len(get2.FeaturesOff) != 0 {
		t.Fatalf("POST/GET 2 digest or cleared hook state mismatch: post1=%q post2=%q get2=%q state=%+v", digPost1, digPost2, digGet2, get2)
	}
}

// TestE4MonotonicServerRevAndLinearizability verifies Pillar 1: every Services reply
// (/v1/enginex/ui, /api/ui, /v1/enginex/login) carries a strictly monotonic server rev
// (JSON rev + X-UI-Rev header), unchanged GETs preserve rev & X-UI-Digest, session auth
// transitions advance rev, and a pre-commit GET held before a concurrent POST returns a
// strictly lower rev than the committed POST so the client can drop it unconditionally.
func TestE4MonotonicServerRevAndLinearizability(t *testing.T) {
	const accessToken = "E4-TEST-TOKEN"
	s := New(Config{AccessToken: accessToken, UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()

	srv := httptest.NewServer(s)
	defer srv.Close()

	type revUI struct {
		AutoGain bool     `json:"autoGain"`
		Sources  []string `json:"sources"`
		LoggedIn []string `json:"loggedIn"`
		Scope    string   `json:"scope"`
		Rev      int64    `json:"rev"`
	}
	type revLogin struct {
		Provider string `json:"provider"`
		Status   string `json:"status"`
		Rev      int64  `json:"rev"`
	}

	getUI := func(path string) (revUI, int64, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(path, "/api/") {
			req.AddCookie(&http.Cookie{Name: "stream_sync", Value: accessToken})
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status=%d", path, res.StatusCode)
		}
		hdrRev, err := strconv.ParseInt(res.Header.Get("X-UI-Rev"), 10, 64)
		if err != nil || hdrRev <= 0 {
			t.Fatalf("GET %s invalid X-UI-Rev=%q (err=%v)", path, res.Header.Get("X-UI-Rev"), err)
		}
		var out revUI
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatalf("GET %s decode: %v", path, err)
		}
		if out.Rev != hdrRev {
			t.Fatalf("GET %s body rev=%d != X-UI-Rev=%d", path, out.Rev, hdrRev)
		}
		return out, hdrRev, res.Header.Get("X-UI-Digest")
	}

	postUI := func(path string) (revUI, int64, string) {
		t.Helper()
		res, err := srv.Client().Post(srv.URL+path, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("POST %s status=%d", path, res.StatusCode)
		}
		hdrRev, err := strconv.ParseInt(res.Header.Get("X-UI-Rev"), 10, 64)
		if err != nil || hdrRev <= 0 {
			t.Fatalf("POST %s invalid X-UI-Rev=%q (err=%v)", path, res.Header.Get("X-UI-Rev"), err)
		}
		var out revUI
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatalf("POST %s decode: %v", path, err)
		}
		if out.Rev != hdrRev {
			t.Fatalf("POST %s body rev=%d != X-UI-Rev=%d", path, out.Rev, hdrRev)
		}
		return out, hdrRev, res.Header.Get("X-UI-Digest")
	}

	// 1. Unchanged GETs on /v1/enginex/ui and /api/ui report identical rev and X-UI-Digest.
	_, rev1, dig1 := getUI("/v1/enginex/ui")
	_, rev1b, dig1b := getUI("/v1/enginex/ui")
	_, apiRev1, _ := getUI("/api/ui")
	if rev1 != rev1b || rev1 != apiRev1 || dig1 == "" || dig1 != dig1b {
		t.Fatalf("unchanged GETs drifted: rev=(%d,%d,%d) dig=(%q,%q)", rev1, rev1b, apiRev1, dig1, dig1b)
	}

	// 2. Settings POST advances rev strictly, and subsequent GET matches both rev and digest.
	u2, rev2, dig2 := postUI("/v1/enginex/ui?autoGain=1")
	if !u2.AutoGain || rev2 <= rev1 || dig2 == dig1 {
		t.Fatalf("POST autoGain=1 did not advance rev/digest: rev1=%d rev2=%d dig1=%q dig2=%q", rev1, rev2, dig1, dig2)
	}
	_, rev2Get, dig2Get := getUI("/v1/enginex/ui")
	if rev2Get != rev2 || dig2Get != dig2 {
		t.Fatalf("GET after POST mismatch: rev=(%d,%d) dig=(%q,%q)", rev2, rev2Get, dig2, dig2Get)
	}

	// 3. Login endpoints (/v1/enginex/login POST & GET) carry rev and X-UI-Rev.
	startRes, err := srv.Client().Post(srv.URL+"/v1/enginex/login?provider=deezer", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var started revLogin
	if err := json.NewDecoder(startRes.Body).Decode(&started); err != nil {
		startRes.Body.Close()
		t.Fatal(err)
	}
	startHdr, err := strconv.ParseInt(startRes.Header.Get("X-UI-Rev"), 10, 64)
	startRes.Body.Close()
	if startRes.StatusCode != http.StatusOK || err != nil || started.Rev != startHdr || started.Rev < rev2 {
		t.Fatalf("POST /v1/enginex/login bad rev: status=%d hdr=%d body=%d want>=%d", startRes.StatusCode, startHdr, started.Rev, rev2)
	}
	pollRes, err := srv.Client().Get(srv.URL + "/v1/enginex/login?provider=deezer")
	if err != nil {
		t.Fatal(err)
	}
	var polled revLogin
	if err := json.NewDecoder(pollRes.Body).Decode(&polled); err != nil {
		pollRes.Body.Close()
		t.Fatal(err)
	}
	pollHdr, err := strconv.ParseInt(pollRes.Header.Get("X-UI-Rev"), 10, 64)
	pollRes.Body.Close()
	if pollRes.StatusCode != http.StatusOK || err != nil || polled.Rev != pollHdr || polled.Rev < started.Rev {
		t.Fatalf("GET /v1/enginex/login bad rev: status=%d hdr=%d body=%d want>=%d", pollRes.StatusCode, pollHdr, polled.Rev, started.Rev)
	}

	// 4. Owner auth completion advances rev monotonically.
	dzSess, err := s.ownerSession("deezer")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.saveDeezerOwner(dzSess.p.(*providers.Deezer), deezerOwner{UserID: "42", ARL: "test-arl-token"}); err != nil {
		t.Fatal(err)
	}
	u3, rev3, _ := getUI("/v1/enginex/ui")
	if rev3 <= rev2 || !slices.Contains(u3.LoggedIn, "deezer") {
		t.Fatalf("owner auth save did not advance rev/loggedIn: rev2=%d rev3=%d loggedIn=%v", rev2, rev3, u3.LoggedIn)
	}

	// 5. Linearizability: a GET that takes uiMu before a concurrent POST commits
	//    MUST receive a strictly lower rev than the POST's reply (even under -race).
	gateEntered := make(chan struct{})
	gateRelease := make(chan struct{})
	var gateOnce sync.Once
	var preCommitRev int64
	var preCommitAutoGain bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/enginex/ui", nil)
		s.uiStatus(rec, req)
		gateOnce.Do(func() { close(gateEntered) })
		<-gateRelease
		var snap revUI
		_ = json.Unmarshal(rec.Body.Bytes(), &snap)
		preCommitRev = snap.Rev
		preCommitAutoGain = snap.AutoGain
	}()
	<-gateEntered
	uPost, revPost, _ := postUI("/v1/enginex/ui?autoGain=0")
	close(gateRelease)
	wg.Wait()
	if uPost.AutoGain {
		t.Fatalf("POST autoGain=0 did not persist")
	}
	if !(preCommitRev > 0 && preCommitRev < revPost) {
		t.Fatalf("pre-commit GET rev=%d must be strictly < committed POST rev=%d (preAutoGain=%v)", preCommitRev, revPost, preCommitAutoGain)
	}

	// 6. Concurrent POST/GET storm under -race keeps rev strictly positive and monotonic per serialized write.
	var storm sync.WaitGroup
	errs := make(chan string, 32)
	for i := 0; i < 8; i++ {
		i := i
		storm.Add(1)
		go func() {
			defer storm.Done()
			if i%2 == 0 {
				val := "0"
				if i%4 == 0 {
					val = "1"
				}
				res, err := srv.Client().Post(srv.URL+"/v1/enginex/ui?masterLimiter="+val, "", nil)
				if err != nil {
					errs <- err.Error()
					return
				}
				defer res.Body.Close()
				var u revUI
				if err := json.NewDecoder(res.Body).Decode(&u); err != nil || u.Rev <= 0 {
					errs <- "bad POST rev"
				}
			} else {
				res, err := srv.Client().Get(srv.URL + "/v1/enginex/ui")
				if err != nil {
					errs <- err.Error()
					return
				}
				defer res.Body.Close()
				var u revUI
				if err := json.NewDecoder(res.Body).Decode(&u); err != nil || u.Rev <= 0 {
					errs <- "bad GET rev"
				}
			}
		}()
	}
	storm.Wait()
	close(errs)
	for msg := range errs {
		t.Fatal(msg)
	}
	_, revFinal, _ := getUI("/v1/enginex/ui")
	if revFinal < revPost+4 {
		t.Fatalf("4 concurrent POSTs must advance rev by at least 4: before=%d after=%d", revPost, revFinal)
	}
}

// TestE4SourceDeltaPreservesOtherClients verifies Pillar 2: per-provider delta writes
// (source_<id>=0|1) modify only the named provider against current server state without
// clobbering concurrent changes from another client (e.g. Web Console), reject unknown
// sources or invalid booleans with HTTP 400, clear active scope when that source is
// disabled, and keep full-list sources=... backwards-compatible.
func TestE4SourceDeltaPreservesOtherClients(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()

	type deltaUI struct {
		Sources []string `json:"sources"`
		Scope   string   `json:"scope"`
		Rev     int64    `json:"rev"`
	}
	postDelta := func(query string) (int, deltaUI) {
		t.Helper()
		res, err := srv.Client().Post(srv.URL+"/v1/enginex/ui?"+query, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out deltaUI
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}

	// Start with tidal + deezer enabled (soundcloud disabled).
	code, initUI := postDelta("sources=tidal,deezer")
	if code != http.StatusOK || !slices.Equal(initUI.Sources, []string{"tidal", "deezer"}) {
		t.Fatalf("seed sources failed: code=%d sources=%v", code, initUI.Sources)
	}
	revSeed := initUI.Rev

	// 1. Web Console disables Deezer via delta while Deck still has stale state.
	code, afterConsole := postDelta("source_deezer=0")
	if code != http.StatusOK {
		t.Fatalf("console source_deezer=0 returned %d", code)
	}
	if slices.Contains(afterConsole.Sources, "deezer") || !slices.Contains(afterConsole.Sources, "tidal") {
		t.Fatalf("console disable deezer wrong: %v", afterConsole.Sources)
	}
	if afterConsole.Rev <= revSeed {
		t.Fatalf("source_deezer=0 did not advance rev: %d -> %d", revSeed, afterConsole.Rev)
	}

	// 2. Deck toggles SoundCloud ON using delta source_soundcloud=1 -> Deezer MUST stay OFF!
	code, afterDeck := postDelta("source_soundcloud=1")
	if code != http.StatusOK {
		t.Fatalf("deck source_soundcloud=1 returned %d", code)
	}
	if !slices.Equal(afterDeck.Sources, []string{"tidal", "soundcloud"}) {
		t.Fatalf("delta write clobbered Web Console choice: got %v, want [tidal soundcloud]", afterDeck.Sources)
	}
	if afterDeck.Rev <= afterConsole.Rev {
		t.Fatalf("source_soundcloud=1 did not advance rev: %d -> %d", afterConsole.Rev, afterDeck.Rev)
	}

	// 3. Re-enabling an already-enabled source preserves canonical uiServices order without duplicates.
	code, dedup := postDelta("source_netease=1&source_soundcloud=1")
	if code != http.StatusOK {
		t.Fatalf("multi-delta returned %d", code)
	}
	if !slices.Equal(dedup.Sources, []string{"tidal", "netease", "soundcloud"}) {
		t.Fatalf("canonical order / dedup wrong: got %v, want [tidal netease soundcloud]", dedup.Sources)
	}

	// 4. Disabling active scope clears v.Scope cleanly.
	code, scoped := postDelta("scope=tidal")
	if code != http.StatusOK || scoped.Scope != "tidal" {
		t.Fatalf("set scope=tidal failed: code=%d scope=%q", code, scoped.Scope)
	}
	code, afterDisableScope := postDelta("source_tidal=0")
	if code != http.StatusOK || slices.Contains(afterDisableScope.Sources, "tidal") || afterDisableScope.Scope != "" {
		t.Fatalf("disabling active scope failed: code=%d scope=%q sources=%v", code, afterDisableScope.Scope, afterDisableScope.Sources)
	}

	// 5. Validation: unknown source, runtime-only rekordbox, and invalid bool all return HTTP 400
	//    without mutating persisted sources or rev.
	revBeforeBad := afterDisableScope.Rev
	for _, bad := range []string{"source_bogus=1", "source_rekordbox=1", "source_deezer=maybe", "source_=1"} {
		badCode, _ := postDelta(bad)
		if badCode != http.StatusBadRequest {
			t.Fatalf("query %q returned HTTP %d, want 400", bad, badCode)
		}
	}
	var check deltaUI
	tidalGet(t, srv.URL, "/v1/enginex/ui", http.StatusOK, &check)
	if check.Rev != revBeforeBad || !slices.Equal(check.Sources, []string{"netease", "soundcloud"}) {
		t.Fatalf("invalid delta mutated state: rev=%d (want %d) sources=%v", check.Rev, revBeforeBad, check.Sources)
	}

	// 6. Legacy full-list sources= remains backwards-compatible for Web Console callers.
	code, legacy := postDelta("sources=tidal,spotify")
	if code != http.StatusOK || !slices.Equal(legacy.Sources, []string{"tidal", "spotify"}) || legacy.Rev <= revBeforeBad {
		t.Fatalf("legacy sources= failed: code=%d rev=%d sources=%v", code, legacy.Rev, legacy.Sources)
	}
}

type e4FinalResponse struct {
	Code        int
	HeaderRev   int64
	Rev         int64
	HeaderEpoch string
	Epoch       string
	Digest      string
	Body        map[string]json.RawMessage
	Sources     []string
	Logged      []string
}

func (r e4FinalResponse) has(id string) bool { return slices.Contains(r.Logged, id) }

type e4FinalAPI struct {
	s *Server
	h *httptest.Server
}

func e4FinalStart(cfg Config) e4FinalAPI {
	s := New(cfg, nil)
	return e4FinalAPI{s, httptest.NewServer(s)}
}
func (a e4FinalAPI) close() { a.h.Close(); a.s.Close() }

func (a e4FinalAPI) call(method, path, payload string) (e4FinalResponse, error) {
	var out e4FinalResponse
	req, err := http.NewRequest(method, a.h.URL+path, strings.NewReader(payload))
	if err != nil {
		return out, err
	}
	if strings.HasPrefix(path, "/api/") {
		req.AddCookie(&http.Cookie{Name: "stream_sync", Value: a.s.AccessToken()})
	}
	if payload != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.h.Client().Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	out.Code = resp.StatusCode
	out.HeaderRev, _ = strconv.ParseInt(resp.Header.Get("X-UI-Rev"), 10, 64)
	out.HeaderEpoch = resp.Header.Get("X-UI-Epoch")
	out.Digest = resp.Header.Get("X-UI-Digest")
	if err = json.NewDecoder(resp.Body).Decode(&out.Body); err != nil {
		return out, err
	}
	_ = json.Unmarshal(out.Body["rev"], &out.Rev)
	_ = json.Unmarshal(out.Body["epoch"], &out.Epoch)
	_ = json.Unmarshal(out.Body["sources"], &out.Sources)
	_ = json.Unmarshal(out.Body["loggedIn"], &out.Logged)
	return out, nil
}

func (a e4FinalAPI) must(t *testing.T, method, path, payload string) e4FinalResponse {
	t.Helper()
	r, e := a.call(method, path, payload)
	if e != nil || r.Code/100 != 2 {
		t.Fatalf("HTTP fixture response %d: %v", r.Code, e)
	}
	return r
}

// TestE4FinalAuthSnapshot verifies R15 BLOCK 1: saved settings, auth/login membership,
// rev, and epoch are published as one atomic snapshot — never pairing rev=1 with Deezer
// signed in alongside an earlier rev=1 with Deezer signed out.
func TestE4FinalAuthSnapshot(t *testing.T) {
	a := e4FinalStart(Config{UIDir: t.TempDir(), StateDir: t.TempDir()})
	defer a.close()
	dz, e := a.s.ownerSession("deezer")
	if e != nil {
		t.Fatal(e)
	}
	first := a.must(t, "GET", "/v1/enginex/ui", "")
	a.s.tidal.authMu.Lock()
	type result struct {
		v   e4FinalResponse
		err error
	}
	ch := make(chan result, 1)
	go func() { v, err := a.call("GET", "/v1/enginex/ui", ""); ch <- result{v, err} }()
	deadline := time.Now().Add(5 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		b := make([]byte, 512*1024)
		b = b[:runtime.Stack(b, true)]
		for _, g := range bytes.Split(b, []byte("\n\n")) {
			if bytes.Contains(g, []byte("realAuth(")) && bytes.Contains(g, []byte(".uiStatus(")) {
				blocked = true
				break
			}
		}
		if blocked {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !blocked {
		a.s.tidal.authMu.Unlock()
		<-ch
		t.Fatal("read did not reach snapshot gate")
	}
	e = a.s.saveDeezerOwner(dz.p.(*providers.Deezer), deezerOwner{UserID: "42", ARL: "fixture"})
	a.s.tidal.authMu.Unlock()
	mixed := <-ch
	if e != nil || mixed.err != nil {
		t.Fatalf("fixture could not change auth: save=%v get=%v", e, mixed.err)
	}
	last := a.must(t, "GET", "/v1/enginex/ui", "")
	t.Logf("before=(%d,%v), gated GET=(%d,%v), after=(%d,%v)", first.Rev, first.has("deezer"), mixed.v.Rev, mixed.v.has("deezer"), last.Rev, last.has("deezer"))
	if first.has("deezer") || !last.has("deezer") || (mixed.v.has("deezer") && mixed.v.Rev != last.Rev) {
		t.Fatalf("auth body and revision are from different snapshots: first=(%d,%v) mixed=(%d,%v) last=(%d,%v)",
			first.Rev, first.has("deezer"), mixed.v.Rev, mixed.v.has("deezer"), last.Rev, last.has("deezer"))
	}
	if mixed.v.Rev != mixed.v.HeaderRev || mixed.v.Epoch == "" || mixed.v.Epoch != mixed.v.HeaderEpoch {
		t.Fatalf("gated GET metadata mismatch: rev=%d/%d epoch=%q/%q", mixed.v.Rev, mixed.v.HeaderRev, mixed.v.Epoch, mixed.v.HeaderEpoch)
	}

	// Concurrent stress under -race: alternating settings + auth mutations must never expose
	// two distinct auth/settings states under the same (epoch, rev).
	type seenState struct {
		deezer   bool
		autoGain string
	}
	var mu sync.Mutex
	seen := map[int64]seenState{}
	var wg sync.WaitGroup
	errCh := make(chan string, 16)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if (id+j)%2 == 0 {
					_, _ = a.call("POST", "/v1/enginex/ui?autoGain="+strconv.Itoa(j%2), "")
				} else {
					arl := ""
					if j%2 == 0 {
						arl = fmt.Sprintf("arl-%d-%d", id, j)
					}
					_ = a.s.saveDeezerOwner(dz.p.(*providers.Deezer), deezerOwner{UserID: "42", ARL: arl})
				}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				r, err := a.call("GET", "/v1/enginex/ui", "")
				if err != nil || r.Code != http.StatusOK {
					errCh <- fmt.Sprintf("GET failed: code=%d err=%v", r.Code, err)
					return
				}
				if r.Rev <= 0 || r.Rev != r.HeaderRev || r.Epoch == "" || r.Epoch != r.HeaderEpoch {
					errCh <- fmt.Sprintf("bad metadata: rev=%d/%d epoch=%q/%q", r.Rev, r.HeaderRev, r.Epoch, r.HeaderEpoch)
					return
				}
				st := seenState{deezer: r.has("deezer"), autoGain: string(r.Body["autoGain"])}
				mu.Lock()
				if prev, ok := seen[r.Rev]; ok && prev != st {
					mu.Unlock()
					errCh <- fmt.Sprintf("rev %d observed conflicting states: %+v vs %+v", r.Rev, prev, st)
					return
				}
				seen[r.Rev] = st
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Fatal(msg)
	}
}

// TestE4FinalRestart verifies R15 BLOCK 2: restarting the server preserves persisted settings,
// advances persisted ui.rev monotonically across restarts (including consecutive write-free
// restarts), emits a distinct per-Server epoch (JSON + X-UI-Epoch), and degrades cleanly to a
// fresh epoch even when ui.rev cannot be persisted.
func TestE4FinalRestart(t *testing.T) {
	root := t.TempDir()
	cfg := Config{UIDir: filepath.Join(root, "ui"), StateDir: filepath.Join(root, "state")}
	a := e4FinalStart(cfg)
	for i := 0; i < 5; i++ {
		a.must(t, "POST", "/v1/enginex/ui?autoGain="+strconv.Itoa(i%2), "")
	}
	before := a.must(t, "GET", "/v1/enginex/ui", "")
	if before.Epoch == "" || before.Epoch != before.HeaderEpoch || before.Rev != before.HeaderRev {
		t.Fatalf("before restart metadata invalid: rev=%d/%d epoch=%q/%q", before.Rev, before.HeaderRev, before.Epoch, before.HeaderEpoch)
	}
	// ui.rev must live as a sibling file, never inside settings.conf.
	if b, err := os.ReadFile(filepath.Join(cfg.UIDir, "settings.conf")); err == nil && strings.Contains(string(b), "rev") {
		t.Fatalf("settings.conf must not contain ui.rev key: %s", string(b))
	}
	if _, err := os.Stat(filepath.Join(cfg.UIDir, "ui.rev")); err != nil {
		t.Fatalf("expected sibling ui.rev next to settings.conf: %v", err)
	}
	a.close()

	a = e4FinalStart(cfg)
	after := a.must(t, "GET", "/v1/enginex/ui", "")
	t.Logf("before restart (%s,%d), after restart (%s,%d), persisted setting %s/%s",
		before.Epoch, before.Rev, after.Epoch, after.Rev, before.Body["autoGain"], after.Body["autoGain"])
	if after.Rev <= before.Rev {
		t.Fatalf("new process served non-advancing revision: before=%d after=%d", before.Rev, after.Rev)
	}
	if after.Epoch == "" || after.Epoch == before.Epoch || after.Epoch != after.HeaderEpoch || after.Rev != after.HeaderRev {
		t.Fatalf("restart epoch/rev headers invalid: before=%q after=%q/%q rev=%d/%d",
			before.Epoch, after.Epoch, after.HeaderEpoch, after.Rev, after.HeaderRev)
	}
	if string(after.Body["autoGain"]) != string(before.Body["autoGain"]) {
		t.Fatalf("persisted setting lost across restart: %s vs %s", before.Body["autoGain"], after.Body["autoGain"])
	}
	a.close()

	// Second consecutive restart with zero writes still advances rev and rotates epoch.
	a = e4FinalStart(cfg)
	after2 := a.must(t, "GET", "/v1/enginex/ui", "")
	a.close()
	if after2.Rev <= after.Rev || after2.Epoch == "" || after2.Epoch == after.Epoch {
		t.Fatalf("second restart failed to advance rev/epoch: (%s,%d) -> (%s,%d)", after.Epoch, after.Rev, after2.Epoch, after2.Rev)
	}

	// Ephemeral/missing ui.rev (e.g. wiped or read-only dir) still starts cleanly at rev>=1 with a unique epoch.
	_ = os.Remove(filepath.Join(cfg.UIDir, "ui.rev"))
	a = e4FinalStart(cfg)
	defer a.close()
	ephemeral := a.must(t, "GET", "/v1/enginex/ui", "")
	if ephemeral.Rev < 1 || ephemeral.Epoch == "" || ephemeral.Epoch == after2.Epoch || ephemeral.Epoch != ephemeral.HeaderEpoch {
		t.Fatalf("ephemeral restart failed: (%q/%q, %d)", ephemeral.Epoch, ephemeral.HeaderEpoch, ephemeral.Rev)
	}
}

// TestE4FinalContracts verifies R15 FIX 4: ALL Services/UI/Login error and success responses
// emit consistent positive rev and non-empty epoch in both JSON body and X-UI-Rev/X-UI-Epoch headers.
func TestE4FinalContracts(t *testing.T) {
	a := e4FinalStart(Config{UIDir: t.TempDir(), StateDir: t.TempDir()})
	defer a.close()
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{"POST", "/v1/enginex/ui?source_unknown=1", 400},
		{"POST", "/api/ui?source_deezer=x", 400},
		{"POST", "/api/ui?remoteLoad=1", 403},
		{"POST", "/v1/enginex/ui?scope=spotify", 409},
		{"GET", "/v1/enginex/login?provider=deezer", 404},
		{"POST", "/v1/enginex/login?provider=invalid", 400},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			r, e := a.call(tc.method, tc.path, "")
			if e != nil || r.Code != tc.code {
				t.Fatalf("fixture returned %d (%v), want %d", r.Code, e, tc.code)
			}
			if r.Rev <= 0 || r.HeaderRev != r.Rev {
				t.Errorf("HTTP %d has body rev %d and header rev %d", r.Code, r.Rev, r.HeaderRev)
			}
			if r.Epoch == "" || r.HeaderEpoch != r.Epoch {
				t.Errorf("HTTP %d has body epoch %q and header epoch %q", r.Code, r.Epoch, r.HeaderEpoch)
			}
		})
	}
	prev := a.must(t, "GET", "/v1/enginex/ui", "")
	for _, provider := range []string{"deezer", "webdav", "nas"} {
		start := a.must(t, "POST", "/v1/enginex/login?provider="+provider, "")
		next := a.must(t, "GET", "/v1/enginex/login?provider="+provider, "")
		if start.Rev < prev.Rev || start.HeaderRev != start.Rev || start.Epoch == "" || start.HeaderEpoch != start.Epoch {
			t.Errorf("%s login start bad metadata: rev=%d/%d epoch=%q/%q", provider, start.Rev, start.HeaderRev, start.Epoch, start.HeaderEpoch)
		}
		if next.Rev < start.Rev || next.HeaderRev != next.Rev || next.Epoch != start.Epoch || next.HeaderEpoch != next.Epoch {
			t.Errorf("%s login status bad metadata: rev=%d/%d epoch=%q/%q", provider, next.Rev, next.HeaderRev, next.Epoch, next.HeaderEpoch)
		}
	}
}

// TestE4FinalWire verifies R15 Pillar 2 + pre-commit GET linearizability over live HTTP.
func TestE4FinalWire(t *testing.T) {
	a := e4FinalStart(Config{UIDir: t.TempDir(), StateDir: t.TempDir()})
	defer a.close()
	for _, add := range []bool{true, false} {
		name := "remove"
		if add {
			name = "add"
		}
		t.Run("peer_"+name, func(t *testing.T) {
			initial, peer := "tidal,spotify,deezer", "tidal,spotify"
			if add {
				initial, peer = peer, initial
			}
			a.must(t, "POST", "/v1/enginex/ui?sources="+initial, "")
			before := a.must(t, "POST", "/api/ui?sources="+peer, "")
			done := a.must(t, "POST", "/v1/enginex/ui?source_soundcloud=1", "")
			state := a.must(t, "GET", "/v1/enginex/ui", "")
			if slices.Contains(state.Sources, "deezer") != add || !slices.Contains(state.Sources, "soundcloud") {
				t.Errorf("delta write clobbered peer membership; Deezer=%v sources=%v", slices.Contains(state.Sources, "deezer"), state.Sources)
			}
			if before.Rev <= 0 || done.Rev <= before.Rev || done.Rev != done.HeaderRev || state.Rev != done.Rev || done.Digest == "" || done.Digest != state.Digest || done.Epoch == "" || done.Epoch != state.Epoch {
				t.Errorf("wire revision/epoch/digest mismatch: %d/%d/%d epoch=%q/%q", before.Rev, done.Rev, state.Rev, done.Epoch, state.Epoch)
			}
		})
	}
	t.Run("Precommit", func(t *testing.T) {
		a.must(t, "POST", "/v1/enginex/ui?sources=tidal", "")
		entered, release := make(chan struct{}), make(chan struct{})
		g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "POST" {
				close(entered)
				<-release
			}
			a.s.ServeHTTP(w, r)
		}))
		defer g.Close()
		gated := e4FinalAPI{a.s, g}
		ch := make(chan e4FinalResponse, 1)
		go func() { x, _ := gated.call("POST", "/v1/enginex/ui?source_deezer=1", ""); ch <- x }()
		<-entered
		old, e := gated.call("GET", "/v1/enginex/ui", "")
		close(release)
		newer := <-ch
		if e != nil || old.Rev == 0 || old.Rev >= newer.Rev || slices.Contains(old.Sources, "deezer") || !slices.Contains(newer.Sources, "deezer") {
			t.Fatalf("pre-commit GET %d and committed POST %d", old.Rev, newer.Rev)
		}
	})
}

// TestE4FinalMergedRaw verifies explicit featuresOff / shadowPausedFollow fields, digest, rev, and epoch
// across /v1/enginex/ui and /api/ui together with shadow-grid state.
func TestE4FinalMergedRaw(t *testing.T) {
	dir := t.TempDir()
	state, disabled, alive := hookFeatureStatePath, hookDisablePath, engineAlive
	t.Cleanup(func() { hookFeatureStatePath, hookDisablePath, engineAlive = state, disabled, alive })
	hookFeatureStatePath, hookDisablePath, engineAlive = filepath.Join(dir, "features"), filepath.Join(dir, "disabled"), func(pid int) bool { return pid == 4242 }
	a := e4FinalStart(Config{UIDir: t.TempDir(), StateDir: t.TempDir()})
	defer a.close()
	var last int64
	for i, on := range []bool{true, false, true} {
		off, follow := "", ""
		if on {
			off, follow = "stems", "shadow-paused-follow"
		}
		if err := os.WriteFile(hookFeatureStatePath, []byte(fmt.Sprintf("pid=4242\nbuild=stems,shadow-paused-follow\noff=%s\non=%s\n", off, follow)), 0600); err != nil {
			t.Fatal(err)
		}
		ack := a.must(t, "POST", "/v1/enginex/ui?autoGain="+strconv.Itoa(i%2), "")
		get := a.must(t, "GET", "/v1/enginex/ui", "")
		api := a.must(t, "GET", "/api/ui", "")
		features := "[]"
		if on {
			features = `["stems"]`
		}
		for _, r := range []e4FinalResponse{get, ack} {
			if string(r.Body["featuresOff"]) != features || string(r.Body["shadowPausedFollow"]) != strconv.FormatBool(on) || r.HeaderRev != r.Rev || r.Rev <= last || r.Epoch == "" || r.HeaderEpoch != r.Epoch {
				t.Errorf("explicit fields or revision/epoch failed at phase %d", i)
			}
		}
		if ack.Digest == "" || get.Digest != ack.Digest || api.Rev != ack.Rev || api.HeaderRev != api.Rev || api.Epoch != ack.Epoch || api.HeaderEpoch != api.Epoch {
			t.Error("digest or cross-route revision/epoch mismatch")
		}
		last = get.Rev
	}
}

// TestE4FinalProviderTransitions verifies TIDAL, Deezer, NetEase, Spotify, and Apple Music
// sign-in and sign-out transitions each advance rev atomically with consistent rev/epoch headers.
func TestE4FinalProviderTransitions(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/qr/key":
			fmt.Fprint(w, `{"code":200,"data":{"unikey":"fixture"}}`)
		case "/login/qr/create":
			fmt.Fprint(w, `{"code":200,"data":{"qrurl":"fixture"}}`)
		case "/login/qr/check":
			fmt.Fprint(w, `{"code":803,"cookie":"fixture"}`)
		case "/login/status":
			fmt.Fprint(w, `{"code":200,"data":{"profile":{"userId":42}}}`)
		default:
			fmt.Fprint(w, `{"data":[{"id":"fixture","attributes":{"name":"fixture"}}]}`)
		}
	}))
	defer stub.Close()
	cfg := Config{UIDir: t.TempDir(), StateDir: t.TempDir(), NeteaseURL: stub.URL, SpotifyAccountsURL: stub.URL, AppleMusicURL: stub.URL, AppleMusicWebURL: stub.URL, HTTP: stub.Client()}
	a := e4FinalStart(cfg)
	defer a.close()
	check := func(id string, before e4FinalResponse, want bool) e4FinalResponse {
		t.Helper()
		after := a.must(t, "GET", "/v1/enginex/ui", "")
		member := after.has(id)
		if id == "tidal" {
			member = string(after.Body["tidalLoggedIn"]) == "true"
		}
		if member != want || after.Rev <= before.Rev || after.Rev != after.HeaderRev || after.Epoch == "" || after.Epoch != after.HeaderEpoch {
			t.Errorf("%s transition lacks a distinct revision: member=%v (want %v) rev=%d->%d hdr=%d epoch=%q/%q",
				id, member, want, before.Rev, after.Rev, after.HeaderRev, after.Epoch, after.HeaderEpoch)
		}
		return after
	}
	prev := a.must(t, "GET", "/v1/enginex/ui", "")
	if e := a.s.tidal.saveRealAuth(tidalAuth{AccessToken: "qa-state"}); e != nil {
		t.Fatal(e)
	}
	prev = check("tidal", prev, true)
	if e := a.s.tidal.clearRealAuth(); e != nil {
		t.Fatal(e)
	}
	prev = check("tidal", prev, false)
	for _, id := range []string{"deezer", "netease", "spotify", "applemusic"} {
		t.Run(id, func(t *testing.T) {
			before := a.must(t, "GET", "/v1/enginex/ui", "")
			session := a.must(t, "POST", "/api/sessions", `{"provider":"`+id+`"}`)
			var key string
			_ = json.Unmarshal(session.Body["id"], &key)
			a.s.mu.Lock()
			ss := a.s.sessions[key]
			a.s.mu.Unlock()
			if ss == nil {
				t.Fatal("missing test session")
			}
			switch id {
			case "deezer":
				if e := a.s.saveDeezerOwner(ss.p.(*providers.Deezer), deezerOwner{UserID: "42", ARL: "fixture"}); e != nil {
					t.Fatal(e)
				}
			case "netease":
				ss.qr.Key = "fixture"
				a.must(t, "POST", "/api/sessions/"+key+"/poll", "")
			case "spotify":
				if e := ss.p.(*providers.Spotify).SetToken(providers.Token{AccessToken: "qa-state", Expiry: time.Now().Add(time.Hour)}); e != nil {
					t.Fatal(e)
				}
				a.must(t, "POST", "/api/sessions", `{"provider":"spotify"}`)
			case "applemusic":
				gen := a.s.appleMusic.CredGen()
				if e := a.s.appleMusic.SetOwner(context.Background(), providers.AppleMusicOwner{DevToken: "fixture", Links: []string{"https://music.apple.com/us/playlist/test/pl.fixture"}}); e != nil {
					t.Fatal(e)
				}
				a.s.syncAppleMusicSession(true, gen)
			}
			before = check(id, before, true)
			req, _ := http.NewRequest("DELETE", a.h.URL+"/api/sessions/"+key, nil)
			req.AddCookie(&http.Cookie{Name: "stream_sync", Value: a.s.AccessToken()})
			resp, e := a.h.Client().Do(req)
			if e != nil {
				t.Fatal(e)
			}
			resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				t.Fatalf("delete returned %d", resp.StatusCode)
			}
			check(id, before, false)
		})
	}
}
