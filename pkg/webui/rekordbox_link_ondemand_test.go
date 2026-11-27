package webui

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
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

// commitRBXA1ToStick simulates Unit B's SQLite hook (sc_rbs_fill_sql) committing an RBXA1 payload
// onto Unit A's m.db over Engine Link airNfs so Unit A can verify the committed row before journaling.
func commitRBXA1ToStick(t *testing.T, stick string, engineID int64, rbxa1 []byte) {
	t.Helper()
	dec, err := rbstate.DecodePerf(rbxa1)
	if err != nil {
		t.Fatalf("DecodePerf: %v", err)
	}
	mdb := filepath.Join(stick, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mdb}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=? WHERE trackId=?`,
		dec.TrackData, dec.Overview, dec.Beats, dec.QuickCues, dec.Loops, dec.ActiveLoops, engineID); err != nil {
		t.Fatalf("commit PerformanceData: %v", err)
	}
	if _, err := db.Exec(`UPDATE Track SET isAnalyzed = 1 WHERE id = ?`, engineID); err != nil {
		t.Fatalf("commit Track.isAnalyzed: %v", err)
	}
}

func TestRekordboxLinkOnDemand4DeckSingleFlightAndFilledJournal(t *testing.T) {
	mediaRoot, stick, libUUID, tr, importKey := setupLinkOnDemandStick(t, "NEXTLIB", 2)
	stageRoot := t.TempDir()
	t.Setenv(rbstate.StageEnv, stageRoot)

	const ownerUUID = "aaaa0001-1111-2222-3333-444455556666"
	srvA := New(Config{
		StateDir:    t.TempDir(),
		MediaRoot:   mediaRoot,
		RBStageRoot: stageRoot,
		LinkUUID:    ownerUUID,
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
		deckState: deckState{Deck: 1, Link: "{" + strings.ToUpper(ownerUUID) + "}"},
		ip:        hostA,
		at:        time.Now(),
	}
	srvB.shadow.mu.Unlock()

	netDB := "net://" + ownerUUID + "/NEXTLIB/Engine Library/Database2/m.db"
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

	// 1. Unit B queries Unix socket for net:// track -> Unit A builds once and replies verified RBXA1.
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
	if got := resp.Header.Get("X-RB-UUID"); got != libUUID {
		t.Fatalf("X-RB-UUID = %q, want %q", got, libUUID)
	}
	if got := resp.Header.Get("X-RB-Owner"); got != ownerUUID {
		t.Fatalf("X-RB-Owner = %q, want %q", got, ownerUUID)
	}
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

	// 3. Commit the RBXA1 blobs into Unit A's m.db (as B's SQLite hook does), then notify track-filled.
	commitRBXA1ToStick(t, stick, tr.EngineID, bodyB)

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

// Fails on c76b2ede: stale caller BPM/Key/Length created a different single-flight key and built twice.
func TestRekordboxLinkOnDemandSingleFlightIgnoresCallerMetadata(t *testing.T) {
	mediaRoot, stick, libUUID, tr, importKey := setupLinkOnDemandStick(t, "SFLIGHT", 1)
	srvA := New(Config{StateDir: t.TempDir(), MediaRoot: mediaRoot, RBStageRoot: t.TempDir()}, nil)
	defer srvA.Close()

	// 1. Peer request with stale positive BPM (99.0) and stale Key/Length JSON fields (must be ignored).
	rawPeer1 := map[string]any{
		"uuid":      libUUID,
		"path":      "../" + tr.Rel,
		"id":        tr.EngineID,
		"importKey": importKey,
		"bpm":       99.0,
		"key":       11,
		"length":    777,
	}
	b1, _ := json.Marshal(rawPeer1)
	req1 := httptest.NewRequest(http.MethodPost, RekordboxPeerOnDemandRoute, bytes.NewReader(b1))
	req1.RemoteAddr = "192.168.1.20:5000"
	rec1 := httptest.NewRecorder()
	srvA.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("peer1 status=%d body=%s", rec1.Code, rec1.Body.String())
	}

	// 2. Direct forPeer=true build with a DIFFERENT stale positive BPM (140.5) must hit the exact same flight/cache.
	res2, err := srvA.buildOnDemandCachedWithImportKey(context.Background(), bridge.OnDemandRequest{
		Mount:      stick,
		UUID:       libUUID,
		EnginePath: "../" + tr.Rel,
		EngineID:   tr.EngineID,
		EngineKey:  5,
		EngineBPM:  140.5,
		EngineLen:  999,
	}, true, importKey)
	if err != nil {
		t.Fatalf("peer2 build: %v", err)
	}
	if !bytes.Equal(rec1.Body.Bytes(), res2.RBXA1) {
		t.Fatal("stale caller metadata changed RBXA1 payload")
	}

	// 3. Local load on Unit A with stale BPM also reuses the owner-row-keyed cache (build count stays 1).
	localBody, _ := json.Marshal(rbOnDemandRequest{
		DB:   filepath.Join(stick, "Engine Library", "Database2", "m.db"),
		UUID: libUUID,
		Path: "../" + tr.Rel,
		ID:   tr.EngineID,
		BPM:  75.0,
	})
	reqLocal := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(localBody))
	reqLocal.RemoteAddr = "127.0.0.1:5001"
	recLocal := httptest.NewRecorder()
	srvA.ServeHTTP(recLocal, reqLocal)
	if recLocal.Code != http.StatusOK {
		t.Fatalf("local status=%d body=%s", recLocal.Code, recLocal.Body.String())
	}
	if got := srvA.rbOnDemandBuildCount.Load(); got != 1 {
		t.Fatalf("same owner row built %d times with differing caller BPM, want 1", got)
	}
}

// Fails on c76b2ede: track-filled previously wrote filled.jsonl with isAnalyzed=0 and wrote duplicate lines.
func TestRekordboxLinkOnDemandFilledRequiresCommitAndDeduplicates(t *testing.T) {
	mediaRoot, stick, libUUID, tr, importKey := setupLinkOnDemandStick(t, "FILLVER", 1)
	stageRoot := t.TempDir()
	srvA := New(Config{StateDir: t.TempDir(), MediaRoot: mediaRoot, RBStageRoot: stageRoot}, nil)
	defer srvA.Close()

	// Build RBXA1 first.
	peerBody, _ := json.Marshal(rbPeerOnDemandRequest{
		UUID: libUUID, Path: "../" + tr.Rel, ID: tr.EngineID, ImportKey: importKey,
	})
	reqData := httptest.NewRequest(http.MethodPost, RekordboxPeerOnDemandRoute, bytes.NewReader(peerBody))
	reqData.RemoteAddr = "192.168.1.30:4000"
	recData := httptest.NewRecorder()
	srvA.ServeHTTP(recData, reqData)
	if recData.Code != http.StatusOK {
		t.Fatalf("track-data status=%d body=%s", recData.Code, recData.Body.String())
	}

	postFilled := func() *httptest.ResponseRecorder {
		raw, _ := json.Marshal(rbPeerFilledRequest{
			UUID: libUUID, Path: "../" + tr.Rel, ID: tr.EngineID, ImportKey: importKey,
		})
		req := httptest.NewRequest(http.MethodPost, RekordboxPeerFilledRoute, bytes.NewReader(raw))
		req.RemoteAddr = "192.168.1.30:4001"
		rec := httptest.NewRecorder()
		srvA.ServeHTTP(rec, req)
		return rec
	}

	jpath := filepath.Join(stageRoot, libUUID, "filled.jsonl")

	// 1. Uncommitted row (isAnalyzed=0): MUST reject (409 not_committed) and write NO filled.jsonl.
	if rec := postFilled(); rec.Code != http.StatusConflict || rec.Header().Get("X-RB-Status") != "not_committed" {
		t.Fatalf("uncommitted track-filled = (%d, %q), want (409, not_committed)", rec.Code, rec.Header().Get("X-RB-Status"))
	}
	if _, err := os.Stat(jpath); !os.IsNotExist(err) {
		t.Fatalf("uncommitted track-filled created filled.jsonl: err=%v", err)
	}

	// 2. isAnalyzed=1 but wrong beatData digest (e.g. stock OA row): MUST reject and write NO filled.jsonl.
	mdb := filepath.Join(stick, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mdb}).String())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE PerformanceData SET beatData = X'01020304', quickCues = X'0506', loops = X'0708' WHERE trackId = ?`, tr.EngineID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE Track SET isAnalyzed = 1 WHERE id = ?`, tr.EngineID)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if rec := postFilled(); rec.Code != http.StatusConflict || rec.Header().Get("X-RB-Status") != "not_committed" {
		t.Fatalf("mismatched-digest track-filled = (%d, %q), want (409, not_committed)", rec.Code, rec.Header().Get("X-RB-Status"))
	}
	if _, err := os.Stat(jpath); !os.IsNotExist(err) {
		t.Fatalf("mismatched-digest track-filled created filled.jsonl: err=%v", err)
	}

	// 3. Commit the real RBXA1 blobs + isAnalyzed=1 -> first notify appends 1 line, repeats deduplicate
	//    both in-memory and across an in-memory reset (restart).
	commitRBXA1ToStick(t, stick, tr.EngineID, recData.Body.Bytes())
	for i := 0; i < 3; i++ {
		if i == 2 {
			srvA.rbLinkJournalMu.Lock()
			srvA.rbLinkJournals = nil // simulate restart: must still dedup against filled.jsonl on disk
			srvA.rbLinkJournalMu.Unlock()
		}
		if rec := postFilled(); rec.Code != http.StatusOK {
			t.Fatalf("committed track-filled #%d status=%d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	if got := srvA.rbLinkFilledCount.Load(); got != 1 {
		t.Fatalf("rbLinkFilledCount = %d after 3 notices, want 1", got)
	}
	raw, err := os.ReadFile(jpath)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("filled.jsonl has %d lines after 3 notices, want 1", len(lines))
	}
}

func TestRekordboxLinkOnDemand2DeckShadowOffFallback(t *testing.T) {
	mediaRoot, _, libUUID, tr, importKey := setupLinkOnDemandStick(t, "STICK2D", 1)
	const ownerUUID = "bbbb0002-1111-2222-3333-444455556666"
	srvA := New(Config{StateDir: t.TempDir(), MediaRoot: mediaRoot, RBStageRoot: t.TempDir(), LinkUUID: ownerUUID}, nil)
	defer srvA.Close()
	tsA := httptest.NewServer(srvA)
	defer tsA.Close()
	uA, _ := url.Parse(tsA.URL)

	srvB := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB.Close()
	var candCalls atomic.Int32
	srvB.rbLinkPeerCandidates = func(context.Context) []string {
		candCalls.Add(1)
		return []string{uA.Host}
	}

	netDB := "net://" + ownerUUID + "/STICK2D/Engine Library/Database2/m.db"
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

// Fails on c76b2ede:
//  1. Unrelated shadow peer prevented 2-deck fallback from being tried.
//  2. Unrelated TCP peer skipped ARP fallback and poisoned the 60 s peer negative cache.
//  3. Bare RBXA1 reply without verified owner UUID header was accepted.
func TestRekordboxLinkOnDemandOwnerResolutionTiersAndVerification(t *testing.T) {
	mediaRoot, _, libUUID, tr, importKey := setupLinkOnDemandStick(t, "OWNTIERS", 1)
	const ownerUUID = "f00d0001-1111-2222-3333-444455556666"
	srvA := New(Config{StateDir: t.TempDir(), MediaRoot: mediaRoot, RBStageRoot: t.TempDir(), LinkUUID: ownerUUID}, nil)
	defer srvA.Close()
	tsA := httptest.NewServer(srvA)
	defer tsA.Close()
	uA, _ := url.Parse(tsA.URL)

	// Unrelated EngineX peer (no stick mounted, different Link UUID).
	srvUnrelated := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir(), LinkUUID: "99990001-1111-2222-3333-444455556666"}, nil)
	defer srvUnrelated.Close()
	tsUnrel := httptest.NewServer(srvUnrelated)
	defer tsUnrel.Close()
	uUnrel, _ := url.Parse(tsUnrel.URL)
	unrelHost, unrelPortStr, _ := net.SplitHostPort(uUnrel.Host)
	unrelPort := 0
	for _, c := range unrelPortStr {
		unrelPort = unrelPort*10 + int(c-'0')
	}

	// Unrelated non-EngineX HTTP server returning plain 405 (must NEVER negative-cache ownerUUID).
	unrel405 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer unrel405.Close()
	u405, _ := url.Parse(unrel405.URL)

	// Rogue server replying 200 OK with RBXA1 magic but missing/wrong owner UUID headers.
	rogue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("X-RB-Status", "owner")
			w.Header().Set("X-RB-UUID", "wrong-stick-uuid")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("X-RB-Key", tr.Key)
		w.Header().Set("X-RB-Written", "AAECAwQFBgcICQoLDA0ODw==")
		w.Header().Set("X-RB-Rules", "5")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("RBXA1\x01\x00\x00\x00\x00"))
	}))
	defer rogue.Close()
	uRogue, _ := url.Parse(rogue.URL)

	reqPayload, _ := json.Marshal(rbOnDemandRequest{
		DB:        "net://" + ownerUUID + "/OWNTIERS/Engine Library/Database2/m.db",
		UUID:      libUUID,
		Path:      "../" + tr.Rel,
		ID:        tr.EngineID,
		ImportKey: importKey,
	})

	// Subcase A: 1 unrelated shadow peer in shadow.peers (no exact peerDecks match) + real owner in 2-deck fallback.
	srvB1 := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB1.Close()
	srvB1.shadow.mu.Lock()
	srvB1.shadow.peerPort = unrelPort
	srvB1.shadow.peers[unrelHost] = time.Now()
	srvB1.shadow.mu.Unlock()
	srvB1.rbLinkPeerCandidates = func(context.Context) []string { return []string{uA.Host} }
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(reqPayload))
	req1.RemoteAddr = "127.0.0.1:5000"
	srvB1.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("unrelated shadow peer blocked 2-deck fallback: status=%d body=%s", rec1.Code, rec1.Body.String())
	}
	assertValidOnDemandResponse(t, rec1.Header(), rec1.Body.Bytes(), tr)

	// Subcase B: TCP fallback has only unrelated 405 + rogue bare-RBXA1; real owner is in ARP fallback.
	srvB2 := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB2.Close()
	srvB2.rbLinkPeerCandidates = func(context.Context) []string { return []string{u405.Host, uRogue.Host} }
	srvB2.rbLinkARPCandidates = func(context.Context) []string { return []string{uA.Host} }
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(reqPayload))
	req2.RemoteAddr = "127.0.0.1:5001"
	srvB2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("unrelated TCP peers blocked ARP fallback or rogue won: status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	assertValidOnDemandResponse(t, rec2.Header(), rec2.Body.Bytes(), tr)
	if srvB2.isLinkPeerNegCached(ownerUUID, time.Now()) {
		t.Fatal("unrelated 405 server poisoned negative cache for ownerUUID")
	}

	// Subcase C: Direct POST reply from rogue server without X-RB-UUID (even if endpoint came from peerDecks) is REJECTED.
	srvB3 := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB3.Close()
	rHost, rPortStr, _ := net.SplitHostPort(uRogue.Host)
	rPort := 0
	for _, c := range rPortStr {
		rPort = rPort*10 + int(c-'0')
	}
	srvB3.shadow.mu.Lock()
	srvB3.shadow.peerPort = rPort
	srvB3.shadow.peerDecks[1] = peerDeck{deckState: deckState{Deck: 1, Link: ownerUUID}, ip: rHost, at: time.Now()}
	srvB3.shadow.mu.Unlock()
	srvB3.rbLinkPeerCandidates = func(context.Context) []string { return nil }
	srvB3.rbLinkARPCandidates = func(context.Context) []string { return nil }
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(reqPayload))
	req3.RemoteAddr = "127.0.0.1:5002"
	srvB3.ServeHTTP(rec3, req3)
	if rec3.Code == http.StatusOK {
		t.Fatalf("accepted unverified bare RBXA1 reply without requested owner UUID")
	}

	// Subcase D: Unrelated 405 server alone (not verified owner) must NOT negative-cache ownerUUID.
	srvB4 := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB4.Close()
	srvB4.rbLinkPeerCandidates = func(context.Context) []string { return []string{u405.Host} }
	srvB4.rbLinkARPCandidates = func(context.Context) []string { return nil }
	rec4 := httptest.NewRecorder()
	req4 := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(reqPayload))
	req4.RemoteAddr = "127.0.0.1:5003"
	srvB4.ServeHTTP(rec4, req4)
	if rec4.Code == http.StatusOK || rec4.Header().Get("X-RB-Status") == "peer_unsupported" || srvB4.isLinkPeerNegCached(ownerUUID, time.Now()) {
		t.Fatalf("unverified 405 negative-cached ownerUUID: code=%d hdr=%q neg=%v",
			rec4.Code, rec4.Header().Get("X-RB-Status"), srvB4.isLinkPeerNegCached(ownerUUID, time.Now()))
	}
}

func TestRekordboxLinkOnDemandIdentityAndSecurity(t *testing.T) {
	mediaRoot, stick, libUUID, tr, importKey := setupLinkOnDemandStick(t, "SECUSB", 1)
	stageRoot := t.TempDir()

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

	const ownerUUID = "aaaa9999-1111-2222-3333-444455556666"
	srvA := New(Config{StateDir: t.TempDir(), MediaRoot: mediaRoot, RBStageRoot: stageRoot, LinkUUID: ownerUUID}, nil)
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
	// 4. Right UUID, wrong importKey -> 404 AND must NOT call NoteLoading (priority queue stays untouched)!
	if rec := postPeer(RekordboxPeerOnDemandRoute, rbPeerOnDemandRequest{UUID: libUUID, VolHint: "SECUSB", Path: "../" + tr.Rel, ImportKey: importKey + 999}, "192.168.1.20:9999", nil); rec.Code != http.StatusNotFound || rec.Header().Get("X-RB-Status") != "not_found" {
		t.Fatalf("wrong importKey: status=%d hdr=%q, want 404/not_found", rec.Code, rec.Header().Get("X-RB-Status"))
	}
	srvA.rbPriorityMu.Lock()
	priAfterInvalid := srvA.rbPriority
	srvA.rbPriorityMu.Unlock()
	if priAfterInvalid != nil {
		t.Fatal("invalid importKey LAN request invoked NoteLoading before track validation")
	}
	// 5. Right UUID + importKey, wrong owner UUID -> 404.
	if rec := postPeer(RekordboxPeerOnDemandRoute, rbPeerOnDemandRequest{UUID: libUUID, Owner: "bbbb0000-1111-2222-3333-444455556666", Path: "../" + tr.Rel, ImportKey: importKey}, "192.168.1.20:9999", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("wrong owner UUID: status=%d, want 404", rec.Code)
	}
	// 6. Stock Engine row (NULL pdbImportKey) -> 404.
	if rec := postPeer(RekordboxPeerOnDemandRoute, rbPeerOnDemandRequest{UUID: libUUID, VolHint: "SECUSB", Path: "../Contents/StockEngine.mp3", ImportKey: 1}, "192.168.1.20:9999", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("stock row: status=%d, want 404", rec.Code)
	}
	// 7. Non-LAN public IP -> 403.
	if rec := postPeer(RekordboxPeerOnDemandRoute, rbPeerOnDemandRequest{UUID: libUUID, Path: "../" + tr.Rel, ImportKey: importKey}, "8.8.8.8:1234", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("public IP: status=%d, want 403", rec.Code)
	}
	// 8. External relay header -> 404 (lanOnly guard in ServeHTTP).
	if rec := postPeer(RekordboxPeerOnDemandRoute, rbPeerOnDemandRequest{UUID: libUUID, Path: "../" + tr.Rel, ImportKey: importKey}, "192.168.1.20:1234", map[string]string{"Cf-Connecting-Ip": "1.2.3.4"}); rec.Code != http.StatusNotFound {
		t.Fatalf("relayed request: status=%d, want 404", rec.Code)
	}
	// 9. ForwardHandler hides both peer routes (GET & POST -> 404).
	for _, tc := range []struct{ method, route string }{
		{http.MethodGet, RekordboxPeerOnDemandRoute + "?uuid=" + libUUID},
		{http.MethodPost, RekordboxPeerOnDemandRoute},
		{http.MethodPost, RekordboxPeerFilledRoute},
	} {
		req := httptest.NewRequest(tc.method, tc.route, strings.NewReader(`{}`))
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		srvA.ForwardHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("ForwardHandler %s %s: status=%d, want 404", tc.method, tc.route, rec.Code)
		}
	}
	// 10. LAN admission cap returns 503 busy before touching stick/builder.
	srvA.rbLinkLANQueued.Store(rbLinkMaxQueuedLAN)
	if rec := postPeer(RekordboxPeerOnDemandRoute, rbPeerOnDemandRequest{UUID: libUUID, Path: "../" + tr.Rel, ImportKey: importKey}, "192.168.1.20:9999", nil); rec.Code != http.StatusServiceUnavailable || rec.Header().Get("X-RB-Status") != "busy" {
		t.Fatalf("saturated admission = (%d, %q), want (503, busy)", rec.Code, rec.Header().Get("X-RB-Status"))
	}
	srvA.rbLinkLANQueued.Store(0)

	// 11. Forged /peer/rekordbox/track-filled with wrong importKey must fail and write NO filled.jsonl.
	if rec := postPeer(RekordboxPeerFilledRoute, rbPeerFilledRequest{UUID: libUUID, Path: "../" + tr.Rel, ID: tr.EngineID, ImportKey: importKey + 1}, "192.168.1.20:9999", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("forged track-filled: status=%d, want 404", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(stageRoot, libUUID, "filled.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("forged track-filled created filled.jsonl: err=%v", err)
	}
	// 12. Valid peer request with matching UUID + importKey succeeds even without VolHint.
	if rec := postPeer(RekordboxPeerOnDemandRoute, rbPeerOnDemandRequest{UUID: libUUID, Owner: ownerUUID, Path: "../" + tr.Rel, ID: tr.EngineID, ImportKey: importKey}, "192.168.1.20:9999", nil); rec.Code != http.StatusOK {
		t.Fatalf("valid peer request without VolHint: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRekordboxLinkOnDemandOlderAndNonEngineXPeerNegativeCache(t *testing.T) {
	const olderUUID = "cccc0001-1111-2222-3333-444455556666"
	const nonEXUUID = "cccc0002-1111-2222-3333-444455556666"

	// Older EngineX peer answers GET /peer/decks with its Link UUID and returns 405/404 without X-RB-Status.
	var olderDataHits atomic.Int32
	olderPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/peer/decks" {
			respond(w, map[string]any{"decks": []map[string]any{{"deck": 1, "link": "{" + strings.ToUpper(olderUUID) + "}"}}})
			return
		}
		olderDataHits.Add(1)
		http.NotFound(w, r)
	}))
	defer olderPeer.Close()
	uOlder, _ := url.Parse(olderPeer.URL)

	// Verified peer (in peerDecks) with closed TCP port (ECONNREFUSED).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := ln.Addr().String()
	ln.Close()
	cHost, cPortStr, _ := net.SplitHostPort(closedAddr)
	cPort := 0
	for _, c := range cPortStr {
		cPort = cPort*10 + int(c-'0')
	}

	srvB := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB.Close()

	call := func(peerUUID string) (int, string) {
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

	// 1. Older EngineX peer verified via GET /peer/decks: 1st call negative-caches, 2nd call does 0 LAN requests.
	srvB.rbLinkPeerCandidates = func(context.Context) []string { return []string{uOlder.Host} }
	srvB.rbLinkARPCandidates = func(context.Context) []string { return nil }
	code, st := call(olderUUID)
	if code != http.StatusNotFound || st != "peer_unsupported" {
		t.Fatalf("older peer 1st call = (%d, %q), want (404, peer_unsupported)", code, st)
	}
	hits1 := srvB.rbLinkPeerHTTPCount.Load()
	if olderDataHits.Load() != 1 || hits1 != 1 {
		t.Fatalf("older peer 1st hits: client=%d server=%d, want 1/1", hits1, olderDataHits.Load())
	}
	code, st = call(olderUUID)
	if code != http.StatusNotFound || st != "peer_unsupported" {
		t.Fatalf("older peer 2nd call = (%d, %q), want (404, peer_unsupported)", code, st)
	}
	if srvB.rbLinkPeerHTTPCount.Load() != hits1 || olderDataHits.Load() != 1 {
		t.Fatalf("older peer 2nd call was not zero-cost cached: client=%d server=%d", srvB.rbLinkPeerHTTPCount.Load(), olderDataHits.Load())
	}

	// 2. Refused connection on a peerDecks endpoint is transient (e.g. restarting or moved),
	//    NEVER negative-cached as peer_unsupported (60 s neg-cache requires explicit verified reply).
	srvB.shadow.mu.Lock()
	srvB.shadow.peerPort = cPort
	srvB.shadow.peerDecks[2] = peerDeck{deckState: deckState{Deck: 2, Link: nonEXUUID}, ip: cHost, at: time.Now()}
	srvB.shadow.mu.Unlock()
	srvB.rbLinkPeerCandidates = func(context.Context) []string { return nil }
	srvB.rbLinkARPCandidates = func(context.Context) []string { return nil }
	code, st = call(nonEXUUID)
	if code == http.StatusOK || st == "peer_unsupported" || srvB.isLinkPeerNegCached(nonEXUUID, time.Now()) {
		t.Fatalf("refused connection must be transient, got (%d, %q) neg=%v", code, st, srvB.isLinkPeerNegCached(nonEXUUID, time.Now()))
	}
}

// Fails on 91c54855: a refused, timed-out, or reassigned cached owner address returned
// 404 peer_unsupported / 504 without clearing the stale cache and rediscovering the moved owner.
func TestRekordboxLinkOnDemandCachedOwnerMovedRefusedAndTimeout(t *testing.T) {
	mediaRoot, _, libUUID, tr, importKey := setupLinkOnDemandStick(t, "MOVED", 1)
	const ownerUUID = "f1f10001-1111-2222-3333-444455556666"

	srvNew := New(Config{StateDir: t.TempDir(), MediaRoot: mediaRoot, RBStageRoot: t.TempDir(), LinkUUID: ownerUUID}, nil)
	defer srvNew.Close()
	tsNew := httptest.NewServer(srvNew)
	defer tsNew.Close()
	uNew, _ := url.Parse(tsNew.URL)

	reqBody, _ := json.Marshal(rbOnDemandRequest{
		DB:        "net://" + ownerUUID + "/MOVED/Engine Library/Database2/m.db",
		UUID:      libUUID,
		Path:      "../" + tr.Rel,
		ID:        tr.EngineID,
		ImportKey: importKey,
	})
	doLoad := func(srv *Server) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(reqBody))
		req.RemoteAddr = "127.0.0.1:6100"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// Subcase A: Cached owner moved and old address now refuses connections (ECONNREFUSED).
	srvOld := New(Config{StateDir: t.TempDir(), MediaRoot: mediaRoot, RBStageRoot: t.TempDir(), LinkUUID: ownerUUID}, nil)
	defer srvOld.Close()
	tsOld := httptest.NewServer(srvOld)
	uOld, _ := url.Parse(tsOld.URL)

	srvB := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB.Close()
	var activePeer atomic.Value
	activePeer.Store(uOld.Host)
	srvB.rbLinkPeerCandidates = func(context.Context) []string { return []string{activePeer.Load().(string)} }
	srvB.rbLinkARPCandidates = func(context.Context) []string { return nil }

	if rec := doLoad(srvB); rec.Code != http.StatusOK {
		t.Fatalf("warm cache load failed: %d %s", rec.Code, rec.Body.String())
	}
	if ep, ok := srvB.getCachedLinkPeer(ownerUUID, time.Now()); !ok || ep != uOld.Host {
		t.Fatalf("cached peer = (%q, %v), want %q", ep, ok, uOld.Host)
	}

	// Shut down old owner (causes ECONNREFUSED on cached address) and expose moved owner in discovery.
	tsOld.Close()
	activePeer.Store(uNew.Host)

	recMoved := doLoad(srvB)
	if recMoved.Code != http.StatusOK {
		t.Fatalf("moved owner after refused cached addr = (%d, %q, %s), want 200 OK",
			recMoved.Code, recMoved.Header().Get("X-RB-Status"), recMoved.Body.String())
	}
	assertValidOnDemandResponse(t, recMoved.Header(), recMoved.Body.Bytes(), tr)
	if srvB.isLinkPeerNegCached(ownerUUID, time.Now()) {
		t.Fatal("refused cached address poisoned 60s negative cache")
	}
	if ep, ok := srvB.getCachedLinkPeer(ownerUUID, time.Now()); !ok || ep != uNew.Host {
		t.Fatalf("cache not updated to moved owner: (%q, %v), want %q", ep, ok, uNew.Host)
	}

	// Subcase B: Cached owner address now hangs/times out; clear cache and rediscover within remaining budget.
	hangSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer hangSrv.Close()
	uHang, _ := url.Parse(hangSrv.URL)

	srvB2 := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB2.Close()
	srvB2.rbLinkAnswerBudget = 240 * time.Millisecond
	srvB2.setCachedLinkPeer(ownerUUID, uHang.Host, time.Now())
	srvB2.rbLinkPeerCandidates = func(context.Context) []string { return []string{uNew.Host} }
	srvB2.rbLinkARPCandidates = func(context.Context) []string { return nil }

	t0 := time.Now()
	recTimeoutMoved := doLoad(srvB2)
	elapsed := time.Since(t0)
	if recTimeoutMoved.Code != http.StatusOK {
		t.Fatalf("moved owner after timed-out cached addr = (%d, %q, %s) in %v, want 200 OK",
			recTimeoutMoved.Code, recTimeoutMoved.Header().Get("X-RB-Status"), recTimeoutMoved.Body.String(), elapsed)
	}
	assertValidOnDemandResponse(t, recTimeoutMoved.Header(), recTimeoutMoved.Body.Bytes(), tr)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("rediscovery after cached timeout took %v, exceeded budget", elapsed)
	}
	if srvB2.isLinkPeerNegCached(ownerUUID, time.Now()) {
		t.Fatal("timed-out cached address poisoned negative cache")
	}
	if ep, ok := srvB2.getCachedLinkPeer(ownerUUID, time.Now()); !ok || ep != uNew.Host {
		t.Fatalf("cache not updated after cached timeout: (%q, %v), want %q", ep, ok, uNew.Host)
	}

	// Subcase C: Cached address reassigned to an unrelated 405 HTTP server -> NOT neg-cached, rediscovers real owner.
	reassigned405 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer reassigned405.Close()
	uReassigned, _ := url.Parse(reassigned405.URL)

	srvB3 := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB3.Close()
	srvB3.setCachedLinkPeer(ownerUUID, uReassigned.Host, time.Now())
	srvB3.rbLinkPeerCandidates = func(context.Context) []string { return []string{uNew.Host} }
	srvB3.rbLinkARPCandidates = func(context.Context) []string { return nil }

	recReassigned := doLoad(srvB3)
	if recReassigned.Code != http.StatusOK {
		t.Fatalf("reassigned cached 405 server blocked rediscovery: (%d, %q, %s)",
			recReassigned.Code, recReassigned.Header().Get("X-RB-Status"), recReassigned.Body.String())
	}
	assertValidOnDemandResponse(t, recReassigned.Header(), recReassigned.Body.Bytes(), tr)
	if srvB3.isLinkPeerNegCached(ownerUUID, time.Now()) {
		t.Fatal("reassigned unverified 405 server poisoned negative cache")
	}
}

// Fails on 91c54855: replyMatchesOwner treated X-RB-Owner as optional, allowing a discovery
// reply with only X-RB-UUID (no X-RB-Owner) to establish the peer cache.
func TestRekordboxLinkOnDemandDiscoveryRequiresOwnerHeaders(t *testing.T) {
	mediaRoot, _, libUUID, tr, importKey := setupLinkOnDemandStick(t, "REQHDR", 1)
	const ownerUUID = "f2f20001-1111-2222-3333-444455556666"

	// Peer replying 200 owner with X-RB-UUID ONLY (missing X-RB-Owner) must be rejected.
	missingOwnerHdr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("X-RB-Status", "owner")
			w.Header().Set("X-RB-UUID", libUUID)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("X-RB-UUID", libUUID)
		w.Header().Set("X-RB-Key", tr.Key)
		w.Header().Set("X-RB-Written", "AAECAwQFBgcICQoLDA0ODw==")
		w.Header().Set("X-RB-Rules", "5")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("RBXA1\x01\x00\x00\x00\x00"))
	}))
	defer missingOwnerHdr.Close()
	uMissing, _ := url.Parse(missingOwnerHdr.URL)

	// Real unit lacking its Link UUID must NOT answer GET discovery as owner.
	srvNoLink := New(Config{StateDir: t.TempDir(), MediaRoot: mediaRoot, RBStageRoot: t.TempDir()}, nil)
	defer srvNoLink.Close()
	tsNoLink := httptest.NewServer(srvNoLink)
	defer tsNoLink.Close()
	uNoLink, _ := url.Parse(tsNoLink.URL)

	// Real owner with LinkUUID set returns BOTH X-RB-UUID and X-RB-Owner.
	srvReal := New(Config{StateDir: t.TempDir(), MediaRoot: mediaRoot, RBStageRoot: t.TempDir(), LinkUUID: ownerUUID}, nil)
	defer srvReal.Close()
	tsReal := httptest.NewServer(srvReal)
	defer tsReal.Close()
	uReal, _ := url.Parse(tsReal.URL)

	srvB := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB.Close()
	srvB.rbLinkPeerCandidates = func(context.Context) []string { return []string{uMissing.Host, uNoLink.Host} }
	srvB.rbLinkARPCandidates = func(context.Context) []string { return []string{uReal.Host} }

	body, _ := json.Marshal(rbOnDemandRequest{
		DB:        "net://" + ownerUUID + "/REQHDR/Engine Library/Database2/m.db",
		UUID:      libUUID,
		Path:      "../" + tr.Rel,
		ID:        tr.EngineID,
		ImportKey: importKey,
	})
	req := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:6101"
	rec := httptest.NewRecorder()
	srvB.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("discovery with missing-owner-header candidate failed: %d %s", rec.Code, rec.Body.String())
	}
	assertValidOnDemandResponse(t, rec.Header(), rec.Body.Bytes(), tr)
	if ep, ok := srvB.getCachedLinkPeer(ownerUUID, time.Now()); !ok || ep != uReal.Host {
		t.Fatalf("cached peer = (%q, %v), want verified %q (not %q)", ep, ok, uReal.Host, uMissing.Host)
	}
	if replyMatchesOwner(http.Header{"X-Rb-Uuid": {libUUID}}, ownerUUID, libUUID) {
		t.Fatal("replyMatchesOwner accepted missing X-RB-Owner")
	}
}

// Fails on 91c54855: owner LAN endpoints did not set a socket read deadline on request body
// decoding, so a stalled body blocked past the 750 ms server budget.
func TestRekordboxLinkOnDemandServerBodyReadDeadline(t *testing.T) {
	mediaRoot, _, _, _, _ := setupLinkOnDemandStick(t, "BODYDL", 1)
	srvA := New(Config{StateDir: t.TempDir(), MediaRoot: mediaRoot, RBStageRoot: t.TempDir(), LinkUUID: "f3f30001-1111-2222-3333-444455556666"}, nil)
	defer srvA.Close()
	srvA.rbLinkAnswerBudget = 80 * time.Millisecond
	tsA := httptest.NewServer(srvA)
	defer tsA.Close()
	uA, _ := url.Parse(tsA.URL)

	for _, route := range []string{RekordboxPeerOnDemandRoute, RekordboxPeerFilledRoute} {
		conn, err := net.DialTimeout("tcp", uA.Host, time.Second)
		if err != nil {
			t.Fatalf("%s dial: %v", route, err)
		}
		_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Length: 64\r\nContent-Type: application/json\r\n\r\n{\"uuid\":\"partial", route, uA.Host)
		if err != nil {
			conn.Close()
			t.Fatalf("%s write: %v", route, err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
		t0 := time.Now()
		buf := make([]byte, 512)
		n, rerr := conn.Read(buf)
		elapsed := time.Since(t0)
		conn.Close()
		if rerr != nil || n == 0 {
			t.Fatalf("%s stalled body did not receive server timeout response in %v: n=%d err=%v", route, elapsed, n, rerr)
		}
		respHead := string(buf[:n])
		if !strings.Contains(respHead, "504 ") || !strings.Contains(respHead, "X-Rb-Status: timeout") {
			t.Fatalf("%s stalled body response = %q, want 504 with X-Rb-Status: timeout", route, respHead)
		}
		if elapsed > 300*time.Millisecond {
			t.Fatalf("%s stalled body took %v, want ~80 ms", route, elapsed)
		}
	}
}

// Fails on 91c54855: journal dedup used one global mutex across sticks and an unbounded map.
func TestRekordboxLinkOnDemandBoundedPerStickJournalDedup(t *testing.T) {
	stageRoot := t.TempDir()
	srv := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir(), RBStageRoot: stageRoot}, nil)
	defer srv.Close()
	srv.rbLinkJournalCap = 4

	makeRes := func(i int) *bridge.OnDemandResult {
		return &bridge.OnDemandResult{
			Key:     fmt.Sprintf("track-%d.mp3", i),
			Written: []byte{byte(i), 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
			Rules:   5,
			RowHash: fmt.Sprintf("hash-%d", i),
		}
	}

	// 1. Per-stick isolation: holding stick A's lock MUST NOT block stick B's append.
	jA := srv.stickJournal("stick-a")
	jA.mu.Lock()
	doneB := make(chan error, 1)
	go func() {
		appended, err := srv.appendRemoteFilledJournal(context.Background(), "stick-b", 1, makeRes(1))
		if err == nil && !appended {
			err = errors.New("expected stick-b append")
		}
		doneB <- err
	}()
	select {
	case err := <-doneB:
		jA.mu.Unlock()
		if err != nil {
			t.Fatalf("stick-b failed while stick-a locked: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		jA.mu.Unlock()
		t.Fatal("stick-b blocked waiting on stick-a's journal lock")
	}

	// 2. Bounded per-stick seen cap: inserting 12 distinct keys keeps in-memory map <= cap (4)
	//    and still deduplicates recent keys without appending duplicates.
	for i := 1; i <= 12; i++ {
		appended, err := srv.appendRemoteFilledJournal(context.Background(), "stick-cap", int64(i), makeRes(i))
		if err != nil || !appended {
			t.Fatalf("stick-cap append %d = (%v, %v)", i, appended, err)
		}
	}
	jCap := srv.stickJournal("stick-cap")
	jCap.mu.Lock()
	seenLen := len(jCap.seen)
	orderLen := len(jCap.order)
	jCap.mu.Unlock()
	if seenLen > 4 || orderLen > 4 {
		t.Fatalf("stick-cap dedup index grew unbounded: seen=%d order=%d, want <= 4", seenLen, orderLen)
	}
	if appended, err := srv.appendRemoteFilledJournal(context.Background(), "stick-cap", 12, makeRes(12)); err != nil || appended {
		t.Fatalf("recent key duplicate = (%v, %v), want (false, nil)", appended, err)
	}

	// 3. Tracked stick map is capped at rbLinkMaxTrackedSticks (16).
	for i := 0; i < rbLinkMaxTrackedSticks+8; i++ {
		_, err := srv.appendRemoteFilledJournal(context.Background(), fmt.Sprintf("stick-many-%02d", i), 1, makeRes(i+1))
		if err != nil {
			t.Fatalf("stick-many-%02d: %v", i, err)
		}
	}
	srv.rbLinkJournalMu.Lock()
	stickCount := len(srv.rbLinkJournals)
	srv.rbLinkJournalMu.Unlock()
	if stickCount > rbLinkMaxTrackedSticks {
		t.Fatalf("tracked stick journals = %d, want <= %d", stickCount, rbLinkMaxTrackedSticks)
	}

	// 4. Cancelled context aborts journal append immediately without touching disk.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := srv.appendRemoteFilledJournal(cancelled, "stick-cancel", 1, makeRes(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled journal append err = %v, want context.Canceled", err)
	}
}

func TestRekordboxLinkOnDemandAnswerBudgetTimeoutAndNoRedirect(t *testing.T) {
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
	srvB.rbLinkPeerCandidates = func(context.Context) []string { return []string{uSlow.Host} }
	srvB.rbLinkARPCandidates = func(context.Context) []string { return nil }

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

	srvB.rbLinkPeerCandidates = func(context.Context) []string { return []string{uRedir.Host} }
	req2 := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(body))
	req2.RemoteAddr = "127.0.0.1:5555"
	rec2 := httptest.NewRecorder()
	srvB.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusOK || redirTargetHits.Load() != 0 {
		t.Fatalf("redirect followed: code=%d targetHits=%d", rec2.Code, redirTargetHits.Load())
	}
}

// Fails on c76b2ede: /proc discovery had no ctx deadline check, and owner LAN handler had no server-side deadline.
func TestRekordboxLinkOnDemandDiscoveryAndServerDeadlines(t *testing.T) {
	// 1. Cancelled context stops /proc reads immediately (0 reads).
	srvB := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB.Close()
	var reads atomic.Int32
	srvB.rbLinkProcReadFile = func(string) ([]byte, error) {
		reads.Add(1)
		return nil, os.ErrNotExist
	}
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := srvB.discoverEngineLinkTCPPeers(cancelledCtx); len(got) != 0 {
		t.Fatalf("cancelled TCP discovery returned %v", got)
	}
	if got := srvB.discoverARPLANPeers(cancelledCtx); len(got) != 0 {
		t.Fatalf("cancelled ARP discovery returned %v", got)
	}
	if reads.Load() != 0 {
		t.Fatalf("cancelled /proc discovery performed %d reads, want 0", reads.Load())
	}

	// 2. Owner server-side deadline enforces rbLinkBudget even when client provides no timeout.
	mediaRoot, stick, libUUID, tr, importKey := setupLinkOnDemandStick(t, "SRVDL", 1)
	srvA := New(Config{StateDir: t.TempDir(), MediaRoot: mediaRoot, RBStageRoot: t.TempDir()}, nil)
	defer srvA.Close()
	srvA.rbLinkAnswerBudget = 80 * time.Millisecond

	// Hold an EXCLUSIVE SQLite lock on m.db so Unit A's lookupOnDemandDBMeta blocks on busy_timeout(500).
	mdb := filepath.Join(stick, "Engine Library", "Database2", "m.db")
	lockDB, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mdb}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer lockDB.Close()
	lockConn, err := lockDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lockConn.Close()
	if _, err := lockConn.ExecContext(context.Background(), `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	defer lockConn.ExecContext(context.Background(), `ROLLBACK`)

	raw, _ := json.Marshal(rbPeerOnDemandRequest{
		UUID: libUUID, Path: "../" + tr.Rel, ID: tr.EngineID, ImportKey: importKey,
	})
	t0 := time.Now()
	req := httptest.NewRequest(http.MethodPost, RekordboxPeerOnDemandRoute, bytes.NewReader(raw))
	req.RemoteAddr = "192.168.1.40:5000"
	rec := httptest.NewRecorder()
	srvA.ServeHTTP(rec, req)
	elapsed := time.Since(t0)
	if rec.Code != http.StatusGatewayTimeout || rec.Header().Get("X-RB-Status") != "timeout" {
		t.Fatalf("owner server deadline = (%d, %q) in %v, want (504, timeout)", rec.Code, rec.Header().Get("X-RB-Status"), elapsed)
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("owner server deadline took %v, want ~80 ms (< 300 ms)", elapsed)
	}
}

