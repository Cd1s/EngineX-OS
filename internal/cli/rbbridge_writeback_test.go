package cli

import (
	"bytes"
	"encoding/json"
	"github.com/Cd1s/EngineX-OS/pkg/bridge/bridgetest"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
)

// #321: the Eject's write-back (the edisksd hook runs rb-bridge --writeback-cues --on-eject)
// is stream-sync's final pass, with the keys its watcher kept; rb-bridge does it itself only when
// stream-sync does not answer.
func TestRBBridgeOnEjectHandsThePassToStreamSync(t *testing.T) {
	media := t.TempDir()
	root := filepath.Join(media, "STICK")
	testfixture.Export(t, root)
	var out bytes.Buffer
	if err := RBBridge([]string{"--mount-path", root}, &out); err != nil {
		t.Fatalf("initial mount import: %v", err)
	}
	bridgetest.Import(t, root)
	var asked atomic.Int32
	var query string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tracksChecked":77,"tracksRead":0,"tracksChanged":0,"filesUpdated":0,"oneLibraryUpdated":false}`))
	}))
	oldGW, oldMedia := writeBackGateway, writeBackMedia
	writeBackGateway, writeBackMedia = gw.URL, media
	defer func() { writeBackGateway, writeBackMedia = oldGW, oldMedia }()

	out.Reset()
	if err := RBBridge([]string{"--mount-path", root, "--writeback-cues", "--on-eject"}, &out); err != nil {
		t.Fatalf("writeback --on-eject: %v", err)
	}
	var res struct {
		TracksChecked int `json:"tracksChecked"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || asked.Load() != 1 || res.TracksChecked != 77 || query != "final=1&drive=STICK" {
		t.Fatalf("not stream-sync's pass: asked %d, query %q, out %q, %v", asked.Load(), query, &out, err)
	}

	// No stream-sync behind the port: rb-bridge's own pass.
	gw.Close()
	out.Reset()
	if err := RBBridge([]string{"--mount-path", root, "--writeback-cues", "--on-eject"}, &out); err != nil {
		t.Fatalf("writeback --on-eject without stream-sync: %v", err)
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.TracksChecked == 0 || res.TracksChecked == 77 {
		t.Fatalf("own pass: %q %v", &out, err)
	}
}

// #321 review R2: stream-sync took the Eject's pass and has not finished when rb-bridge's wait
// runs out. rb-bridge reports it and does no pass of its own (the hook's budget is spent); its
// request ends, so stream-sync's pass is cancelled at its next check.
func TestRBBridgeOnEjectGivesUpOnAPassThatOverruns(t *testing.T) {
	media := t.TempDir()
	root := filepath.Join(media, "STICK")
	testfixture.Export(t, root)
	var out bytes.Buffer
	if err := RBBridge([]string{"--mount-path", root}, &out); err != nil {
		t.Fatalf("initial mount import: %v", err)
	}
	ended := make(chan time.Duration, 1)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		select { // a pass that outlasts the wait
		case <-r.Context().Done():
			ended <- time.Since(t0)
		case <-time.After(10 * time.Second):
			ended <- -1
		}
	}))
	defer gw.Close()
	oldGW, oldMedia, oldWait := writeBackGateway, writeBackMedia, writeBackWait
	writeBackGateway, writeBackMedia, writeBackWait = gw.URL, media, 300*time.Millisecond
	defer func() { writeBackGateway, writeBackMedia, writeBackWait = oldGW, oldMedia, oldWait }()

	out.Reset()
	t0 := time.Now()
	err := RBBridge([]string{"--mount-path", root, "--writeback-cues", "--on-eject"}, &out)
	took := time.Since(t0)
	if err == nil || !strings.Contains(err.Error(), "stream-sync did not finish within 300ms") {
		t.Fatalf("an overrun reported as %v", err)
	}
	if out.Len() != 0 || took > 2*time.Second {
		t.Fatalf("rb-bridge ran a pass of its own (%q) or waited %s", &out, took)
	}
	select {
	case d := <-ended:
		if d < 0 {
			t.Fatal("stream-sync's request was not ended when rb-bridge gave up")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream-sync's request still open")
	}
}
