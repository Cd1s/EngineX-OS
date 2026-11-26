package webui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

func TestRekordboxOnDemandEndpointAndUnixSocket(t *testing.T) {
	ctx := context.Background()
	mediaRoot := t.TempDir()
	stick := filepath.Join(mediaRoot, "NEXTLIB")
	if err := os.MkdirAll(stick, 0o755); err != nil {
		t.Fatal(err)
	}
	testfixture.LargeExport(t, stick, 2)
	if _, err := bridge.ConvertAtMount(ctx, stick, bridge.MountOptions{}); err != nil {
		t.Fatalf("ConvertAtMount: %v", err)
	}
	dbDir := filepath.Join(stick, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	var tr *rbstate.Track
	for _, cand := range snap.Tracks {
		if cand.EngineID == 1 {
			tr = cand
			break
		}
	}
	if tr == nil {
		t.Fatal("missing track 1")
	}

	sockPath := filepath.Join(t.TempDir(), "rb-ondemand.sock")
	srv := New(Config{
		StateDir:              t.TempDir(),
		MediaRoot:             mediaRoot,
		RekordboxOnDemandSock: sockPath,
	}, nil)
	defer srv.Close()

	key0 := 0
	reqPayload, _ := json.Marshal(rbOnDemandRequest{
		DB:   filepath.Join(dbDir, "m.db"),
		UUID: snap.LibraryUUID,
		Path: "../" + tr.Rel,
		ID:   tr.EngineID,
		Key:  &key0,
		BPM:  128.0,
	})

	// 1. Non-loopback TCP request must be rejected with 403 Forbidden.
	reqRemote := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(reqPayload))
	reqRemote.RemoteAddr = "192.168.1.50:54321"
	recRemote := httptest.NewRecorder()
	srv.ServeHTTP(recRemote, reqRemote)
	if recRemote.Code != http.StatusForbidden {
		t.Fatalf("non-loopback status=%d, want 403", recRemote.Code)
	}

	// 2. Relayed loopback request (Cf-Connecting-Ip) must be rejected with 403 Forbidden.
	reqRelayed := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(reqPayload))
	reqRelayed.RemoteAddr = "127.0.0.1:54321"
	reqRelayed.Header.Set("Cf-Connecting-Ip", "192.168.1.50")
	recRelayed := httptest.NewRecorder()
	srv.ServeHTTP(recRelayed, reqRelayed)
	if recRelayed.Code != http.StatusForbidden {
		t.Fatalf("relayed status=%d, want 403", recRelayed.Code)
	}

	// 3. Valid loopback request via Server.ServeHTTP (tidal gateway /v1/ path) succeeds with RBXA1 + headers.
	reqLocal := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(reqPayload))
	reqLocal.RemoteAddr = "127.0.0.1:54321"
	recLocal := httptest.NewRecorder()
	srv.ServeHTTP(recLocal, reqLocal)
	if recLocal.Code != http.StatusOK {
		t.Fatalf("loopback status=%d body=%s", recLocal.Code, recLocal.Body.String())
	}
	assertValidOnDemandResponse(t, recLocal.Header(), recLocal.Body.Bytes(), tr)

	// 4. Unix domain socket serves the exact same RBXA1 + headers.
	unixClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
	}
	resp, err := unixClient.Post("http://unix"+RekordboxOnDemandRoute, "application/json", bytes.NewReader(reqPayload))
	if err != nil {
		t.Fatalf("unix POST: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unix status=%d body=%s", resp.StatusCode, string(body))
	}
	assertValidOnDemandResponse(t, resp.Header, body, tr)
	if !bytes.Equal(body, recLocal.Body.Bytes()) {
		t.Fatal("unix and loopback TCP RBXA1 bodies differ")
	}

	// 5. UUID mismatch -> 409 Conflict; missing track -> 404 Not Found; path escape -> 404 Not Found.
	badUUID, _ := json.Marshal(rbOnDemandRequest{Mount: stick, UUID: "other", Path: "../" + tr.Rel})
	reqBadUUID := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(badUUID))
	reqBadUUID.RemoteAddr = "127.0.0.1:1234"
	recBadUUID := httptest.NewRecorder()
	srv.ServeHTTP(recBadUUID, reqBadUUID)
	if recBadUUID.Code != http.StatusConflict {
		t.Fatalf("uuid mismatch status=%d, want 409", recBadUUID.Code)
	}

	missing, _ := json.Marshal(rbOnDemandRequest{Mount: stick, UUID: snap.LibraryUUID, Path: "../Contents/Nope.mp3"})
	reqMissing := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(missing))
	reqMissing.RemoteAddr = "127.0.0.1:1234"
	recMissing := httptest.NewRecorder()
	srv.ServeHTTP(recMissing, reqMissing)
	if recMissing.Code != http.StatusNotFound {
		t.Fatalf("missing track status=%d, want 404", recMissing.Code)
	}

	escape, _ := json.Marshal(rbOnDemandRequest{Mount: filepath.Join(mediaRoot, "..", "etc"), UUID: snap.LibraryUUID, Path: "../" + tr.Rel})
	reqEscape := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(escape))
	reqEscape.RemoteAddr = "127.0.0.1:1234"
	recEscape := httptest.NewRecorder()
	srv.ServeHTTP(recEscape, reqEscape)
	if recEscape.Code != http.StatusNotFound {
		t.Fatalf("mount escape status=%d, want 404", recEscape.Code)
	}

	// 6. Symlinked volume under mediaRoot must be rejected with 404 Not Found.
	symTarget := t.TempDir()
	symMount := filepath.Join(mediaRoot, "SYMLINK_USB")
	if err := os.Symlink(symTarget, symMount); err != nil {
		t.Fatal(err)
	}
	symReqBody, _ := json.Marshal(rbOnDemandRequest{Mount: symMount, UUID: snap.LibraryUUID, Path: "../" + tr.Rel})
	reqSym := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(symReqBody))
	reqSym.RemoteAddr = "127.0.0.1:1234"
	recSym := httptest.NewRecorder()
	srv.ServeHTTP(recSym, reqSym)
	if recSym.Code != http.StatusNotFound {
		t.Fatalf("symlink mount status=%d, want 404", recSym.Code)
	}
}

