package webui

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

func setupLinkOnDemandStick(t *testing.T, volName string, trackCount int) (mediaRoot, stick, libUUID string, tr *rbstate.Track, importKey int64) {
	t.Helper()
	ctx := context.Background()
	mediaRoot = t.TempDir()
	stick = filepath.Join(mediaRoot, volName)
	if err := os.MkdirAll(stick, 0o755); err != nil {
		t.Fatal(err)
	}
	testfixture.LargeExport(t, stick, trackCount)
	if _, err := bridge.ConvertAtMount(ctx, stick, bridge.MountOptions{}); err != nil {
		t.Fatalf("ConvertAtMount: %v", err)
	}
	dbDir := filepath.Join(stick, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, cand := range snap.Tracks {
		if cand.EngineID == 1 {
			tr = cand
			break
		}
	}
	if tr == nil {
		t.Fatal("missing track 1")
	}
	mdb := filepath.Join(dbDir, "m.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mdb}).String()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT COALESCE(pdbImportKey, 0) FROM Track WHERE id = ?`, tr.EngineID).Scan(&importKey); err != nil {
		t.Fatal(err)
	}
	if importKey <= 0 {
		t.Fatalf("expected positive pdbImportKey for track 1, got %d", importKey)
	}
	return mediaRoot, stick, snap.LibraryUUID, tr, importKey
}

func TestRekordboxLinkOnDemand4DeckSingleFlightAndFilledJournal(t *testing.T) {
	mediaRoot, stick, libUUID, tr, importKey := setupLinkOnDemandStick(t, "NEXTLIB", 2)
	stageRoot := t.TempDir()
	t.Setenv(rbstate.StageEnv, stageRoot)

	// Unit A owns the mounted stick.
	srvA := New(Config{
		StateDir:    t.TempDir(),
		MediaRoot:   mediaRoot,
		RBStageRoot: stageRoot,
	}, nil)
	defer srvA.Close()
	tsA := httptest.NewServer(srvA)
	defer tsA.Close()
	uA, _ := url.Parse(tsA.URL)
	hostA, portAStr, _ := net.SplitHostPort(uA.Host)
	portA := 0
	for _, c := range portAStr {
		portA = portA*10 + int(c-'0')
	}

	// Unit B has no stick mounted locally, knows Unit A via 4-deck shadow peerDecks.
	sockB := filepath.Join(t.TempDir(), "rb-ondemand.sock")
	srvB := New(Config{
		StateDir:              t.TempDir(),
		MediaRoot:             t.TempDir(),
		RekordboxOnDemandSock: sockB,
	}, nil)
	defer srvB.Close()
	srvB.shadow.mu.Lock()
	srvB.shadow.peerPort = portA
	srvB.shadow.peers[hostA] = time.Now()
	srvB.shadow.peerDecks[1] = peerDeck{
		deckState: deckState{Deck: 1, Link: "{AAAA0001-1111-2222-3333-444455556666}"},
		ip:        hostA,
		at:        time.Now(),
	}
	srvB.shadow.mu.Unlock()

	netDB := "net://aaaa0001-1111-2222-3333-444455556666/NEXTLIB/Engine Library/Database2/m.db"
	key0 := 0
	reqBody, _ := json.Marshal(rbOnDemandRequest{
		DB:        netDB,
		UUID:      libUUID,
		Path:      "../" + tr.Rel,
		ID:        tr.EngineID,
		ImportKey: importKey,
		Key:       &key0,
		BPM:       128.0,
	})

	// 1. Unit B's hook queries Unix socket for net:// track -> Unit A builds once and replies RBXA1.
	unixClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockB)
			},
		},
	}
	resp, err := unixClient.Post("http://unix"+RekordboxOnDemandRoute, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("B unix track-data: %v", err)
	}
	bodyB, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("B status=%d body=%s", resp.StatusCode, string(bodyB))
	}
	assertValidOnDemandResponse(t, resp.Header, bodyB, tr)
	if got := srvB.rbLinkPeerHTTPCount.Load(); got != 1 {
		t.Fatalf("B peer HTTP count = %d, want 1", got)
	}
	if got := srvA.rbOnDemandBuildCount.Load(); got != 1 {
		t.Fatalf("A build count = %d, want 1", got)
	}

	// 2. Unit A's own local load of the same track hits the shared cache (build count remains 1).
	localPayload, _ := json.Marshal(rbOnDemandRequest{
		DB:   filepath.Join(stick, "Engine Library", "Database2", "m.db"),
		UUID: libUUID,
		Path: "../" + tr.Rel,
		ID:   tr.EngineID,
		Key:  &key0,
		BPM:  128.0,
	})
	reqLocal := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(localPayload))
	reqLocal.RemoteAddr = "127.0.0.1:12345"
	recLocal := httptest.NewRecorder()
	srvA.ServeHTTP(recLocal, reqLocal)
	if recLocal.Code != http.StatusOK {
		t.Fatalf("A local status=%d body=%s", recLocal.Code, recLocal.Body.String())
	}
	if !bytes.Equal(recLocal.Body.Bytes(), bodyB) {
		t.Fatal("B remote RBXA1 differs from A local RBXA1")
	}
	if got := srvA.rbOnDemandBuildCount.Load(); got != 1 {
		t.Fatalf("A local load did not reuse cache shared with B: buildCount=%d, want 1", got)
	}

	// 3. Unit B notifies track-filled -> 202 immediately -> Unit A appends filled.jsonl with verified digests.
	filledBody, _ := json.Marshal(rbOnDemandRequest{
		DB:        netDB,
		UUID:      libUUID,
		Path:      "../" + tr.Rel,
		ID:        tr.EngineID,
		ImportKey: importKey,
	})
	fResp, err := unixClient.Post("http://unix"+RekordboxOnDemandFilledRoute, "application/json", bytes.NewReader(filledBody))
	if err != nil {
		t.Fatalf("B unix track-filled: %v", err)
	}
	fResp.Body.Close()
	if fResp.StatusCode != http.StatusAccepted {
		t.Fatalf("B track-filled status=%d, want 202", fResp.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	for srvA.rbLinkFilledCount.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := srvA.rbLinkFilledCount.Load(); got != 1 {
		t.Fatalf("A filled count = %d, want 1", got)
	}

	// Verify filled.jsonl parses via rbstate.Stage.Filled() and carries complete #321 fields.
	st, err := rbstate.OpenStage(libUUID)
	if err != nil {
		t.Fatalf("OpenStage: %v", err)
	}
	filled := st.Filled()
	entry, ok := filled[tr.Key]
	if !ok {
		t.Fatalf("filled.jsonl missing key %q: got %+v", tr.Key, filled)
	}
	wantWR, _ := base64.StdEncoding.DecodeString(resp.Header.Get("X-RB-Written"))
	if entry.EngineID != tr.EngineID || !bytes.Equal(entry.Written, wantWR) {
		t.Fatalf("filled.jsonl mismatch: got %+v want id=%d wr=%x", entry, tr.EngineID, wantWR)
	}
	rawJournal, err := os.ReadFile(filepath.Join(stageRoot, libUUID, "filled.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var rawLine map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(rawJournal), &rawLine); err != nil {
		t.Fatalf("invalid filled.jsonl JSON: %v", err)
	}
	for _, field := range []string{"id", "k", "wr", "wrnm", "al", "v", "w", "at"} {
		if _, exists := rawLine[field]; !exists {
			t.Fatalf("filled.jsonl missing field %q: %s", field, string(rawJournal))
		}
	}
}

func TestRekordboxLinkOnDemand2DeckShadowOffFallback(t *testing.T) {
	mediaRoot, _, libUUID, tr, importKey := setupLinkOnDemandStick(t, "STICK2D", 1)
	srvA := New(Config{StateDir: t.TempDir(), MediaRoot: mediaRoot, RBStageRoot: t.TempDir()}, nil)
	defer srvA.Close()
	tsA := httptest.NewServer(srvA)
	defer tsA.Close()
	uA, _ := url.Parse(tsA.URL)

	// Unit B has shadow OFF (zero shadow.peers / shadow.peerDecks): uses on-demand peer candidates.
	srvB := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB.Close()
	var candCalls atomic.Int32
	srvB.rbLinkPeerCandidates = func() []string {
		candCalls.Add(1)
		return []string{uA.Host}
	}

	netDB := "net://bbbb0002-1111-2222-3333-444455556666/STICK2D/Engine Library/Database2/m.db"
	payload, _ := json.Marshal(rbOnDemandRequest{
		DB:        netDB,
		UUID:      libUUID,
		Path:      "../" + tr.Rel,
		ID:        tr.EngineID,
		ImportKey: importKey,
	})
	for call := 1; call <= 2; call++ {
		req := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(payload))
		req.RemoteAddr = "127.0.0.1:43210"
		rec := httptest.NewRecorder()
		srvB.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("2-deck call %d status=%d body=%s", call, rec.Code, rec.Body.String())
		}
		assertValidOnDemandResponse(t, rec.Header(), rec.Body.Bytes(), tr)
	}
	if got := candCalls.Load(); got != 1 {
		t.Fatalf("2-deck candidate scan called %d times, want 1 (cached after first hit)", got)
	}
}

