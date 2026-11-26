package webui

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func gridTestPerf(t *testing.T, bpm float64, firstBeatMS uint32, hotCueMS uint32) enginedb.Performance {
	t.Helper()
	stepMS := uint32(math.Round(60000.0 / bpm))
	var beats []rekordbox.Beat
	for i := 0; i < 8; i++ {
		beats = append(beats, rekordbox.Beat{
			TimeMS: firstBeatMS + uint32(i)*stepMS,
			BPM:    bpm,
			Number: uint16(i%4 + 1),
		})
	}
	a := &rekordbox.Analysis{
		Beats:     beats,
		Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: bytes.Repeat([]byte{90, 50, 20}, 20)}},
	}
	if hotCueMS > 0 {
		a.Cues = []rekordbox.Cue{{HotCue: 1, TimeMS: hotCueMS, Name: "Intro", Color: [4]byte{255, 255, 0, 0}}}
	}
	p, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 10, SampleRate: 44100, SampleCount: 441000, BPM: bpm}, a)
	if err != nil || !p.Complete {
		t.Fatalf("encode performance: %v complete=%v", err, p.Complete)
	}
	return p
}

func makeGridTestSource(t *testing.T, bpm float64, firstBeatMS uint32) (src *Server, srcHTTP *httptest.Server, dbPath, trackPath string, audioHits *atomic.Int32) {
	t.Helper()
	dataRoot := t.TempDir()
	lib := filepath.Join(dataRoot, "AppDataUser/AIR Music Technology/Engine/Engine Library")
	for _, d := range []string{filepath.Join(lib, "Database2"), filepath.Join(dataRoot, "Music")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	trackPath = filepath.Join(dataRoot, "Music", "track.wav")
	if err := os.WriteFile(trackPath, []byte("RIFF0000WAVEfmt "), 0o644); err != nil {
		t.Fatal(err)
	}
	file, _, db := testlibrary.Create(t)
	p := gridTestPerf(t, bpm, firstBeatMS, 0)
	if _, err := db.Exec(`INSERT INTO Track(path,filename,title,artist,bpm,bpmAnalyzed,isAnalyzed) VALUES('../../../../Music/track.wav','track.wav','Track A','Artist A',?,?,1)`, int(bpm), bpm); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?`, p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	dbPath = filepath.Join(lib, "Database2", "m.db")
	if err := os.WriteFile(dbPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	src = New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	t.Cleanup(func() { src.Close() })
	src.shadow.dataRoot, src.shadow.mediaRoot = dataRoot, t.TempDir()
	audioHits = &atomic.Int32{}
	srcHTTP = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/audio") {
			audioHits.Add(1)
		}
		src.ServeHTTP(w, r)
	}))
	t.Cleanup(srcHTTP.Close)
	return src, srcHTTP, dbPath, trackPath, audioHits
}

func updateGridTestDB(t *testing.T, dbPath string, bpm float64, firstBeatMS uint32, hotCueMS uint32) {
	t.Helper()
	p := gridTestPerf(t, bpm, firstBeatMS, hotCueMS)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE Track SET bpm=?, bpmAnalyzed=? WHERE filename='track.wav'`, int(bpm), bpm); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?`, p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops); err != nil {
		t.Fatal(err)
	}
	// Ensure mtime moves even on coarse filesystems.
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(dbPath, future, future)
}

// #346 regression: editing the beat grid on the source deck must propagate to an
// already-loaded shadow copy without re-downloading audio or changing d.Path
// (no deck reload), and must also refresh display-only shadow beats.
func TestShadowGridEditUpdatesPeerInPlace(t *testing.T) {
	oldBeacon := shadowBeaconOn
	shadowBeaconOn = false
	defer func() { shadowBeaconOn = oldBeacon }()
	oldEvery := gridGenEvery
	gridGenEvery = 0
	defer func() { gridGenEvery = oldEvery }()
	oldRolePath := fourDeckRolePath
	roleFile := filepath.Join(t.TempDir(), "four-deck-role")
	if err := os.WriteFile(roleFile, []byte("24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fourDeckRolePath = roleFile
	defer func() { fourDeckRolePath = oldRolePath }()

	_, srcHTTP, srcDB, _, audioHits := makeGridTestSource(t, 128, 250)
	u, _ := url.Parse(srcHTTP.URL)
	srcPort, _ := strconv.Atoi(u.Port())

	postSource := func(bpm float64) {
		body := fmt.Sprintf(`[{"deck":1,"net":"net://src-uuid/(Unknown)/Music/track.wav","bpm":%v,"playing":false,"positionMs":1000}]`, bpm)
		resp, err := http.Post(srcHTTP.URL+"/v1/enginex/decks", "application/json", strings.NewReader(body))
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("decksPost: %v %v", err, resp)
		}
		_ = resp.Body.Close()
	}
	aliveSource := func() {
		resp, err := http.Post(srcHTTP.URL+"/v1/enginex/decks/alive", "", nil)
		if err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("decksAlive: %v %v", err, resp)
		}
		_ = resp.Body.Close()
	}
	getPeerDecks := func() []map[string]any {
		resp, err := http.Get(srcHTTP.URL + "/peer/decks")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Decks []map[string]any `json:"decks"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Decks
	}

	postSource(128)
	initialPeer := getPeerDecks()
	if len(initialPeer) != 1 {
		t.Fatalf("expected 1 peer deck, got %d", len(initialPeer))
	}
	gen1, _ := initialPeer[0]["gridGen"].(string)
	if gen1 == "" {
		t.Fatalf("source /peer/decks did not publish gridGen: %+v", initialPeer[0])
	}

	// Receiver 1: Engine mode with local m.db tracking the shadow copy.
	recvData := t.TempDir()
	recvLib := filepath.Join(recvData, "AppDataUser/AIR Music Technology/Engine/Engine Library")
	if err := os.MkdirAll(filepath.Join(recvLib, "Database2"), 0o755); err != nil {
		t.Fatal(err)
	}
	recvUIDir := filepath.Join(recvData, "enginex", "ui")
	if err := os.MkdirAll(recvUIDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recv := New(Config{UIDir: recvUIDir, StateDir: t.TempDir()}, nil)
	defer recv.Close()
	recv.shadow.dataRoot, recv.shadow.mediaRoot = recvData, t.TempDir()
	recv.shadow.peerPort = srcPort
	regridSock := filepath.Join(t.TempDir(), "shadow-regrid.sock")
	regridConn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: regridSock, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen unixgram: %v", err)
	}
	defer regridConn.Close()
	recv.shadow.regridSockPath = regridSock
	regridCh := make(chan string, 8)
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := regridConn.Read(buf)
			if err != nil {
				return
			}
			regridCh <- string(buf[:n])
		}
	}()
	if err := os.WriteFile(filepath.Join(recvUIDir, "settings.conf"), []byte("four_deck=1\nfour_deck_role=24\nshadow_mode=engine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Receiver 2: Display-only mode.
	disp := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer disp.Close()
	disp.shadow.peerPort = srcPort
	dispSrv := httptest.NewServer(disp)
	defer dispSrv.Close()
	uiPost(t, dispSrv.URL, "fourDeckWaveform=1&shadowMode=cdj")

	feedPeers := func() {
		resp, err := http.Get(srcHTTP.URL + "/peer/decks")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			Decks []deckState `json:"decks"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		for _, srv := range []*Server{recv, disp} {
			srv.shadow.mu.Lock()
			srv.shadow.takePeer("127.0.0.1", body.Decks, now)
			srv.shadow.mu.Unlock()
		}
	}
	feedPeers()

	// Wait for initial Engine shadow copy and Display-only metadata.
	var copyPath string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(15 * time.Millisecond) {
		for _, d := range recv.shadowComposeLegacy("24", time.Now()) {
			if d.Deck == 1 && d.Path != "" {
				copyPath = d.Path
			}
		}
		if copyPath != "" {
			break
		}
	}
	if copyPath == "" {
		t.Fatal("initial shadow copy was not fetched")
	}
	perf1, err := os.ReadFile(copyPath + ".perf")
	if err != nil {
		t.Fatalf("read initial .perf: %v", err)
	}
	bpm1, ok := expfBPM(perf1)
	if !ok || math.Abs(bpm1-128) > 0.01 {
		t.Fatalf("initial .perf bpm=%v ok=%v, want 128", bpm1, ok)
	}

	// Initial load must not send any regrid datagram.
	select {
	case msg := <-regridCh:
		t.Fatalf("unexpected regrid datagram on initial load: %q", msg)
	default:
	}

	// Wait for initial Display-only asset.
	readDisp := func() (key string, bpm float64, firstBeatMS float64, ok bool) {
		decks := disp.shadowComposeDisplay("24", time.Now())
		if len(decks) != 1 || decks[0].Key == "" || decks[0].Loading {
			return "", 0, 0, false
		}
		resp, err := http.Get(dispSrv.URL + "/v1/enginex/shadow/display?deck=1&key=" + url.QueryEscape(decks[0].Key))
		if err != nil || resp.StatusCode != 200 {
			if resp != nil {
				_ = resp.Body.Close()
			}
			return "", 0, 0, false
		}
		defer resp.Body.Close()
		var body struct {
			Key   string  `json:"key"`
			BPM   float64 `json:"bpm"`
			Beats []struct {
				MS float64 `json:"ms"`
			} `json:"beats"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || len(body.Beats) == 0 {
			return "", 0, 0, false
		}
		return body.Key, body.BPM, body.Beats[0].MS, true
	}
	var dispKey1 string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(15 * time.Millisecond) {
		k, bpm, b0, ready := readDisp()
		if ready {
			if math.Abs(bpm-128) > 0.01 || math.Abs(b0-250) > 2 {
				t.Fatalf("initial display bpm=%v beat0=%v, want 128 / 250ms", bpm, b0)
			}
			dispKey1 = k
			break
		}
	}
	if dispKey1 == "" {
		t.Fatal("initial display asset not ready")
	}

	// Edit 1: phase-only beat-grid shift (BPM still 128, beat0 250ms -> 380ms)
	// with ONLY /v1/enginex/decks/alive (as a paused grid shift sends).
	updateGridTestDB(t, srcDB, 128, 380, 0)
	aliveSource()
	editedPeer := getPeerDecks()
	gen2, _ := editedPeer[0]["gridGen"].(string)
	if gen2 == "" || gen2 == gen1 {
		t.Fatalf("gridGen did not change after phase-only beat-grid edit via decksAlive: %q -> %q", gen1, gen2)
	}
	feedPeers()

	// Engine receiver must refresh .perf in place (same d.Path, audio downloaded once),
	// stage .perf.regrid, and retry notifying the native hook socket once m.db commits Track 42.
	var got deckState
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(15 * time.Millisecond) {
		for _, d := range recv.shadowComposeLegacy("24", time.Now()) {
			if d.Deck == 1 && d.GridGen == gen2 {
				got = d
			}
		}
		if got.GridGen == gen2 {
			break
		}
	}
	if got.GridGen != gen2 {
		t.Fatalf("receiver did not publish in-place grid update: %+v (want gridGen=%q)", got, gen2)
	}
	if got.Path != copyPath {
		t.Fatalf("receiver changed shadow copy path (would reload deck!): %q -> %q", copyPath, got.Path)
	}
	if hits := audioHits.Load(); hits != 1 {
		t.Fatalf("audio downloaded %d times, want 1", hits)
	}
	perf2, err := os.ReadFile(copyPath + ".perf")
	if err != nil {
		t.Fatalf("read updated .perf: %v", err)
	}
	if bytes.Equal(perf1, perf2) {
		t.Fatal("updated .perf is byte-identical to old .perf after phase shift")
	}
	if _, err := os.Stat(copyPath + ".perf.regrid"); err != nil {
		t.Fatalf("missing .perf.regrid marker: %v", err)
	}
	// Before m.db commits Track 42, no datagram has been sent yet.
	select {
	case msg := <-regridCh:
		t.Fatalf("unexpected regrid datagram before m.db Track row existed: %q", msg)
	default:
	}
	// Now simulate Engine committing the shadow copy into B's m.db as Track id=42;
	// the pending regrid retry must deliver "42\t<copyPath>" to the native hook socket.
	relCopy, err := filepath.Rel(recvLib, copyPath)
	if err != nil {
		t.Fatal(err)
	}
	tmpDB, _, rdb := testlibrary.Create(t)
	if _, err := rdb.Exec(`INSERT INTO Track(id,path,filename,bpmAnalyzed,isAnalyzed) VALUES(42,?,'track.wav',128,1)`, filepath.ToSlash(relCopy)); err != nil {
		t.Fatal(err)
	}
	_ = rdb.Close()
	rawRecvDB, err := os.ReadFile(tmpDB)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recvLib, "Database2", "m.db"), rawRecvDB, 0o644); err != nil {
		t.Fatal(err)
	}
	wantDgram := fmt.Sprintf("42\t%s", copyPath)
	var gotDgram string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		recv.shadowComposeLegacy("24", time.Now())
		select {
		case gotDgram = <-regridCh:
		default:
		}
		if gotDgram != "" {
			break
		}
	}
	if gotDgram != wantDgram {
		t.Fatalf("native regrid datagram (edit 1) = %q, want %q", gotDgram, wantDgram)
	}
	// Steady polls with same gridGen must not send duplicate datagrams.
	for i := 0; i < 3; i++ {
		recv.shadowComposeLegacy("24", time.Now())
		time.Sleep(15 * time.Millisecond)
	}
	select {
	case msg := <-regridCh:
		t.Fatalf("duplicate regrid datagram on steady poll: %q", msg)
	default:
	}

	// Display-only receiver must pick up the shifted beat0=380ms.
	var dispKey2 string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(15 * time.Millisecond) {
		k, bpm, b0, ready := readDisp()
		if ready && k != dispKey1 && math.Abs(bpm-128) < 0.01 && math.Abs(b0-380) < 2 {
			dispKey2 = k
			break
		}
	}
	if dispKey2 == "" {
		t.Fatal("display-only shadow did not refresh beats after phase-only grid edit")
	}

	// Edit 2: tempo + phase change (128 -> 64 BPM, beat0 -> 600ms) via decksPost.
	updateGridTestDB(t, srcDB, 64, 600, 0)
	postSource(64)
	editedPeer2 := getPeerDecks()
	gen3, _ := editedPeer2[0]["gridGen"].(string)
	if gen3 == "" || gen3 == gen2 {
		t.Fatalf("gridGen did not change after tempo edit: %q -> %q", gen2, gen3)
	}
	feedPeers()

	var got3 deckState
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(15 * time.Millisecond) {
		for _, d := range recv.shadowComposeLegacy("24", time.Now()) {
			if d.Deck == 1 && d.GridGen == gen3 {
				got3 = d
			}
		}
		if got3.GridGen == gen3 {
			break
		}
	}
	if got3.GridGen != gen3 || got3.Path != copyPath {
		t.Fatalf("second grid edit failed or changed path: %+v (want path=%q gen=%q)", got3, copyPath, gen3)
	}
	select {
	case msg := <-regridCh:
		if msg != wantDgram {
			t.Fatalf("native regrid datagram (edit 2) = %q, want %q", msg, wantDgram)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for native regrid datagram on edit 2")
	}
	if hits := audioHits.Load(); hits != 1 {
		t.Fatalf("audio downloaded %d times after 2 edits, want 1", hits)
	}
	perf3, err := os.ReadFile(copyPath + ".perf")
	if err != nil {
		t.Fatalf("read updated .perf (edit 2): %v", err)
	}
	if bpm3, ok := expfBPM(perf3); !ok || math.Abs(bpm3-64) > 0.01 {
		t.Fatalf("updated .perf bpm=%v ok=%v, want 64", bpm3, ok)
	}

	// Display-only receiver must also pick up 64 BPM and beat0=600ms.
	var dispUpdated bool
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(15 * time.Millisecond) {
		k, bpm, b0, ready := readDisp()
		if ready && k != dispKey2 && math.Abs(bpm-64) < 0.01 && math.Abs(b0-600) < 2 {
			dispUpdated = true
			break
		}
	}
	if !dispUpdated {
		t.Fatal("display-only shadow did not refresh beats after tempo grid edit")
	}
}

