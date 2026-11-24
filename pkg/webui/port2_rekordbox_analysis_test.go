package webui

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge/v2/bridgetest"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestPort2RekordboxLinkAnalysisMatchesBridge(t *testing.T) {
	for _, format := range []string{"mp3", "mp3 info", "aac"} {
		t.Run(format, func(t *testing.T) { p2AssertLinkMatchesBridge(t, format) })
	}
}

func p2AssertLinkMatchesBridge(t *testing.T, format string) {
	t.Helper()
	root := t.TempDir()
	var file []byte
	rel := testfixture.LargeTrackPath(1)
	if format == "aac" {
		rel, file = testfixture.AACExport(t, root), testfixture.AACWithPriming()
	} else {
		testfixture.LargeExport(t, root, 1)
		file = testfixture.MP3WithLAMETag()
		if format == "mp3 info" {
			copy(file[36:], "Info")
		}
		file = append(file, bytes.Repeat(file[417:834], 100)...)
		binary.BigEndian.PutUint32(file[44:], 102)
		binary.BigEndian.PutUint32(file[48:], uint32(len(file)))
		testfixture.Write(t, root, rel, file)
	}
	bridgetest.Import(t, root)

	db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library/Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	want := make([][]byte, 5)
	var analysed int
	var locked sql.NullBool
	err = db.QueryRow(`SELECT p.trackData,p.overviewWaveFormData,p.beatData,p.quickCues,p.loops,t.isAnalyzed,t.isBeatGridLocked
		FROM Track t JOIN PerformanceData p ON p.trackId=t.id WHERE t.pdbImportKey=1`).
		Scan(&want[0], &want[1], &want[2], &want[3], &want[4], &analysed, &locked)
	_ = db.Close()
	if err != nil || analysed != 1 || locked.Bool || len(want[2]) == 0 || len(want[3]) == 0 {
		t.Fatalf("bridge row: %v analysed=%d locked=%v", err, analysed, locked)
	}

	lib, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	a := &rekordbox.Analysis{}
	for _, raw := range [][]byte{testfixture.DAT(), testfixture.EXT(), testfixture.EX2()} {
		part, err := rekordbox.ParseAnalysis(raw)
		if err != nil {
			t.Fatal(err)
		}
		a.Merge(part)
	}
	var one rekordbox.Track
	for _, tr := range lib.Tracks {
		if tr.ID == 1 {
			one = tr
		}
	}
	paths := map[uint32]string{1: "/" + rel}
	e := newRbxEnv(t, paths)
	nfs := &prolink.NFSServer{Root: root}
	if err := nfs.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nfs.Close)
	oldExport := rbxExport
	t.Cleanup(func() { rbxExport = oldExport })
	rbxExport = "/C/"
	rbxNFS = func(ip net.IP) *prolink.NFSClient {
		return &prolink.NFSClient{IP: ip, Portmap: nfs.PortmapPort(), Timeout: time.Second}
	}
	prefix, audio, err := rbxNFS(net.IPv4(127, 0, 0, 1)).FetchPrefix(t.Context(), rbxExport, paths[1], 32<<10)
	if err != nil || audio.Size != uint32(len(file)) || !bytes.Equal(prefix, file[:32<<10]) {
		t.Fatalf("NFS prefix: %d of %d bytes: %v", len(prefix), len(file), err)
	}
	link := rb7Link{fakeRbxLink{paths}, one, a, map[string][]byte{"DAT": testfixture.DAT(), "EXT": testfixture.EXT(), "2EX": testfixture.EX2()}}
	dialRekordbox = func(context.Context, net.IP, byte) (rbxLink, error) { return link, nil }
	old := rbxAnalysisDir
	rbxAnalysisDir = t.TempDir()
	t.Cleanup(func() { rbxAnalysisDir = old })
	t.Cleanup(rbxAnalysisJobs.Wait)
	ids, _ := e.trackIDs(t, "Tracks")
	b, err := os.ReadFile(rbxAnalysisPath(ids["First"]))
	if err != nil {
		t.Fatalf("analysis not ready: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rbxAnalysisDir, ".loading")); err != nil {
		t.Fatalf("analysis hook not enabled: %v", err)
	}
	if b[len(b)-1] != byte(analysed) {
		t.Errorf("Link analysed flag %d differs from bridge %d", b[len(b)-1], analysed)
	}
	got := rbxBlobs(b)
	if len(got) != 5 {
		t.Fatalf("analysis blobs: %d", len(got))
	}
	for i, name := range []string{"trackData", "overviewWaveFormData", "beatData", "quickCues", "loops"} {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("%s differs: got %d bytes, bridge %d", name, len(got[i]), len(want[i]))
		}
	}
	replay, err := os.ReadFile(rbxAnalysisPath(ids["First"]) + ".perf")
	if err != nil {
		t.Fatal(err)
	}
	expectedReplay, err := perfFromBlobs(one.BPM, want[0], want[2], want[1])
	if err != nil || !bytes.Equal(replay, expectedReplay) {
		t.Fatalf("download replay differs: %v", err)
	}
}