func TestRekordboxLinkOnDemandIdentityAndSecurity(t *testing.T) {
	mediaRoot, stick, libUUID, tr, importKey := setupLinkOnDemandStick(t, "SECUSB", 1)
	stageRoot := t.TempDir()

	// Insert a stock Engine row (pdbImportKey IS NULL) on the same stick.
	mdb := filepath.Join(stick, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mdb}).String())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO Track(path, filename, pdbImportKey, isAnalyzed) VALUES('../Contents/StockEngine.mp3', 'StockEngine.mp3', NULL, 0)`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	srvA := New(Config{StateDir: t.TempDir(), MediaRoot: mediaRoot, RBStageRoot: stageRoot}, nil)
	defer srvA.Close()

	postPeer := func(route string, body any, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, route, bytes.NewReader(raw))
		req.RemoteAddr = remoteAddr
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		srvA.ServeHTTP(rec, req)
		return rec
	}

	// 1. Path alone without UUID -> 400.
	if rec := postPeer(RekordboxPeerOnDemandRoute, rbPeerOnDemandRequest{Path: "../" + tr.Rel, ImportKey: importKey}, "192.168.1.20:9999", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing UUID: status=%d, want 400", rec.Code)
	}
	// 2. Missing/zero importKey -> 400.
	if rec := postPeer(RekordboxPeerOnDemandRoute, rbPeerOnDemandRequest{UUID: libUUID, Path: "../" + tr.Rel, ImportKey: 0}, "192.168.1.20:9999", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("zero importKey: status=%d, want 400", rec.Code)
	}
	// 3. Wrong UUID -> 404 with X-RB-Status: not_found.
	if rec := postPeer(RekordboxPeerOnDemandRoute, rbPeerOnDemandRequest{UUID: "wrong-uuid", VolHint: "SECUSB", Path: "../" + tr.Rel, ImportKey: importKey}, "192.168.1.20:9999", nil); rec.Code != http.StatusNotFound || rec.Header().Get("X-RB-Status") != "not_found" {
		t.Fatalf("wrong UUID: status=%d hdr=%q, want 404/not_found", rec.Code, rec.Header().Get("X-RB-Status"))
	}
	// 4. Right UUID, wrong importKey -> 404 with X-RB-Status: not_found.
	if rec := postPeer(RekordboxPeerOnDemandRoute, rbPeerOnDemandRequest{UUID: libUUID, VolHint: "SECUSB", Path: "../" + tr.Rel, ImportKey: importKey + 999}, "192.168.1.20:9999", nil); rec.Code != http.StatusNotFound || rec.Header().Get("X-RB-Status") != "not_found" {
		t.Fatalf("wrong importKey: status=%d hdr=%q, want 404/not_found", rec.Code, rec.Header().Get("X-RB-Status"))
	}
	// 5. Stock Engine row (NULL pdbImportKey) -> 404.
	if rec := postPeer(RekordboxPeerOnDemandRoute, rbPeerOnDemandRequest{UUID: libUUID, VolHint: "SECUSB", Path: "../Contents/StockEngine.mp3", ImportKey: 1}, "192.168.1.20:9999", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("stock row: status=%d, want 404", rec.Code)
	}
	// 6. Non-LAN public IP -> 403.
	if rec := postPeer(RekordboxPeerOnDemandRoute, rbPeerOnDemandRequest{UUID: libUUID, Path: "../" + tr.Rel, ImportKey: importKey}, "8.8.8.8:1234", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("public IP: status=%d, want 403", rec.Code)
	}
	// 7. External relay header -> 404 (lanOnly guard in ServeHTTP).
	if rec := postPeer(RekordboxPeerOnDemandRoute, rbPeerOnDemandRequest{UUID: libUUID, Path: "../" + tr.Rel, ImportKey: importKey}, "192.168.1.20:1234", map[string]string{"Cf-Connecting-Ip": "1.2.3.4"}); rec.Code != http.StatusNotFound {
		t.Fatalf("relayed request: status=%d, want 404", rec.Code)
	}
	// 8. ForwardHandler hides both peer routes (404).
	for _, route := range []string{RekordboxPeerOnDemandRoute, RekordboxPeerFilledRoute} {
		req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(`{}`))
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		srvA.ForwardHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("ForwardHandler %s: status=%d, want 404", route, rec.Code)
		}
	}
	// 9. Forged /peer/rekordbox/track-filled with wrong importKey must fail and write NO filled.jsonl.
	if rec := postPeer(RekordboxPeerFilledRoute, rbPeerFilledRequest{UUID: libUUID, Path: "../" + tr.Rel, ID: tr.EngineID, ImportKey: importKey + 1}, "192.168.1.20:9999", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("forged track-filled: status=%d, want 404", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(stageRoot, libUUID, "filled.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("forged track-filled created filled.jsonl: err=%v", err)
	}
	// 10. Valid peer request with matching UUID + importKey succeeds even without VolHint.
	if rec := postPeer(RekordboxPeerOnDemandRoute, rbPeerOnDemandRequest{UUID: libUUID, Path: "../" + tr.Rel, ID: tr.EngineID, ImportKey: importKey}, "192.168.1.20:9999", nil); rec.Code != http.StatusOK {
		t.Fatalf("valid peer request without VolHint: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRekordboxLinkOnDemandOlderAndNonEngineXPeerNegativeCache(t *testing.T) {
	// Older EngineX peer returns plain 404 without X-RB-Status.
	var olderHits atomic.Int32
	olderPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		olderHits.Add(1)
		http.NotFound(w, r)
	}))
	defer olderPeer.Close()
	uOlder, _ := url.Parse(olderPeer.URL)

	// Non-EngineX peer: closed TCP port (ECONNREFUSED).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := ln.Addr().String()
	ln.Close()

	srvB := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB.Close()

	call := func(peerUUID string, ep string) (int, string) {
		srvB.rbLinkPeerCandidates = func() []string { return []string{ep} }
		body, _ := json.Marshal(rbOnDemandRequest{
			DB:        "net://" + peerUUID + "/USB1/Engine Library/Database2/m.db",
			UUID:      "lib-old-1",
			Path:      "../Contents/A.mp3",
			ID:        1,
			ImportKey: 42,
		})
		req := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:4444"
		rec := httptest.NewRecorder()
		srvB.ServeHTTP(rec, req)
		return rec.Code, rec.Header().Get("X-RB-Status")
	}

	// Older EngineX peer: 1st call probes once, 2nd call hits negative cache with 0 LAN requests.
	code, st := call("cccc0001-1111-2222-3333-444455556666", uOlder.Host)
	if code != http.StatusNotFound || st != "peer_unsupported" {
		t.Fatalf("older peer 1st call = (%d, %q), want (404, peer_unsupported)", code, st)
	}
	if got := srvB.rbLinkPeerHTTPCount.Load(); got != 1 || olderHits.Load() != 1 {
		t.Fatalf("older peer 1st hits: client=%d server=%d, want 1/1", got, olderHits.Load())
	}
	code, st = call("cccc0001-1111-2222-3333-444455556666", uOlder.Host)
	if code != http.StatusNotFound || st != "peer_unsupported" {
		t.Fatalf("older peer 2nd call = (%d, %q), want (404, peer_unsupported)", code, st)
	}
	if got := srvB.rbLinkPeerHTTPCount.Load(); got != 1 || olderHits.Load() != 1 {
		t.Fatalf("older peer 2nd call was not zero-cost cached: client=%d server=%d, want 1/1", got, olderHits.Load())
	}

	// Non-EngineX peer (ECONNREFUSED): 1st call probes once, 2nd call does 0 LAN requests.
	code, st = call("cccc0002-1111-2222-3333-444455556666", closedAddr)
	if code != http.StatusNotFound || st != "peer_unsupported" {
		t.Fatalf("non-EngineX 1st call = (%d, %q), want (404, peer_unsupported)", code, st)
	}
	if got := srvB.rbLinkPeerHTTPCount.Load(); got != 2 {
		t.Fatalf("non-EngineX 1st count = %d, want 2", got)
	}
	code, st = call("cccc0002-1111-2222-3333-444455556666", closedAddr)
	if code != http.StatusNotFound || st != "peer_unsupported" {
		t.Fatalf("non-EngineX 2nd call = (%d, %q), want (404, peer_unsupported)", code, st)
	}
	if got := srvB.rbLinkPeerHTTPCount.Load(); got != 2 {
		t.Fatalf("non-EngineX 2nd call was not zero-cost cached: count=%d, want 2", got)
	}
}

func TestRekordboxLinkOnDemandAnswerBudgetTimeoutAndNoRedirect(t *testing.T) {
	// 1. Slow/unresponsive peer aborts within rbLinkAnswerBudget (504 timeout) without permanent neg cache.
	slowPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(600 * time.Millisecond):
		case <-r.Context().Done():
		}
	}))
	defer slowPeer.Close()
	uSlow, _ := url.Parse(slowPeer.URL)

	srvB := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB.Close()
	srvB.rbLinkAnswerBudget = 120 * time.Millisecond
	srvB.rbLinkPeerCandidates = func() []string { return []string{uSlow.Host} }

	body, _ := json.Marshal(rbOnDemandRequest{
		DB:        "net://dddd0001-1111-2222-3333-444455556666/USB1/Engine Library/Database2/m.db",
		UUID:      "lib-slow-1",
		Path:      "../Contents/Slow.mp3",
		ID:        1,
		ImportKey: 55,
	})
	t0 := time.Now()
	req := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	srvB.ServeHTTP(rec, req)
	elapsed := time.Since(t0)
	if rec.Code != http.StatusGatewayTimeout || rec.Header().Get("X-RB-Status") != "timeout" {
		t.Fatalf("slow peer = (%d, %q), want (504, timeout)", rec.Code, rec.Header().Get("X-RB-Status"))
	}
	if elapsed < 90*time.Millisecond || elapsed > 320*time.Millisecond {
		t.Fatalf("slow peer elapsed = %v, want ~120 ms (< 320 ms)", elapsed)
	}
	if srvB.isLinkPeerNegCached("dddd0001-1111-2222-3333-444455556666", time.Now()) {
		t.Fatal("timeout must not permanently negative-cache peer as unsupported")
	}

	// 2. Peer returning 302 redirect must NOT be followed.
	var redirTargetHits atomic.Int32
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirTargetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer targetSrv.Close()
	redirPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetSrv.URL, http.StatusFound)
	}))
	defer redirPeer.Close()
	uRedir, _ := url.Parse(redirPeer.URL)

	srvB.rbLinkPeerCandidates = func() []string { return []string{uRedir.Host} }
	req2 := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(body))
	req2.RemoteAddr = "127.0.0.1:5555"
	rec2 := httptest.NewRecorder()
	srvB.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusOK || redirTargetHits.Load() != 0 {
		t.Fatalf("redirect followed: code=%d targetHits=%d", rec2.Code, redirTargetHits.Load())
	}
}

func TestRekordboxLinkOnDemandConcurrentTwoUnits(t *testing.T) {
	mediaRoot, _, libUUID, tr, importKey := setupLinkOnDemandStick(t, "CONCUSB", 2)
	stageRoot := t.TempDir()
	srvA := New(Config{StateDir: t.TempDir(), MediaRoot: mediaRoot, RBStageRoot: stageRoot}, nil)
	defer srvA.Close()
	tsA := httptest.NewServer(srvA)
	defer tsA.Close()
	uA, _ := url.Parse(tsA.URL)

	srvB1 := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB1.Close()
	srvB1.rbLinkPeerCandidates = func() []string { return []string{uA.Host} }

	srvB2 := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB2.Close()
	srvB2.rbLinkPeerCandidates = func() []string { return []string{uA.Host} }

	netDB := "net://eeee0001-1111-2222-3333-444455556666/CONCUSB/Engine Library/Database2/m.db"
	payload, _ := json.Marshal(rbOnDemandRequest{
		DB:        netDB,
		UUID:      libUUID,
		Path:      "../" + tr.Rel,
		ID:        tr.EngineID,
		ImportKey: importKey,
	})

	const workers = 6
	bodies := make([][]byte, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		srv := srvB1
		if i%2 == 1 {
			srv = srvB2
		}
		go func(idx int, s *Server) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(payload))
			req.RemoteAddr = "127.0.0.1:6000"
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				errs[idx] = io.ErrUnexpectedEOF
				return
			}
			bodies[idx] = append([]byte(nil), rec.Body.Bytes()...)
			// Also notify track-filled concurrently.
			fReq := httptest.NewRequest(http.MethodPost, RekordboxOnDemandFilledRoute, bytes.NewReader(payload))
			fReq.RemoteAddr = "127.0.0.1:6000"
			fRec := httptest.NewRecorder()
			s.ServeHTTP(fRec, fReq)
		}(i, srv)
	}
	close(start)
	wg.Wait()

	for i := range workers {
		if errs[i] != nil || len(bodies[i]) < 5 || !bytes.Equal(bodies[i], bodies[0]) {
			t.Fatalf("worker %d failed or returned different RBXA1: err=%v len=%d", i, errs[i], len(bodies[i]))
		}
	}
	if got := srvA.rbOnDemandBuildCount.Load(); got != 1 {
		t.Fatalf("concurrent 2-unit load built %d times, want 1 (single-flight)", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for srvA.rbLinkFilledCount.Load() < workers && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	raw, err := os.ReadFile(filepath.Join(stageRoot, libUUID, "filled.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	if len(lines) < workers {
		t.Fatalf("filled.jsonl lines = %d, want >= %d", len(lines), workers)
	}
	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("corrupted filled.jsonl line %d: %v (%s)", i, err, string(line))
		}
	}
}

func TestParseNetDBPathAndProcNetTCP(t *testing.T) {
	uuid, vol, ok := parseNetDBPath("net://{AABBCCDD-1122-3344-5566-778899AABBCC}/MY USB/Engine Library/Database2/m.db")
	if !ok || uuid != "aabbccdd-1122-3344-5566-778899aabbcc" || vol != "MY USB" {
		t.Fatalf("parseNetDBPath valid = (%q, %q, %v)", uuid, vol, ok)
	}
	for _, bad := range []string{
		"net://bad_host/USB/Engine Library/Database2/m.db",
		"net://1111/../Engine Library/Database2/m.db",
		"net://1111/a/b/Engine Library/Database2/m.db",
		"net://1111/USB/Engine Library/Database2/sm.db",
		"/media/USB/Engine Library/Database2/m.db",
	} {
		if _, _, ok := parseNetDBPath(bad); ok {
			t.Fatalf("parseNetDBPath accepted invalid path %q", bad)
		}
	}

	// /proc/net/tcp little-endian IPv4:
	//   10.99.0.1     -> 0100630A (ESTABLISHED 01, keep)
	//   169.254.12.34 -> 220CFEA9 (ESTABLISHED 01, keep)
	//   127.0.0.1     -> 0100007F (loopback, skip)
	//   8.8.8.8       -> 08080808 (public, skip)
	//   10.99.0.2     -> 0200630A (own IP, skip)
	//   192.168.1.9   -> 0901A8C0 (LISTEN 0A, skip)
	sample := strings.Join([]string{
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode",
		"   0: 0200630A:C350 0100630A:1F90 01 00000000:00000000 00:00000000 00000000     0        0 100",
		"   1: 0200630A:C351 0100630A:C352 01 00000000:00000000 00:00000000 00000000     0        0 101",
		"   2: 0200630A:C352 0100007F:1F90 01 00000000:00000000 00:00000000 00000000     0        0 102",
		"   3: 0200630A:C353 08080808:01BB 01 00000000:00000000 00:00000000 00000000     0        0 103",
		"   4: 0200630A:C354 0200630A:1F90 01 00000000:00000000 00:00000000 00000000     0        0 104",
		"   5: 00000000:1F90 0901A8C0:0000 0A 00000000:00000000 00:00000000 00000000     0        0 105",
		"   6: 0200630A:C355 220CFEA9:C350 01 00000000:00000000 00:00000000 00000000     0        0 106",
	}, "\n")
	got := parseProcNetTCPLANPeers(sample, map[string]bool{"10.99.0.2": true}, 4)
	if len(got) != 2 || got[0] != "10.99.0.1" || got[1] != "169.254.12.34" {
		t.Fatalf("parseProcNetTCPLANPeers = %v, want [10.99.0.1 169.254.12.34]", got)
	}

	// /proc/net/tcp6 IPv4-mapped ::ffff:10.99.0.1 (Qt dual-stack socket) + pure IPv6 skipped.
	sample6 := strings.Join([]string{
		"  sl  local_address                         remote_address                        st",
		"   0: 0000000000000000FFFF00000200630A:C350 0000000000000000FFFF00000100630A:C351 01",
		"   1: 00000000000000000000000000000001:C350 00000000000000000000000000000001:C351 01",
		"   2: 0000000000000000FFFF00000200630A:C350 0000000000000000FFFF00000100007F:C351 01",
	}, "\n")
	got6 := parseProcNetTCPLANPeers(sample6, map[string]bool{"10.99.0.2": true}, 4)
	if len(got6) != 1 || got6[0] != "10.99.0.1" {
		t.Fatalf("parseProcNetTCPLANPeers(tcp6) = %v, want [10.99.0.1]", got6)
	}

	// /proc/net/arp complete (0x2) private/link-local fallback.
	arp := strings.Join([]string{
		"IP address       HW type     Flags       HW address            Mask     Device",
		"10.99.0.1        0x1         0x2         aa:bb:cc:dd:ee:01     *        eth0",
		"10.99.0.2        0x1         0x2         aa:bb:cc:dd:ee:02     *        eth0",
		"192.168.1.50     0x1         0x0         00:00:00:00:00:00     *        eth0",
		"8.8.8.8          0x1         0x2         aa:bb:cc:dd:ee:03     *        eth0",
	}, "\n")
	gotARP := parseProcNetARPLANPeers(arp, map[string]bool{"10.99.0.2": true}, 4)
	if len(gotARP) != 1 || gotARP[0] != "10.99.0.1" {
		t.Fatalf("parseProcNetARPLANPeers = %v, want [10.99.0.1]", gotARP)
	}
}