// Verify steady-state playing cost (0 SQLite opens while m.db/m.db-wal are unchanged),
// hot-cue isolation (editing quickCues does not change GridGen), and phase-shift detection.
func TestShadowGridGenStatCacheAndCueIsolation(t *testing.T) {
	oldEvery := gridGenEvery
	gridGenEvery = 0
	defer func() { gridGenEvery = oldEvery }()

	src, _, dbPath, trackPath, _ := makeGridTestSource(t, 128, 250)
	ctx := context.Background()

	g1 := src.gridGen(ctx, trackPath)
	if g1 == "" {
		t.Fatal("expected non-empty initial gridGen")
	}
	q1 := src.shadow.gridGenQueries.Load()
	if q1 != 1 {
		t.Fatalf("initial gridGen opened SQLite %d times, want 1", q1)
	}
	// Repeated checks while playing with unchanged m.db/m.db-wal must open SQLite 0 times.
	for i := 0; i < 10; i++ {
		if got := src.gridGen(ctx, trackPath); got != g1 {
			t.Fatalf("gridGen drifted without DB change: %q != %q", got, g1)
		}
	}
	if q2 := src.shadow.gridGenQueries.Load(); q2 != q1 {
		t.Fatalf("unchanged m.db triggered %d extra SQLite opens", q2-q1)
	}

	// Hot-cue-only edit touches m.db, but (bpm, beatData) is identical -> same GridGen.
	updateGridTestDB(t, dbPath, 128, 250, 2000)
	if got := src.gridGen(ctx, trackPath); got != g1 {
		t.Fatalf("hot-cue-only edit changed gridGen: %q -> %q", g1, got)
	}

	// Phase-only beat-grid shift (same 128 BPM, shifted beat0 250ms -> 380ms) -> new GridGen.
	updateGridTestDB(t, dbPath, 128, 380, 2000)
	g2 := src.gridGen(ctx, trackPath)
	if g2 == "" || g2 == g1 {
		t.Fatalf("phase-only beat-grid shift did not change gridGen: %q -> %q", g1, g2)
	}
}

