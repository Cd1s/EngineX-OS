package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

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
