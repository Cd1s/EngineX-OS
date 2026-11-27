package rbstate

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func sampleSnapshot() *Snapshot {
	s := New("11111111-2222-3333-4444-555555555555")
	s.Layout = LayoutFolders
	s.Sources = []FileSig{{Rel: "PIONEER/rekordbox/export.pdb", Size: 10, MTimeNS: 20}, {Rel: "PIONEER/rekordbox/exportLibrary.db", Size: 30, MTimeNS: 40}}
	s.Stamp = Stamp(s.Layout, s.Sources, "")
	s.Pending = Pending{Perf: 1, Verify: true}
	s.Tracks["contents/a/one.mp3"] = &Track{
		Key: "contents/a/one.mp3", EngineID: 12, Rel: "Contents/A/One.mp3",
		AnalysisPath: "/PIONEER/USBANLZ/P001/00000001/ANLZ0000.DAT",
		ArtworkPath:  "/PIONEER/Artwork/00001/a1.jpg",
		Resolved:     true,
		Refs:         []SourceRef{{Kind: DeviceLibrary, ID: 5, RecordHash: "aa"}, {Kind: OneLibrary, ID: 9, RecordHash: "bb"}},
		Row:          "r1", Perf: PerfApplied, Written: []byte{1, 2, 3},
		Built: TrackSig{Rules: Rules, Row: "r1", Audio: FileSig{Rel: "Contents/A/One.mp3", Size: 1, MTimeNS: 2}, ANLZ: []FileSig{{Rel: "x.DAT", Size: 3, MTimeNS: 4}}},
	}
	s.Tracks["contents/b/<two>&.flac"] = &Track{Key: "contents/b/<two>&.flac", Rel: "Contents/B/<two>&.flac"}
	s.Lists["\x00\x00Set"] = &List{Key: "\x00\x00Set", EngineID: 3, Hash: "h"}
	return s
}