func TestRekordboxLinkOnDemandConcurrentTwoUnits(t *testing.T) {
	mediaRoot, stick, libUUID, tr, importKey := setupLinkOnDemandStick(t, "CONCUSB", 2)
	stageRoot := t.TempDir()
	const ownerUUID = "eeee0001-1111-2222-3333-444455556666"
	srvA := New(Config{StateDir: t.TempDir(), MediaRoot: mediaRoot, RBStageRoot: stageRoot, LinkUUID: ownerUUID}, nil)
	defer srvA.Close()
	tsA := httptest.NewServer(srvA)
	defer tsA.Close()
	uA, _ := url.Parse(tsA.URL)

	srvB1 := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB1.Close()
	srvB1.rbLinkPeerCandidates = func(context.Context) []string { return []string{uA.Host} }

	srvB2 := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB2.Close()
	srvB2.rbLinkPeerCandidates = func(context.Context) []string { return []string{uA.Host} }

	netDB := "net://" + ownerUUID + "/CONCUSB/Engine Library/Database2/m.db"
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

	// Commit once on Unit A's stick, then fire 6 concurrent track-filled notices -> deduplicated to 1 line!
	commitRBXA1ToStick(t, stick, tr.EngineID, bodies[0])
	var fwg sync.WaitGroup
	fStart := make(chan struct{})
	for i := range workers {
		fwg.Add(1)
		srv := srvB1
		if i%2 == 1 {
			srv = srvB2
		}
		go func(s *Server) {
			defer fwg.Done()
			<-fStart
			fReq := httptest.NewRequest(http.MethodPost, RekordboxOnDemandFilledRoute, bytes.NewReader(payload))
			fReq.RemoteAddr = "127.0.0.1:6000"
			fRec := httptest.NewRecorder()
			s.ServeHTTP(fRec, fReq)
		}(srv)
	}
	close(fStart)
	fwg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for srvA.rbLinkFilledCount.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if got := srvA.rbLinkFilledCount.Load(); got != 1 {
		t.Fatalf("concurrent track-filled recorded %d entries, want 1 (deduplicated)", got)
	}
	raw, err := os.ReadFile(filepath.Join(stageRoot, libUUID, "filled.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("filled.jsonl lines = %d, want 1 (deduplicated)", len(lines))
	}
	var m map[string]any
	if err := json.Unmarshal(lines[0], &m); err != nil {
		t.Fatalf("corrupted filled.jsonl line: %v (%s)", err, string(lines[0]))
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

	// Only Engine Link service sockets (owned by Engine inode AND ephemeral ports >=1024 excluding 8080/8443)
	// are accepted; >4 peers are all returned without silent truncation.
	sample := strings.Join([]string{
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode",
		"   0: 0200630A:C350 0100630A:1F90 01 00000000:00000000 00:00000000 00000000     0        0 100", // :8080 stream-sync (skip)
		"   1: 0200630A:C351 0300630A:0016 01 00000000:00000000 00:00000000 00000000     0        0 101", // :22 SSH (skip)
		"   2: 0200630A:C352 0400630A:01BD 01 00000000:00000000 00:00000000 00000000     0        0 102", // :445 SMB (skip)
		"   3: 0200630A:C353 0500630A:C352 01 00000000:00000000 00:00000000 00000000     0        0 999", // non-Engine inode (skip)
		"   4: 0200630A:C354 0100630A:B6C7 01 00000000:00000000 00:00000000 00000000     0        0 201", // Engine Link 10.99.0.1
		"   5: 0200630A:C355 0A00630A:B6C8 01 00000000:00000000 00:00000000 00000000     0        0 202", // Engine Link 10.99.0.10
		"   6: 0200630A:C356 0B00630A:B6C9 01 00000000:00000000 00:00000000 00000000     0        0 203", // Engine Link 10.99.0.11
		"   7: 0200630A:C357 0C00630A:B6CA 01 00000000:00000000 00:00000000 00000000     0        0 204", // Engine Link 10.99.0.12
		"   8: 0200630A:C358 220CFEA9:B6CB 01 00000000:00000000 00:00000000 00000000     0        0 205", // Engine Link 169.254.12.34 (5th!)
		"   9: 0200630A:C359 08080808:B6CC 01 00000000:00000000 00:00000000 00000000     0        0 206", // public IP (skip)
	}, "\n")
	inodes := map[string]bool{"100": true, "101": true, "102": true, "201": true, "202": true, "203": true, "204": true, "205": true, "206": true}
	got := parseProcNetTCPLANPeers(sample, map[string]bool{"10.99.0.2": true}, inodes)
	want := []string{"10.99.0.1", "10.99.0.10", "10.99.0.11", "10.99.0.12", "169.254.12.34"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("parseProcNetTCPLANPeers = %v, want %v", got, want)
	}

	// /proc/net/tcp6 IPv4-mapped ::ffff:10.99.0.1 (Qt dual-stack Engine Link socket) + pure IPv6 skipped.
	sample6 := strings.Join([]string{
		"  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode",
		"   0: 0000000000000000FFFF00000200630A:C350 0000000000000000FFFF00000100630A:B6C7 01 00000000:00000000 00:00000000 00000000 0 0 301",
		"   1: 00000000000000000000000000000001:C350 00000000000000000000000000000001:B6C7 01 00000000:00000000 00:00000000 00000000 0 0 302",
		"   2: 0000000000000000FFFF00000200630A:C350 0000000000000000FFFF00000100007F:B6C7 01 00000000:00000000 00:00000000 00000000 0 0 303",
	}, "\n")
	got6 := parseProcNetTCPLANPeers(sample6, map[string]bool{"10.99.0.2": true}, map[string]bool{"301": true, "302": true, "303": true})
	if len(got6) != 1 || got6[0] != "10.99.0.1" {
		t.Fatalf("parseProcNetTCPLANPeers(tcp6) = %v, want [10.99.0.1]", got6)
	}

	arp := strings.Join([]string{
		"IP address       HW type     Flags       HW address            Mask     Device",
		"10.99.0.1        0x1         0x2         aa:bb:cc:dd:ee:01     *        eth0",
		"10.99.0.2        0x1         0x2         aa:bb:cc:dd:ee:02     *        eth0",
		"192.168.1.50     0x1         0x0         00:00:00:00:00:00     *        eth0",
		"8.8.8.8          0x1         0x2         aa:bb:cc:dd:ee:03     *        eth0",
	}, "\n")
	gotARP := parseProcNetARPLANPeers(arp, map[string]bool{"10.99.0.2": true})
	if len(gotARP) != 1 || gotARP[0] != "10.99.0.1" {
		t.Fatalf("parseProcNetARPLANPeers = %v, want [10.99.0.1]", gotARP)
	}
}

func buildTestStockOABeats(t *testing.T, sr, samples, beat0Sample, bpm float64, totalBeats int64, adjShift float64) []byte {
	t.Helper()
	spb := sr * 60.0 / bpm
	p0 := beat0Sample - 4*spb
	b0 := int64(-4)
	b1 := totalBeats
	p1 := beat0Sample + float64(b1)*spb
	next0 := uint32(b1 - b0)
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, sr)
	_ = binary.Write(&b, binary.BigEndian, samples)
	b.WriteByte(1)
	for _, shift := range []float64{0, adjShift} {
		_ = binary.Write(&b, binary.BigEndian, uint64(2))
		_ = binary.Write(&b, binary.LittleEndian, p0+shift)
		_ = binary.Write(&b, binary.LittleEndian, b0)
		_ = binary.Write(&b, binary.LittleEndian, next0)
		_ = binary.Write(&b, binary.LittleEndian, uint32(0))
		_ = binary.Write(&b, binary.LittleEndian, p1+shift)
		_ = binary.Write(&b, binary.LittleEndian, b1)
		_ = binary.Write(&b, binary.LittleEndian, uint32(0))
		_ = binary.Write(&b, binary.LittleEndian, uint32(0xd4a9b300))
	}
	b.Write(make([]byte, 9))
	out, err := enginedb.QCompress(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func buildTestQuickCues(t *testing.T, hot0Pos float64, hot0Name string, mainPos float64, mainAdj byte, defPos float64) []byte {
	t.Helper()
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint64(8))
	for i := 0; i < 8; i++ {
		if i == 0 {
			b.WriteByte(byte(len(hot0Name)))
			b.WriteString(hot0Name)
			_ = binary.Write(&b, binary.BigEndian, hot0Pos)
		} else {
			b.WriteByte(0)
			_ = binary.Write(&b, binary.BigEndian, float64(-1))
		}
		b.Write([]byte{0, 0, 0, 0})
	}
	_ = binary.Write(&b, binary.BigEndian, mainPos)
	b.WriteByte(mainAdj)
	_ = binary.Write(&b, binary.BigEndian, defPos)
	out, err := enginedb.QCompress(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func buildTestLoops(slot0Start, slot0End float64, setFlag byte) []byte {
	var b bytes.Buffer
	b.WriteByte(8)
	b.Write(make([]byte, 7))
	for i := 0; i < 8; i++ {
		b.WriteByte(0)
		if i == 0 {
			_ = binary.Write(&b, binary.LittleEndian, slot0Start)
			_ = binary.Write(&b, binary.LittleEndian, slot0End)
			b.Write([]byte{setFlag, setFlag, 0, 0, 0, 0})
		} else {
			_ = binary.Write(&b, binary.LittleEndian, float64(-1))
			_ = binary.Write(&b, binary.LittleEndian, float64(-1))
			b.Write(make([]byte, 6))
		}
	}
	return b.Bytes()
}

// Fails on eccb70ee: Step 3 repairs stock-Engine-overwritten rekordbox rows (local & Link)
// while never touching any DJ-made cue, loop, active loop, moved main cue, grid edit, grid lock,
// already EngineX-filled row, or stock Engine stick.
func TestRekordboxStockOverwrittenRepairLocalAndLink(t *testing.T) {
	mediaRoot, stick, libUUID, tr, importKey := setupLinkOnDemandStick(t, "REPAIRUSB", 2)
	stageRoot := t.TempDir()
	const ownerUUID = "f5f50001-1111-2222-3333-444455556666"

	srvA := New(Config{
		StateDir:    t.TempDir(),
		MediaRoot:   mediaRoot,
		RBStageRoot: stageRoot,
		LinkUUID:    ownerUUID,
	}, nil)
	defer srvA.Close()
	tsA := httptest.NewServer(srvA)
	defer tsA.Close()
	uA, _ := url.Parse(tsA.URL)

	srvB := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvB.Close()
	srvB.rbLinkPeerCandidates = func(context.Context) []string { return []string{uA.Host} }
	srvB.rbLinkARPCandidates = func(context.Context) []string { return nil }

	mdb := filepath.Join(stick, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mdb}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const beat0 = 11391.4248046875
	stockTD := []byte{0, 0, 0, 68, 0x78, 0x9c}
	stockOV := bytes.Repeat([]byte{0x55}, 1200)
	stockBeats := buildTestStockOABeats(t, 44100, 13707967, beat0, 136.0, 704, 0)
	stockAutoCues := buildTestQuickCues(t, -1, "", beat0, 1, beat0)
	stockUnadjCues := buildTestQuickCues(t, -1, "", 0, 0, 0)
	emptyLoops := buildTestLoops(-1, -1, 0)

	setRow := func(td, ov, bd, qc, lp []byte, active, locked int, thirdParty any, importK any) {
		t.Helper()
		if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=?, thirdPartySourceId=? WHERE trackId=?`,
			td, ov, bd, qc, lp, active, thirdParty, tr.EngineID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET isAnalyzed=?, isBeatGridLocked=?, pdbImportKey=?, bpmAnalyzed=? WHERE id=?`,
			func() int {
				if len(bd) > 0 {
					return 1
				}
				return 0
			}(), locked, importK, 136.0, tr.EngineID); err != nil {
			t.Fatal(err)
		}
	}

	callLocal := func(bd, qc, lp []byte) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(rbOnDemandRequest{
			DB:        mdb,
			UUID:      libUUID,
			Path:      "../" + tr.Rel,
			ID:        tr.EngineID,
			ImportKey: importKey,
			Repair:    true,
			CurBeats:  hex.EncodeToString(bd),
			CurCues:   hex.EncodeToString(qc),
			CurLoops:  hex.EncodeToString(lp),
		})
		req := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(raw))
		req.RemoteAddr = "127.0.0.1:6200"
		rec := httptest.NewRecorder()
		srvA.ServeHTTP(rec, req)
		return rec
	}

	callLink := func(bd, qc, lp []byte) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(rbOnDemandRequest{
			DB:        "net://" + ownerUUID + "/REPAIRUSB/Engine Library/Database2/m.db",
			UUID:      libUUID,
			Path:      "../" + tr.Rel,
			ID:        tr.EngineID,
			ImportKey: importKey,
			Repair:    true,
			CurBeats:  hex.EncodeToString(bd),
			CurCues:   hex.EncodeToString(qc),
			CurLoops:  hex.EncodeToString(lp),
		})
		req := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(raw))
		req.RemoteAddr = "127.0.0.1:6201"
		rec := httptest.NewRecorder()
		srvB.ServeHTTP(rec, req)
		return rec
	}

	// 1. Stock Engine OA row (exact auto-cue at beat 0 layout) -> repaired locally AND over Link!
	setRow(stockTD, stockOV, stockBeats, stockAutoCues, emptyLoops, 0, 0, nil, importKey)
	recLoc := callLocal(stockBeats, stockAutoCues, emptyLoops)
	if recLoc.Code != http.StatusOK || recLoc.Header().Get("X-RB-Repair") != "1" {
		t.Fatalf("local repair = (%d, X-RB-Repair=%q) body=%s", recLoc.Code, recLoc.Header().Get("X-RB-Repair"), recLoc.Body.String())
	}
	assertValidOnDemandResponse(t, recLoc.Header(), recLoc.Body.Bytes(), tr)
	bpmVal, err := strconv.ParseFloat(recLoc.Header().Get("X-RB-BPM"), 64)
	if err != nil || bpmVal < 20 || bpmVal > 500 {
		t.Fatalf("X-RB-BPM = %q, want valid positive BPM", recLoc.Header().Get("X-RB-BPM"))
	}

	recLink := callLink(stockBeats, stockAutoCues, emptyLoops)
	if recLink.Code != http.StatusOK || recLink.Header().Get("X-RB-Repair") != "1" {
		t.Fatalf("link repair = (%d, X-RB-Repair=%q) body=%s", recLink.Code, recLink.Header().Get("X-RB-Repair"), recLink.Body.String())
	}
	if !bytes.Equal(recLink.Body.Bytes(), recLoc.Body.Bytes()) {
		t.Fatal("link repair RBXA1 differed from local repair RBXA1")
	}

	// 2. Aborted partial-load empty-cue stub (NULL td/ov/bd/loops + empty unadjusted quickCues) -> repaired!
	setRow(nil, nil, nil, stockUnadjCues, nil, 0, 0, nil, importKey)
	recStub := callLink(nil, stockUnadjCues, nil)
	if recStub.Code != http.StatusOK || recStub.Header().Get("X-RB-Repair") != "1" {
		t.Fatalf("aborted stub repair = (%d, X-RB-Repair=%q) body=%s", recStub.Code, recStub.Header().Get("X-RB-Repair"), recStub.Body.String())
	}

	// 3. Stale hook blob mismatch -> rejected 409 not_repairable.
	setRow(stockTD, stockOV, stockBeats, stockAutoCues, emptyLoops, 0, 0, nil, importKey)
	if rec := callLink(stockBeats, stockUnadjCues, emptyLoops); rec.Code != http.StatusConflict || rec.Header().Get("X-RB-Status") != "not_repairable" {
		t.Fatalf("stale blob mismatch = (%d, %q), want (409, not_repairable)", rec.Code, rec.Header().Get("X-RB-Status"))
	}
	if rec := callLocal(nil, nil, nil); rec.Code != http.StatusConflict || rec.Header().Get("X-RB-Status") != "not_repairable" {
		t.Fatalf("empty repair blobs = (%d, %q), want (409, not_repairable)", rec.Code, rec.Header().Get("X-RB-Status"))
	}

	// 4. Every DJ edit MUST be rejected (409 not_repairable) on BOTH local and Link:
	djCases := []struct {
		name   string
		bd     []byte
		qc     []byte
		lp     []byte
		active int
		locked int
	}{
		{"dj_hot_cue", stockBeats, buildTestQuickCues(t, 44100, "", beat0, 1, beat0), emptyLoops, 0, 0},
		{"dj_named_cue", stockBeats, buildTestQuickCues(t, -1, "Drop", beat0, 1, beat0), emptyLoops, 0, 0},
		{"dj_moved_main_cue", stockBeats, buildTestQuickCues(t, -1, "", 220500, 1, beat0), emptyLoops, 0, 0},
		{"dj_saved_loop", stockBeats, stockAutoCues, buildTestLoops(44100, 88200, 1), 0, 0},
		{"dj_active_loop", stockBeats, stockAutoCues, emptyLoops, 1, 0},
		{"dj_edited_grid", buildTestStockOABeats(t, 44100, 13707967, beat0, 136.0, 704, 15.0), stockAutoCues, emptyLoops, 0, 0},
		{"dj_locked_grid", stockBeats, stockAutoCues, emptyLoops, 0, 1},
	}
	for _, tc := range djCases {
		setRow(stockTD, stockOV, tc.bd, tc.qc, tc.lp, tc.active, tc.locked, nil, importKey)
		for _, mode := range []struct {
			label string
			rec   *httptest.ResponseRecorder
		}{
			{"local", callLocal(tc.bd, tc.qc, tc.lp)},
			{"link", callLink(tc.bd, tc.qc, tc.lp)},
		} {
			if mode.rec.Code != http.StatusConflict || mode.rec.Header().Get("X-RB-Status") != "not_repairable" {
				t.Fatalf("DJ case %s (%s) = (%d, %q), want (409, not_repairable)", tc.name, mode.label, mode.rec.Code, mode.rec.Header().Get("X-RB-Status"))
			}
		}
	}

	// 5. After committing the repaired RBXA1 row (EngineX-filled PQTZ) -> rejected 409 not_repairable.
	commitRBXA1ToStick(t, stick, tr.EngineID, recLink.Body.Bytes())
	var curBD, curQC, curLP []byte
	if err := db.QueryRow(`SELECT beatData, quickCues, loops FROM PerformanceData WHERE trackId=?`, tr.EngineID).Scan(&curBD, &curQC, &curLP); err != nil {
		t.Fatal(err)
	}
	if rec := callLocal(curBD, curQC, curLP); rec.Code != http.StatusConflict || rec.Header().Get("X-RB-Status") != "not_repairable" {
		t.Fatalf("already EngineX-filled row = (%d, %q), want (409, not_repairable)", rec.Code, rec.Header().Get("X-RB-Status"))
	}

	// 6. Stock Engine row (pdbImportKey IS NULL) -> rejected 404 not_found.
	setRow(stockTD, stockOV, stockBeats, stockAutoCues, emptyLoops, 0, 0, nil, nil)
	if rec := callLocal(stockBeats, stockAutoCues, emptyLoops); rec.Code != http.StatusNotFound {
		t.Fatalf("stock Engine row (pdbImportKey=NULL) = %d, want 404", rec.Code)
	}

	// 7. Row already recorded in filled.jsonl (even with stock-shaped digest) -> rejected 409 not_repairable.
	setRow(stockTD, stockOV, stockBeats, stockAutoCues, emptyLoops, 0, 0, nil, importKey)
	if err := os.MkdirAll(filepath.Join(stageRoot, libUUID), 0o700); err != nil {
		t.Fatal(err)
	}
	jLine, _ := json.Marshal(rbFilledJournalLine{
		ID:  tr.EngineID,
		Key: tr.Key,
		WR:  base64.StdEncoding.EncodeToString(enginedb.WrittenDigest(stockBeats, stockAutoCues, emptyLoops)),
		V:   5,
	})
	if err := os.WriteFile(filepath.Join(stageRoot, libUUID, "filled.jsonl"), append(jLine, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := callLocal(stockBeats, stockAutoCues, emptyLoops); rec.Code != http.StatusConflict || rec.Header().Get("X-RB-Status") != "not_repairable" {
		t.Fatalf("filled.jsonl matched row = (%d, %q), want (409, not_repairable)", rec.Code, rec.Header().Get("X-RB-Status"))
	}
	_ = os.Remove(filepath.Join(stageRoot, libUUID, "filled.jsonl"))

	// 8. OneLibrary FastBase + stock OA -> repaired; DJ edit over FastBase -> rejected.
	fbTrack := rekordbox.Track{
		SampleRate: 44100, Duration: 180, BPM: 136,
		Cues: []rekordbox.Cue{{HotCue: 1, TimeMS: 1000, Name: "Intro"}},
	}
	fb := enginedb.FastBaseForTrack(fbTrack)
	dbDir := filepath.Join(stick, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	snapTr := snap.Tracks[tr.Key]
	snapTr.FastKnown = true
	snapTr.FastCues = fb.Cues
	snapTr.FastLoops = fb.Loops
	snapTr.FastActive = fb.Active
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}
	// Reset builder snapshot cache by using a fresh server instance against the updated snapshot.
	srvFast := New(Config{StateDir: t.TempDir(), MediaRoot: mediaRoot, RBStageRoot: stageRoot, LinkUUID: ownerUUID}, nil)
	defer srvFast.Close()
	setRow(stockTD, stockOV, stockBeats, fb.Cues, fb.Loops, int(fb.Active), 0, nil, importKey)
	fastRaw, _ := json.Marshal(rbOnDemandRequest{
		DB: mdb, UUID: libUUID, Path: "../" + tr.Rel, ID: tr.EngineID, ImportKey: importKey,
		Repair: true, CurBeats: hex.EncodeToString(stockBeats), CurCues: hex.EncodeToString(fb.Cues), CurLoops: hex.EncodeToString(fb.Loops),
	})
	reqFast := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(fastRaw))
	reqFast.RemoteAddr = "127.0.0.1:6202"
	recFast := httptest.NewRecorder()
	srvFast.ServeHTTP(recFast, reqFast)
	if recFast.Code != http.StatusOK || recFast.Header().Get("X-RB-Repair") != "1" {
		t.Fatalf("OneLibrary FastBase + stock OA = (%d, %q) body=%s", recFast.Code, recFast.Header().Get("X-RB-Repair"), recFast.Body.String())
	}
	// DJ adds Hot Cue 2 over OneLibrary FastBase -> rejected!
	fbTrackDJ := fbTrack
	fbTrackDJ.Cues = append(append([]rekordbox.Cue(nil), fbTrack.Cues...), rekordbox.Cue{HotCue: 2, TimeMS: 3000, Name: "DJ2"})
	fbDJ := enginedb.FastBaseForTrack(fbTrackDJ)
	setRow(stockTD, stockOV, stockBeats, fbDJ.Cues, fb.Loops, int(fb.Active), 0, nil, importKey)
	fastDJRaw, _ := json.Marshal(rbOnDemandRequest{
		DB: mdb, UUID: libUUID, Path: "../" + tr.Rel, ID: tr.EngineID, ImportKey: importKey,
		Repair: true, CurBeats: hex.EncodeToString(stockBeats), CurCues: hex.EncodeToString(fbDJ.Cues), CurLoops: hex.EncodeToString(fb.Loops),
	})
	reqFastDJ := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(fastDJRaw))
	reqFastDJ.RemoteAddr = "127.0.0.1:6203"
	recFastDJ := httptest.NewRecorder()
	srvFast.ServeHTTP(recFastDJ, reqFastDJ)
	if recFastDJ.Code != http.StatusConflict || recFastDJ.Header().Get("X-RB-Status") != "not_repairable" {
		t.Fatalf("DJ edit on OneLibrary FastBase = (%d, %q), want (409, not_repairable)", recFastDJ.Code, recFastDJ.Header().Get("X-RB-Status"))
	}

	// 9. Mixed-version older peer replying 200 OK WITHOUT X-RB-Repair: 1 must fail closed at Unit B relay!
	const oldOwner = "f6f60001-1111-2222-3333-444455556666"
	oldPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("X-RB-Status", "owner")
			w.Header().Set("X-RB-UUID", libUUID)
			w.Header().Set("X-RB-Owner", oldOwner)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-RB-UUID", libUUID)
		w.Header().Set("X-RB-Owner", oldOwner)
		w.Header().Set("X-RB-Key", tr.Key)
		w.Header().Set("X-RB-Written", "AAECAwQFBgcICQoLDA0ODw==")
		w.Header().Set("X-RB-Rules", "5")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(recLink.Body.Bytes())
	}))
	defer oldPeer.Close()
	uOld, _ := url.Parse(oldPeer.URL)
	srvBMixed := New(Config{StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	defer srvBMixed.Close()
	srvBMixed.rbLinkPeerCandidates = func(context.Context) []string { return []string{uOld.Host} }
	mixedRaw, _ := json.Marshal(rbOnDemandRequest{
		DB:        "net://" + oldOwner + "/REPAIRUSB/Engine Library/Database2/m.db",
		UUID:      libUUID,
		Path:      "../" + tr.Rel,
		ID:        tr.EngineID,
		ImportKey: importKey,
		Repair:    true,
		CurBeats:  hex.EncodeToString(stockBeats),
		CurCues:   hex.EncodeToString(stockAutoCues),
		CurLoops:  hex.EncodeToString(emptyLoops),
	})
	reqMixed := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(mixedRaw))
	reqMixed.RemoteAddr = "127.0.0.1:6204"
	recMixed := httptest.NewRecorder()
	srvBMixed.ServeHTTP(recMixed, reqMixed)
	if recMixed.Code != http.StatusConflict || recMixed.Header().Get("X-RB-Status") != "not_repairable" {
		t.Fatalf("older peer missing X-RB-Repair = (%d, %q), want (409, not_repairable)", recMixed.Code, recMixed.Header().Get("X-RB-Status"))
	}
}
