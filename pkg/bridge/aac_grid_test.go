package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestAACImportPreservesExportTiming(t *testing.T) {
	for _, tc := range []struct {
		name            string
		fixture         func([]byte) []byte
		priming         float64
		wantSampleCount uint64
		wantBeat0       float64
		wantBeat1       float64
		wantHotB        float64
		wantLoop0Start  float64
	}{
		{name: "truncated moov", fixture: func(b []byte) []byte { return b[:300] }, wantSampleCount: 352800, wantBeat1: 22050, wantHotB: 22050, wantLoop0Start: 33075},
		{name: "HE-AAC core rate", priming: 2048, wantSampleCount: 352800, wantBeat0: -2048, wantBeat1: 20002, wantHotB: 20002, wantLoop0Start: 31027, fixture: func(b []byte) []byte {
			entry := bytes.Index(b, []byte("mp4a"))
			binary.BigEndian.PutUint32(b[entry+4+24:], 22050<<16)
			return b
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			rel := testfixture.AACExport(t, root)
			testfixture.Write(t, root, rel, tc.fixture(testfixture.AACWithPriming()))
			if _, err := bridge.ConvertAtMount(t.Context(), root, enginedb.Options{}, false); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			got := readTrackPositions(t, db, 1)
			if got.trackSampleRate != 44100 || got.trackSampleCount != tc.wantSampleCount || got.beat0 != tc.wantBeat0 || got.beat1 != tc.wantBeat1 ||
				got.hotB != tc.wantHotB || got.loop0Start != tc.wantLoop0Start {
				t.Fatalf("want 44.1 kHz export grid, cue and loop with %v samples of priming; got %+v", tc.priming, got)
			}
		})
	}
}

func TestAACGridMigratesAnUnchangedV2Export(t *testing.T) {
	root := t.TempDir()
	rel := testfixture.AACExport(t, root)
	audio := filepath.Join(root, rel)
	media := testfixture.AACWithPriming()
	// The old bridge did not use the AAC header. Keep the file's size and
	// mtime unchanged so the upgrade must be detected by the stamp.
	testfixture.Write(t, root, rel, make([]byte, len(media)))
	st, err := os.Stat(audio)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "Engine Library", "Database2")
	stamp := filepath.Join(dir, ".rb-bridge-stamp")
	v, err := os.ReadFile(stamp)
	if err != nil || len(v) < 2 {
		t.Fatalf("initial stamp: %q, %v", v, err)
	}
	copy(v, "v2")
	if err = os.WriteFile(stamp, v, 0o644); err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, root, rel, media)
	if err = os.Chtimes(audio, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || res.Bypassed {
		t.Fatalf("v2 upgrade skipped: %+v, %v", res, err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := readTrackPositions(t, db, 1); got.beat0 != -2048 {
		t.Fatalf("first beat %v samples, want -2048", got.beat0)
	}
	v, err = os.ReadFile(stamp)
	if err != nil || !bytes.HasPrefix(v, []byte("v4 ")) {
		t.Fatalf("upgraded stamp: %q, %v", v, err)
	}
}