func TestSnapshotRoundTripAndHeader(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadHeader(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no snapshot: %v", err)
	}
	want := sampleSnapshot()
	if err := want.Save(dir); err != nil {
		t.Fatal(err)
	}
	h, err := ReadHeader(dir)
	if err != nil {
		t.Fatal(err)
	}
	if h.Stamp != want.Stamp || h.Tracks != 2 || h.Lists != 1 || h.Pending != want.Pending || h.LibraryUUID != want.LibraryUUID {
		t.Fatalf("header %+v", h)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := want.Encode()
	b, _ := got.Encode()
	if !bytes.Equal(a, b) {
		t.Fatalf("round trip differs:\n%s\n%s", a, b)
	}
	tr, ok := got.TrackAt("../Contents/A/ONE.mp3")
	if !ok || tr.EngineID != 12 || !tr.Built.Equal(want.Tracks["contents/a/one.mp3"].Built) ||
		tr.AnalysisPath != "/PIONEER/USBANLZ/P001/00000001/ANLZ0000.DAT" ||
		tr.ArtworkPath != "/PIONEER/Artwork/00001/a1.jpg" {
		t.Fatalf("TrackAt: %+v %v", tr, ok)
	}
	if ids := got.EngineIDs(); len(ids) != 1 || !ids[12] {
		t.Fatalf("EngineIDs %v", ids)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("a temp file was left: %v", entries)
	}
}

// A snapshot that is cut short, or whose bytes changed, is no snapshot: the mount then builds it again.
func TestSnapshotCutOrChangedIsAbsent(t *testing.T) {
	dir := t.TempDir()
	whole, _ := sampleSnapshot().Encode()
	p := filepath.Join(dir, SnapshotName)
	for name, b := range map[string][]byte{
		"empty":       {},
		"no end line": whole[:bytes.LastIndex(whole[:len(whole)-1], []byte{'\n'})+1],
		"cut in line": whole[:len(whole)/2],
		"no newline":  whole[:len(whole)-1],
	} {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadHeader(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: ReadHeader %v", name, err)
		}
		if _, err := Load(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: Load %v", name, err)
		}
	}
	// Any one byte changed, wherever it is: the header, a record, the end line.
	for at := range whole {
		changed := bytes.Clone(whole)
		changed[at] ^= 0x01
		if err := os.WriteFile(p, changed, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("byte %d of %d changed and the snapshot still loads: %v", at, len(whole), err)
		}
	}
}

func TestFoldPath(t *testing.T) {
	for in, want := range map[string]string{
		"../Contents/A/One.MP3": "contents/a/one.mp3",
		"/Contents/A/One.MP3":   "contents/a/one.mp3",
		`Contents\A\One.MP3`:    "contents/a/one.mp3",
		"contents/a/one.mp3":    "contents/a/one.mp3",
		"Contents/Café/É":     "contents/café/é", // NFD and NFC name one file
		"Contents/CAFÉ/É":       "contents/café/é",
		"Contents/Straße.mp3":   "contents/strasse.mp3", // full case folding, as pkg/compat resolves
	} {
		if got := FoldPath(in); got != want {
			t.Errorf("FoldPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStampNamesRulesLayoutAndFiles(t *testing.T) {
	files := []FileSig{{Rel: "PIONEER/rekordbox/export.pdb", Size: 1, MTimeNS: 2}}
	a := Stamp(LayoutFolders, files, "")
	if Stamp(LayoutFolders, nil, "") != "" {
		t.Fatal("a stick without exports has a stamp")
	}
	for name, b := range map[string]string{
		"layout": Stamp(LayoutMerged, files, ""),
		"size":   Stamp(LayoutFolders, []FileSig{{Rel: files[0].Rel, Size: 3, MTimeNS: 2}}, ""),
		"time":   Stamp(LayoutFolders, []FileSig{{Rel: files[0].Rel, Size: 1, MTimeNS: 3}}, ""),
		"staged": Stamp(LayoutFolders, files, "cdj"),
	} {
		if a == b {
			t.Errorf("%s does not change the stamp %q", name, a)
		}
	}
}

func TestReplaceFileLeavesTheOldFileWhenItMayNotBeFreed(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, SnapshotName)
	if err := ReplaceFile(dst, []byte("old")); err != nil {
		t.Fatal(err)
	}
	UnsafeToFree = func(p string) bool { return p == dst }
	defer func() { UnsafeToFree = nil }()
	if err := ReplaceFile(dst, []byte("new")); !errors.Is(err, ErrUnownedClusters) {
		t.Fatalf("replace: %v", err)
	}
	if err := RemoveFile(dst); !errors.Is(err, ErrUnownedClusters) {
		t.Fatalf("remove: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "old" {
		t.Fatalf("dst is %q", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
}

// At every point a pull can separate, dst reads whole: the old bytes, then the new.
func TestReplaceFileIsWholeAtEveryCut(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "f")
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	var seen []string
	cut = func(step string) {
		b, err := os.ReadFile(dst)
		if err != nil {
			t.Errorf("%s: %v", step, err)
		}
		seen = append(seen, string(b))
	}
	defer func() { cut = func(string) {} }()
	if err := ReplaceFile(dst, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || seen[0] != "old" || seen[1] != "new" || seen[2] != "new" {
		t.Fatalf("at the cuts: %q", seen)
	}
	// What a cut before the rename leaves is cleaned at the next window.
	if err := os.WriteFile(filepath.Join(dir, ".enginex-rb-123.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := CleanTemps(dir); n != 1 {
		t.Fatalf("CleanTemps removed %d", n)
	}
}

func TestLockVolumeExcludesAndWaits(t *testing.T) {
	mount := filepath.Join(t.TempDir(), "stick")
	if err := os.Mkdir(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockVolume(mount, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = LockVolume(mount, 0); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	if entries, _ := os.ReadDir(mount); len(entries) != 0 {
		t.Fatalf("the lock is a file on the stick: %v", entries)
	}
	go func() { time.Sleep(60 * time.Millisecond); unlock() }()
	again, err := LockVolume(mount, 5*time.Second)
	if err != nil {
		t.Fatalf("waiting lock: %v", err)
	}
	again()
}

func TestPerfFileRoundTripAndBounds(t *testing.T) {
	p := Perf{TrackData: []byte{1}, Overview: []byte{2, 3}, Beats: []byte{4, 5, 6}, QuickCues: nil, Loops: []byte{7}, Complete: true}
	b := EncodePerf(p)
	// The layout tools/ui-mod/rekordbox_analysis.h reads: magic, five length-prefixed blobs, one flag byte.
	want := append([]byte("RBXA1"), 1, 0, 0, 0, 1, 2, 0, 0, 0, 2, 3, 3, 0, 0, 0, 4, 5, 6, 0, 0, 0, 0, 1, 0, 0, 0, 7, 1)
	if !bytes.Equal(b, want) {
		t.Fatalf("layout:\n% x\n% x", b, want)
	}
	got, err := DecodePerf(b)
	if err != nil || !bytes.Equal(got.Beats, p.Beats) || !got.Complete || got.QuickCues != nil {
		t.Fatalf("decode: %+v %v", got, err)
	}
	for _, bad := range [][]byte{nil, b[:len(b)-1], append(append([]byte(nil), b...), 0), append([]byte("RBXA1"), 0xff, 0xff, 0xff, 0x7f)} {
		if _, err := DecodePerf(bad); err == nil {
			t.Errorf("accepted % x", bad)
		}
	}
}

func TestStagePutGetDropAndTornIndex(t *testing.T) {
	t.Setenv(StageEnv, t.TempDir())
	s, err := OpenStage("lib-1")
	if err != nil {
		t.Fatal(err)
	}
	sig := TrackSig{Rules: Rules, Row: "r", Audio: FileSig{Rel: "Contents/a.mp3", Size: 1, MTimeNS: 2}}
	a := &Staged{Key: "contents/a.mp3", Sig: sig, Rel: "Contents/a.mp3", Facts: Facts{SampleRate: 44100, Duration: 1.5},
		Perf: Perf{TrackData: []byte{1}, Beats: []byte{2}, ActiveLoops: 5, Complete: true}, RGB: []byte("rgb")}
	b := &Staged{Key: "contents/b.mp3", Sig: sig, Missing: true}
	c := &Staged{Key: "contents/c.mp3", Sig: sig, Degraded: "audio header", Art: []byte("jpg")}
	for _, st := range []*Staged{a, b, c} {
		if err = s.Put(st); err != nil {
			t.Fatal(err)
		}
	}
	// A newer Put of a key replaces the older one.
	a.Sig.Row = "r2"
	if err = s.Put(a); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkVerified("stamp-1"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	// A line the power cut tore is ignored.
	f, _ := os.OpenFile(filepath.Join(s.Dir(), stageIndex), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"k":"contents/d.mp3","sig":{"a"`)
	f.Close()

	s, err = OpenStage("lib-1")
	if err != nil {
		t.Fatal(err)
	}
	idx, _, err := s.Entries()
	if err != nil || len(idx) != 3 || idx[a.Key].Sig.Row != "r2" {
		t.Fatalf("index %v %v", idx, err)
	}
	if s.Verified() != "stamp-1" {
		t.Fatalf("verified %q", s.Verified())
	}
	got, err := s.Get(a.Key)
	if err != nil || !bytes.Equal(got.Perf.Beats, []byte{2}) || got.Perf.ActiveLoops != 5 || !got.Perf.Complete || string(got.RGB) != "rgb" || got.Facts.SampleRate != 44100 {
		t.Fatalf("get a: %+v %v", got, err)
	}
	if got, err = s.Get(b.Key); err != nil || !got.Missing || got.HasPerf {
		t.Fatalf("get b: %+v %v", got, err)
	}
	if got, err = s.Get(c.Key); err != nil || got.Degraded == "" || string(got.Art) != "jpg" {
		t.Fatalf("get c: %+v %v", got, err)
	}
	entries, verified, err := s.Entries()
	if err != nil || len(entries) != 3 || verified != "stamp-1" || entries[a.Key].Perf.Beats != nil {
		t.Fatalf("entries: %v %q %v", entries, verified, err)
	}
	if full, err := s.Read(entries[a.Key]); err != nil || len(full.Perf.Beats) != 1 {
		t.Fatalf("read: %+v %v", full, err)
	}
	// A staged track whose performance file is gone cannot be read: the window drops it, and
	// the job stages it again.
	if err = os.Remove(filepath.Join(s.Dir(), stageName(a.Key)+".bin")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(a.Key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("get of a lost file: %v", err)
	}
	if _, err = s.Read(idx[a.Key]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read of a lost file: %v", err)
	}
	if err = s.Drop(a.Key, b.Key, c.Key); err != nil {
		t.Fatal(err)
	}
	if idx, _, _ = s.Entries(); len(idx) != 0 {
		t.Fatalf("after drop: %v", idx)
	}
	if _, err = s.Get(c.Key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("get after drop: %v", err)
	}
	if err = s.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(s.Dir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the stage is still there")
	}
}

func TestStageRootInTestsIsPrivateAndNamesAreSafe(t *testing.T) {
	t.Setenv(StageEnv, "")
	root, err := StageRoot()
	if err != nil || root == defaultStageRoot || !strings.HasPrefix(root, os.TempDir()) {
		t.Fatalf("a test's stage root is %q (%v)", root, err)
	}
	t.Setenv(StageEnv, t.TempDir())
	for _, bad := range []string{"", "..", "a/b", ".hidden"} {
		if _, err := OpenStage(bad); err == nil {
			t.Errorf("OpenStage(%q) worked", bad)
		}
	}
}

// The next snapshot is a claim, not the snapshot: readers of the snapshot never see it, and a cut
// one is no claim at all.
func TestNextSnapshotIsSeparateUntilPromoted(t *testing.T) {
	dir := t.TempDir()
	old := sampleSnapshot()
	if err := old.Save(dir); err != nil {
		t.Fatal(err)
	}
	next := sampleSnapshot()
	next.Tracks["contents/new.mp3"] = &Track{Key: "contents/new.mp3", EngineID: 99, Rel: "Contents/new.mp3"}
	if err := next.SaveNext(dir); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(dir); err != nil || len(got.Tracks) != 2 {
		t.Fatalf("the snapshot changed before the promote: %v", err)
	}
	if claims, err := LoadNext(dir); err != nil || claims.Tracks["contents/new.mp3"].EngineID != 99 {
		t.Fatalf("claims: %v", err)
	}
	if err := Promote(dir); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(dir); err != nil || len(got.Tracks) != 3 {
		t.Fatalf("after the promote: %v", err)
	}
	if _, err := LoadNext(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a next snapshot is left: %v", err)
	}
	whole, _ := next.Encode()
	if err := os.WriteFile(filepath.Join(dir, nextName), whole[:len(whole)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadNext(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a cut next snapshot is a claim: %v", err)
	}
	if err := DropNext(dir); err != nil {
		t.Fatal(err)
	}
	if err := DropNext(dir); err != nil {
		t.Fatalf("dropping nothing: %v", err)
	}
}

// R1 and R2 of the deck's UI hook, which fills a row a deck loads before its window: an index
// line names the track's row, and the hook's own record of what it filled is read by key.
func TestStageLineNamesTheRowAndFilledReadsTheHooksRecord(t *testing.T) {
	t.Setenv(StageEnv, t.TempDir())
	s, err := OpenStage("lib-1")
	if err != nil {
		t.Fatal(err)
	}
	sig := TrackSig{Rules: Rules, Row: "r"}
	for _, st := range []*Staged{{Key: "contents/a.mp3", EngineID: 41, Sig: sig}, {Key: "contents/b.mp3", Sig: sig}} { // b has no row yet
		if err = s.Put(st); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(s.Dir(), stageIndex))
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"k":"contents/a.mp3","id":41,`) || strings.Contains(lines[1], `"id"`) {
		t.Fatalf("index lines: %q", lines)
	}
	entries, _, err := s.Entries()
	if err != nil || entries["contents/a.mp3"].EngineID != 41 || entries["contents/b.mp3"].EngineID != 0 {
		t.Fatalf("entries: %+v %v", entries, err)
	}
	if got := s.Filled(); len(got) != 0 {
		t.Fatalf("without the hook's file: %+v", got)
	}
	// The lines as tools/ui-mod/rekordbox_stage.h writes them: the last of a key counts, a torn one is skipped.
	hook := `{"id":41,"k":"contents/a.mp3","wr":"AQIDBAUGBwgJCgsMDQ4PEA==","v":5,"w":"r","at":1795570000}` + "\n" +
		`{"id":41,"k":"contents/a.mp3","wr":"EBESExQVFhcYGRobHB0eHw==","v":5,"w":"r2","at":1795570100}` + "\n" + `{"id":7,"k":"contents/c.mp3","wr":"AQ`
	if err = os.WriteFile(filepath.Join(s.Dir(), stageFilled), []byte(hook), 0o600); err != nil {
		t.Fatal(err)
	}
	got := s.Filled()
	if a := got["contents/a.mp3"]; len(got) != 1 || a.EngineID != 41 || !bytes.Equal(a.Written, []byte{16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31}) {
		t.Fatalf("the hook's record: %+v", got)
	}
}

func TestInstallFilePacedAbortsCleanlyWhenBusy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	dst := filepath.Join(dir, "dst.db")
	payload := bytes.Repeat([]byte("A"), 256<<10)
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	errBusy := errors.New("deck active")
	chunks := 0
	err := InstallFilePaced(src, dst, 64<<10, 128<<10, func(n int, _ time.Duration) error {
		if n > 0 {
			chunks++
			if chunks == 2 {
				return errBusy
			}
		}
		return nil
	})
	if !errors.Is(err, errBusy) {
		t.Fatalf("mid-copy abort: err=%v, want %v", err, errBusy)
	}
	if _, serr := os.Stat(dst); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("dst created on aborted copy: %v", serr)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".enginex-rb-") {
			t.Fatalf("leftover temp file after abort: %s", e.Name())
		}
	}
	if err := InstallFilePaced(src, dst, 64<<10, 128<<10, nil); err != nil {
		t.Fatalf("idle InstallFilePaced: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("installed content mismatch: err=%v len=%d", err, len(got))
	}
}

func TestReplaceFileCtxHonorsDeadlineAtEveryCutPoint(t *testing.T) {
	payload := bytes.Repeat([]byte("Z"), 192<<10) // >= 3 chunks of 64 KiB so "write" cut fires mid-write
	for _, targetCut := range []string{"write", "dirsync", "filesync", "temp"} {
		t.Run(targetCut, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "snapshot.bin")
			if err := os.WriteFile(dst, []byte("original-state"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hit := false
			hook := func(step string) {
				if strings.HasPrefix(step, targetCut+" ") {
					hit = true
					cancel()
				}
			}
			restore := SetCutHooks(hook, hook)
			defer restore()

			err := ReplaceFileCtx(ctx, dst, payload)
			if !hit {
				t.Fatalf("cut point %q never fired", targetCut)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("ReplaceFileCtx at cut %q err=%v, want context.Canceled", targetCut, err)
			}
			if got, rerr := os.ReadFile(dst); rerr != nil || string(got) != "original-state" {
				t.Fatalf("dst corrupted after canceled %q: got=%q err=%v", targetCut, got, rerr)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".enginex-rb-") {
					t.Fatalf("leftover temp file after cut %q: %s", targetCut, e.Name())
				}
			}
		})
	}
}

func TestSnapshotFormat2CompatibilityAndCancelMidDecode(t *testing.T) {
	// 1. Over-512 snapshot remains a single Format-2 gob body readable by target decoders.
	s513 := New("11111111-2222-3333-4444-555555555555")
	for i := 0; i < 513; i++ {
		k := fmt.Sprintf("list/%04d", i)
		s513.Lists[k] = &List{Key: k, EngineID: int64(i + 1), Hash: "h"}
		rel := fmt.Sprintf("Music/Track-%05d.mp3", i+1)
		tk := FoldPath(rel)
		s513.Tracks[tk] = &Track{Key: tk, EngineID: int64(i + 1), Rel: rel}
	}
	raw513, err := s513.Encode()
	if err != nil {
		t.Fatal(err)
	}
	end := lastLine(raw513)
	body513 := raw513[:len(raw513)-len(end)-1]
	_, rest513, _ := bytes.Cut(body513[:len(body513)-1], []byte{'\n'})
	var legacyRec snapshotBody
	if err := gob.NewDecoder(bytes.NewReader(rest513)).Decode(&legacyRec); err != nil {
		t.Fatalf("legacy single-body decode failed on 513-track snapshot: %v", err)
	}
	if len(legacyRec.Lists) != 513 || len(legacyRec.Tracks) != 513 {
		t.Fatalf("legacy decoded counts: lists=%d tracks=%d", len(legacyRec.Lists), len(legacyRec.Tracks))
	}
	got513, err := LoadReaderContext(context.Background(), bytes.NewReader(raw513))
	if err != nil || len(got513.Lists) != 513 || len(got513.Tracks) != 513 {
		t.Fatalf("LoadReaderContext 513 mismatch: err=%v lists=%d tracks=%d", err, len(got513.Lists), len(got513.Tracks))
	}

	// 1b. Reader also accepts R3-style multi-gob chunked 513-track snapshots.
	s513.Format, s513.Rules = Format, Rules
	s513.Header.Tracks, s513.Header.Lists = len(s513.Tracks), len(s513.Lists)
	head513, err := json.Marshal(&s513.Header)
	if err != nil {
		t.Fatal(err)
	}
	var multi bytes.Buffer
	multi.WriteString(snapshotMagic)
	multi.Write(head513)
	multi.WriteByte('\n')
	enc := gob.NewEncoder(&multi)
	listSlice := make([]*List, 0, len(s513.Lists))
	for _, l := range s513.Lists {
		listSlice = append(listSlice, l)
	}
	if err := enc.Encode(snapshotBody{Lists: listSlice}); err != nil {
		t.Fatal(err)
	}
	trackSlice := make([]*Track, 0, len(s513.Tracks))
	for _, tr := range s513.Tracks {
		trackSlice = append(trackSlice, tr)
	}
	if err := enc.Encode(snapshotBody{Tracks: trackSlice[:512]}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(snapshotBody{Tracks: trackSlice[512:]}); err != nil {
		t.Fatal(err)
	}
	multi.WriteByte('\n')
	fmt.Fprintf(&multi, "E %d %08x\n", len(listSlice)+len(trackSlice), crc32.ChecksumIEEE(multi.Bytes()))
	gotMulti, err := LoadReaderContext(context.Background(), bytes.NewReader(multi.Bytes()))
	if err != nil || len(gotMulti.Lists) != 513 || len(gotMulti.Tracks) != 513 {
		t.Fatalf("LoadReaderContext multi-gob 513 mismatch: err=%v lists=%d tracks=%d", err, len(gotMulti.Lists), len(gotMulti.Tracks))
	}

	// 2. 200,000-track single-body Format-2 snapshot cancels promptly mid-decode.
	sBig := New("11111111-2222-3333-4444-555555555555")
	for i := 1; i <= 200000; i++ {
		rel := fmt.Sprintf("Music/Artist-%03d/Track-%06d.mp3", i%250, i)
		k := FoldPath(rel)
		sBig.Tracks[k] = &Track{Key: k, EngineID: int64(i), Rel: rel}
	}
	rawBig, err := sBig.Encode()
	if err != nil {
		t.Fatal(err)
	}
	baseStart := time.Now()
	gotBig, err := LoadReaderContext(context.Background(), bytes.NewReader(rawBig))
	baseline := time.Since(baseStart)
	if err != nil || len(gotBig.Tracks) != 200000 {
		t.Fatalf("200k round-trip failed: err=%v tracks=%d", err, len(gotBig.Tracks))
	}

	delay := baseline / 3
	if delay < 5*time.Millisecond {
		delay = 5 * time.Millisecond
	}
	if delay > 30*time.Millisecond {
		delay = 30 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		mu       sync.Mutex
		cancelAt time.Time
	)
	timer := time.AfterFunc(delay, func() {
		mu.Lock()
		cancelAt = time.Now()
		mu.Unlock()
		cancel()
	})
	defer timer.Stop()

	_, err = LoadReaderContext(ctx, bytes.NewReader(rawBig))
	doneAt := time.Now()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-decode cancel err=%v, want context.Canceled (baseline=%v delay=%v)", err, baseline, delay)
	}
	mu.Lock()
	cAt := cancelAt
	mu.Unlock()
	if cAt.IsZero() {
		t.Fatal("cancel timer never fired before 200k decode completed")
	}
	if sinceCancel := doneAt.Sub(cAt); sinceCancel >= 50*time.Millisecond {
		t.Fatalf("200k decode returned %v after cancel (>= 50ms)", sinceCancel)
	}
}