func assertValidOnDemandResponse(t *testing.T, h http.Header, body []byte, tr *rbstate.Track) {
	t.Helper()
	if !bytes.HasPrefix(body, []byte("RBXA1")) {
		t.Fatalf("missing RBXA1 magic (len=%d)", len(body))
	}
	perf, err := rbstate.DecodePerf(body)
	if err != nil {
		t.Fatalf("DecodePerf: %v", err)
	}
	if !perf.Complete || len(perf.TrackData) == 0 || len(perf.Beats) == 0 || len(perf.Overview) == 0 {
		t.Fatalf("incomplete perf decoded: complete=%v td=%d beats=%d ov=%d", perf.Complete, len(perf.TrackData), len(perf.Beats), len(perf.Overview))
	}
	if got := h.Get("X-RB-Key"); got != tr.Key {
		t.Fatalf("X-RB-Key=%q, want %q", got, tr.Key)
	}
	if got := h.Get("X-RB-Row"); got != tr.Row {
		t.Fatalf("X-RB-Row=%q, want %q", got, tr.Row)
	}
	if got := h.Get("X-RB-Rules"); got != strconv.Itoa(rbstate.Rules) {
		t.Fatalf("X-RB-Rules=%q, want %d", got, rbstate.Rules)
	}
	wr, err := base64.StdEncoding.DecodeString(h.Get("X-RB-Written"))
	if err != nil || len(wr) != 16 {
		t.Fatalf("X-RB-Written=%q err=%v len=%d", h.Get("X-RB-Written"), err, len(wr))
	}
	wrnm, err := base64.StdEncoding.DecodeString(h.Get("X-RB-Written-NoMain"))
	if err != nil || len(wrnm) != 16 {
		t.Fatalf("X-RB-Written-NoMain=%q err=%v len=%d", h.Get("X-RB-Written-NoMain"), err, len(wrnm))
	}
}

