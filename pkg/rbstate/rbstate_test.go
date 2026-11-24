package rbstate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sampleSnapshot() *Snapshot {
	s := New("11111111-2222-3333-4444-555555555555")
	s.Layout = LayoutFolders
	s.Sources = []FileSig{{Rel: "PIONEER/rekordbox/export.pdb", Size: 10, MTimeNS: 20}, {Rel: "PIONEER/rekordbox/exportLibrary.db", Size: 30, MTimeNS: 40}}
	s.Stamp = Stamp(s.Layout, s.Sources, "")
	s.Pending = Pending{Perf: 1, Verify: true}
	s.DB = DBIdentity{Size: 4096, Counter: 7}
	s.Tracks["contents/a/one.mp3"] = &Track{
		Key: "contents/a/one.mp3", EngineID: 12, Rel: "Contents/A/One.mp3", Resolved: true,
		Refs: []SourceRef{{Kind: DeviceLibrary, ID: 5, RecordHash: "aa"}, {Kind: OneLibrary, ID: 9, RecordHash: "bb"}},
		Row:  "r1", Perf: PerfApplied, Written: []byte{1, 2, 3}, Art: "cafe", Detail: []string{"PIONEER/USBANLZ/P001/0001/ANLZ0000.2EX"},
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
	if h.Stamp != want.Stamp || h.Tracks != 2 || h.Lists != 1 || h.Pending != want.Pending || h.DB != want.DB || h.LibraryUUID != want.LibraryUUID {
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
	if !ok || tr.EngineID != 12 || !tr.Built.Equal(want.Tracks["contents/a/one.mp3"].Built) {
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
	changed := bytes.Replace(whole, []byte(`"id":12`), []byte(`"id":13`), 1)
	if err := os.WriteFile(p, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a changed byte passed the checksum: %v", err)
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
		Perf: Perf{TrackData: []byte{1}, Beats: []byte{2}, ActiveLoops: 5, Complete: true}, RGB: []byte("rgb"), Detail: []string{"x.2EX"}}
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
	idx, err := s.Index()
	if err != nil || len(idx) != 3 || idx[a.Key].Row != "r2" {
		t.Fatalf("index %v %v", idx, err)
	}
	if s.Verified() != "stamp-1" {
		t.Fatalf("verified %q", s.Verified())
	}
	got, err := s.Get(a.Key)
	if err != nil || !bytes.Equal(got.Perf.Beats, []byte{2}) || got.Perf.ActiveLoops != 5 || !got.Perf.Complete || string(got.RGB) != "rgb" || got.Facts.SampleRate != 44100 || got.Detail[0] != "x.2EX" {
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
	// A staged track whose performance file is gone is staged again: it is not in the index.
	if err = os.Remove(filepath.Join(s.Dir(), stageName(a.Key)+".bin")); err != nil {
		t.Fatal(err)
	}
	if idx, _ = s.Index(); len(idx) != 2 {
		t.Fatalf("index with a lost file: %v", idx)
	}
	if _, err = s.Get(a.Key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("get of a lost file: %v", err)
	}
	if err = s.Drop(b.Key, c.Key); err != nil {
		t.Fatal(err)
	}
	if idx, _ = s.Index(); len(idx) != 0 {
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

func TestStageNeedsItsRootInTestsAndASafeName(t *testing.T) {
	t.Setenv(StageEnv, "")
	if _, err := OpenStage("lib"); err == nil {
		t.Fatal("a test opened the deck's stage root")
	}
	t.Setenv(StageEnv, t.TempDir())
	for _, bad := range []string{"", "..", "a/b", ".hidden"} {
		if _, err := OpenStage(bad); err == nil {
			t.Errorf("OpenStage(%q) worked", bad)
		}
	}
}
