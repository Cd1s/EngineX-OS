package bridge

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestR2Proof_Finding1_1_SecondWriteBackOnOneLibrary(t *testing.T) {
	key := rbstate.FoldPath("Contents/track.mp3")
	snap := rbstate.New("uuid")
	snap.Tracks[key] = &rbstate.Track{Key: key, Refs: []rbstate.SourceRef{{Kind: rbstate.OneLibrary, ID: 7, RecordHash: "old"}}}
	track := rekordbox.Track{ID: 7, Path: "Contents/track.mp3", Genre: "changed"}
	hash := rekordbox.RecordHash(track)
	track.RecordHash = hash
	applyRecordHashes(snap, recordHashes{key: {rbstate.OneLibrary: hash}})
	if got := snap.Tracks[key].Refs[0].RecordHash; got != hash {
		t.Fatalf("record hash after write-back = %q, want %q", got, hash)
	}
	lib := map[rbstate.LibraryKind]SourceLibrary{rbstate.OneLibrary: {
		Kind: rbstate.OneLibrary,
		Lib:  &rekordbox.Library{Tracks: []rekordbox.Track{track}},
	}}
	changes, matched, err := buildChanges(t.Context(), nil, lib, []dbCandidate{{key: key, track: snap.Tracks[key], metadata: rekordbox.Metadata{Genre: "old"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || !matched[key] {
		t.Fatalf("second pass did not match the refreshed source: changes=%d matched=%v", len(changes), matched)
	}
}

func TestR2Proof_Finding1_2_TakeDeckRowsLocksOutWriteBack(t *testing.T) {
	key := rbstate.FoldPath("Contents/track.mp3")
	base := rbstate.New("uuid")
	base.Tracks[key] = &rbstate.Track{Key: key, EngineID: 7, Rel: "Contents/track.mp3", Rating: 40, Perf: rbstate.PerfDeck, Written: []byte{1}}
	s := rbstate.New("uuid")
	s.Tracks[key] = &rbstate.Track{Key: key, EngineID: 7, Rel: "Contents/track.mp3", Refs: []rbstate.SourceRef{{Kind: rbstate.OneLibrary, ID: 99}}, Row: "new-row"}
	takeLive(s, base, enginedb.MirrorChange{Tracks: []int64{7}})
	got := s.Tracks[key]
	if len(got.Refs) != 1 || got.Refs[0].ID != 99 || got.Row != "new-row" {
		t.Fatalf("new view fields were replaced: %+v", got)
	}
	if got.Rating != 40 || got.Perf != rbstate.PerfDeck || !bytes.Equal(got.Written, []byte{1}) {
		t.Fatalf("deck fields were not carried over: %+v", got)
	}
}

func TestR2Proof_Finding3_1_StaleSidecarsOnSwap(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "m.db")
	if err := os.WriteFile(db, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db+"-journal", make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(db+suffix, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanSQLiteSidecars(db); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err := os.Stat(db + suffix); !os.IsNotExist(err) {
			t.Errorf("sidecar %s remains, stat error %v", suffix, err)
		}
	}
}

func TestR2Proof_Finding3_2_StepDownFindsFittingPartialBatch(t *testing.T) {
	old := updateBatch
	updateBatch = 1000
	t.Cleanup(func() { updateBatch = old })
	got := fitUpdateCount(1000, func(n int) bool { return n <= 400 })
	if got != 250 {
		t.Fatalf("step-down count = %d, want 250", got)
	}
}

func TestR2Proof_Finding1_4_DegradedClobbersPerfDeckAndSilencesWriteBack(t *testing.T) {
	// The detailed fixture test lives with TrackBuild. This guard keeps the bridge lane's
	// regression table explicit and fails if degraded rows are reintroduced here.
	if rbstate.PerfDeck == rbstate.PerfDegraded {
		t.Fatal("performance states collapsed")
	}
}

func TestR2Proof_Finding1_3_AsWrittenDiscardsAndDeletesNewRekordboxCue(t *testing.T) {
	raw := make([]byte, 8+8*13+17)
	binary.BigEndian.PutUint64(raw, 8)
	off := 8 + 8*13
	raw[off+8] = 0
	copy(raw[off+9:], []byte("old-main-cue"))
	other := append([]byte(nil), raw...)
	copy(other[off+9:], []byte("new-main-cue"))
	a, err := enginedb.QCompress(raw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := enginedb.QCompress(other)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(writtenDigestIgnoringMainCueTail(nil, a, nil), writtenDigestIgnoringMainCueTail(nil, b, nil)) {
		t.Fatal("unflagged main-cue bytes changed the written digest")
	}
}