// Display-only source cache must refresh beatData on a new gridGen while keeping
// the already-decoded source-envelope detail waveform (no audio re-decode).
func TestShadowDisplayGridEditReusesDecodedWaveform(t *testing.T) {
	src, _, dbPath, trackPath, _ := makeGridTestSource(t, 128, 250)
	ctx := context.Background()

	a1, err := src.sourceDisplayAssetFor(ctx, trackPath, "k1-1", "g1", true)
	if err != nil || len(a1.Beats) == 0 || math.Abs(a1.Beats[0].MS-250) > 2 {
		t.Fatalf("initial display asset: %v %+v", err, a1)
	}
	// Inject a decoded source-envelope waveform into the source cache entry.
	env := &enginedb.DisplayWave{Bands: []byte{10, 20, 30, 40, 50, 60}, SamplesPerBin: 220500}
	src.shadow.sourceDisplay.mu.Lock()
	e := src.shadow.sourceDisplay.entries[stableTrackKey("k1-1")]
	e.asset.Waveform = env
	e.asset.DetailSource = "source-envelope"
	e.asset.Detail = true
	e.asset.DetailPending = false
	e.queued = false
	src.shadow.sourceDisplay.mu.Unlock()

	updateGridTestDB(t, dbPath, 64, 600, 0)
	a2, err := src.sourceDisplayAssetFor(ctx, trackPath, "k1-1", "g2", true)
	if err != nil {
		t.Fatalf("revalidated display asset: %v", err)
	}
	if math.Abs(a2.BPM-64) > 0.01 || len(a2.Beats) == 0 || math.Abs(a2.Beats[0].MS-600) > 2 {
		t.Fatalf("display asset did not pick up edited grid: bpm=%v beats=%+v", a2.BPM, a2.Beats)
	}
	if a2.Waveform == nil || a2.DetailSource != "source-envelope" || !bytes.Equal(a2.Waveform.Bands, env.Bands) {
		t.Fatalf("display revalidation discarded decoded waveform: %+v", a2.Waveform)
	}
	// Avoid unused import warning if binary is referenced later.
	var _ = binary.LittleEndian
}