func TestBridgeDisplayWaveFallsBackToAnalysisPath(t *testing.T) {
	ctx := context.Background()
	stick := t.TempDir()
	testfixture.LargeExport(t, stick, 1)
	if _, err := bridge.ConvertAtMount(ctx, stick, bridge.MountOptions{}); err != nil {
		t.Fatalf("ConvertAtMount: %v", err)
	}
	dbDir := filepath.Join(stick, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	var tr *rbstate.Track
	for _, cand := range snap.Tracks {
		tr = cand
		break
	}
	if tr == nil || len(tr.Built.ANLZ) != 0 || tr.AnalysisPath == "" {
		t.Fatalf("expected unstaged track with empty Built.ANLZ and non-empty AnalysisPath, got %+v", tr)
	}
	// bridgeDisplayWave must derive .2EX/.EXT from AnalysisPath when no background pass populated Built.ANLZ.
	// Match the duration of the fixture's PWV7 detail waveform (~200 entries at 150 entries/s = ~1333 ms).
	mdb := filepath.Join(dbDir, "m.db")
	// Derive exact duration from the fixture's own .2EX file so the duration-match guard passes deterministically.
	ap := strings.TrimLeft(tr.AnalysisPath, "/")
	ex2Path := filepath.Join(stick, filepath.FromSlash(strings.TrimSuffix(ap, ".DAT")+".2EX"))
	if st, err := os.Stat(ex2Path); err != nil || st.Size() == 0 {
		t.Fatalf("expected fixture .2EX at %s: %v", ex2Path, err)
	}
	var w *enginedb.DisplayWave
	for _, durMS := range []float64{50, 100, 200, 500, 1000, 1333, 2000, 5000, 10000, 12000} {
		if got := bridgeDisplayWave(ctx, mdb, "../"+tr.Rel, tr.EngineID, 44100, durMS); got != nil {
			w = got
			break
		}
	}
	if w == nil || len(w.Bands) == 0 {
		t.Fatalf("bridgeDisplayWave returned nil for unstaged track with AnalysisPath=%q", tr.AnalysisPath)
	}
}

func TestLibraryDisplayReadsUnloadedOnDemandTrackAndDirectArtwork(t *testing.T) {
	ctx := context.Background()
	mediaRoot := t.TempDir()
	stick := filepath.Join(mediaRoot, "ONDEMAND_SHADOW")
	if err := os.MkdirAll(stick, 0o755); err != nil {
		t.Fatal(err)
	}
	testfixture.Export(t, stick)
	wave := make([]byte, 12+3*300) // 300 PWV7 bins = 2.000 s at 150 Hz, matching WAV(true)
	binary.BigEndian.PutUint32(wave, 3)
	binary.BigEndian.PutUint32(wave[4:], 300)
	for i := 0; i < 300; i++ {
		wave[12+3*i] = 120
		wave[12+3*i+1] = 80
		wave[12+3*i+2] = 40
	}
	testfixture.Write(t, stick, "PIONEER/USBANLZ/P001/ANLZ0000.2EX", testfixture.PMAI(testfixture.Section("PWV7", 24, wave)))
	if _, err := bridge.ConvertAtMount(ctx, stick, bridge.MountOptions{OnDemand: true}); err != nil {
		t.Fatalf("ConvertAtMount(OnDemand): %v", err)
	}
	artRel := "PIONEER/Artwork/00001/cover.jpg"
	artFull := filepath.Join(stick, filepath.FromSlash(artRel))
	if err := os.MkdirAll(filepath.Dir(artFull), 0o755); err != nil {
		t.Fatal(err)
	}
	var jpgBuf bytes.Buffer
	if err := jpeg.Encode(&jpgBuf, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artFull, jpgBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Join(stick, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	var tr *rbstate.Track
	for _, cand := range snap.Tracks {
		if cand.AnalysisPath != "" {
			tr = cand
			break
		}
	}
	if tr == nil {
		t.Fatal("missing analyzed track")
	}
	tr.ArtworkPath = "/" + artRel
	if err := snap.SaveNext(dbDir); err != nil {
		t.Fatal(err)
	}
	if err := rbstate.Promote(dbDir); err != nil {
		t.Fatal(err)
	}

	asset, err := libraryDisplay(ctx, filepath.Join(stick, filepath.FromSlash(tr.Rel)), t.TempDir(), mediaRoot)
	if err != nil {
		t.Fatalf("libraryDisplay on unloaded OnDemand track: %v", err)
	}
	if !asset.Detail || asset.DetailSource != "rekordbox" || len(asset.Beats) == 0 || asset.DurationMS <= 0 {
		t.Fatalf("unexpected OnDemand shadow display asset: detail=%v src=%q beats=%d dur=%.1f", asset.Detail, asset.DetailSource, len(asset.Beats), asset.DurationMS)
	}
	if !bytes.Equal(asset.Artwork, jpgBuf.Bytes()) {
		t.Fatalf("expected direct-read artwork bytes (%d), got %d", jpgBuf.Len(), len(asset.Artwork))
	}
}
