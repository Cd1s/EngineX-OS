package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// Tests of the background job (fill.go, pace.go). The old job's tests checked the same
// behaviour through the stage m.db on the stick; each test names the ones it replaces.

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "rb-fill-stage-")
	if err != nil {
		panic(err)
	}
	os.Setenv(rbstate.StageEnv, dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var uuidSeq struct {
	sync.Mutex
	n int
}

// fillStick is a LargeExport of n tracks with a snapshot as a window leaves it after the rows:
// every track has a row (Engine id = its place + 1) and no performance data yet.
func fillStick(t *testing.T, n int) (root string, snap *rbstate.Snapshot) {
	t.Helper()
	root = t.TempDir()
	testfixture.LargeExport(t, root, n)
	writeSnapshot(t, root, func(s *rbstate.Snapshot) {})
	snap = loadSnapshot(t, root)
	return root, snap
}

func dbDirOf(root string) string { return filepath.Join(root, "Engine Library", "Database2") }

// writeSnapshot makes a snapshot of the stick's view and lets edit change it before saving.
func writeSnapshot(t *testing.T, root string, edit func(*rbstate.Snapshot)) {
	t.Helper()
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	v, err := readFillView(context.Background(), r, root, rbstate.LayoutFolders)
	if err != nil {
		t.Fatal(err)
	}
	uuidSeq.Lock()
	uuidSeq.n++
	uuid := fmt.Sprintf("fill-test-%d-%d", os.Getpid(), uuidSeq.n)
	uuidSeq.Unlock()
	if old, err := rbstate.Load(dbDirOf(root)); err == nil {
		uuid = old.LibraryUUID
	}
	s := rbstate.New(uuid)
	s.Layout, s.Stamp, s.Tracks = rbstate.LayoutFolders, v.stamp, map[string]*rbstate.Track{}
	for i, vt := range v.tracks {
		rel, _ := compat.CleanRelative(vt.track.Path)
		s.Tracks[vt.key] = &rbstate.Track{Key: vt.key, EngineID: int64(i + 1), Rel: rel, Row: vt.hash}
	}
	edit(s)
	if err = os.MkdirAll(dbDirOf(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = s.Save(dbDirOf(root)); err != nil {
		t.Fatal(err)
	}
}

func loadSnapshot(t *testing.T, root string) *rbstate.Snapshot {
	t.Helper()
	s, err := rbstate.Load(dbDirOf(root))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func stageOf(t *testing.T, s *rbstate.Snapshot) *rbstate.Stage {
	t.Helper()
	st, err := rbstate.OpenStage(s.LibraryUUID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Remove() })
	return st
}

func mustFill(t *testing.T, root string, o FillOptions) FillResult {
	t.Helper()
	res, err := FillInBackground(context.Background(), root, o)
	if err != nil {
		t.Fatalf("fill: %+v %v", res, err)
	}
	return res
}

// applyStage does what a window does with the stage: every record of the view's hash becomes
// the snapshot's Built, and the records are dropped.
func applyStage(t *testing.T, root string, st *rbstate.Stage) {
	t.Helper()
	entries, _, err := st.Entries()
	if err != nil {
		t.Fatal(err)
	}
	s := loadSnapshot(t, root)
	var keys []string
	for k, e := range entries {
		tr := s.Tracks[k]
		if tr == nil || e.Sig.Row != tr.Row {
			continue
		}
		tr.Built, tr.Perf = e.Sig, rbstate.PerfApplied
		if e.Degraded != "" {
			tr.Perf = rbstate.PerfDegraded
		}
		keys = append(keys, k)
	}
	if err = s.Save(dbDirOf(root)); err != nil {
		t.Fatal(err)
	}
	if err = st.Drop(keys...); err != nil {
		t.Fatal(err)
	}
}

// treeSums is the SHA-256 of every file under root.
func treeSums(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = sha256.Sum256(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func indexOrder(t *testing.T, st *rbstate.Stage) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(st.Dir(), "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var l struct {
			K string `json:"k"`
		}
		if json.Unmarshal([]byte(line), &l) == nil && l.K != "" {
			keys = append(keys, l.K)
		}
	}
	return keys
}

// The job stages every track of the export and writes nothing on the stick but the mini
// waveforms of finished tracks: m.db, the snapshot and the exports stay byte for byte. Replaces
// TestImportInBackgroundConsumesAndAcksPriority (the job completes), TestPublishGeneratesAndPreservesRGBOverviews
// (every finished track gets its mini waveform) and pins the contract's "writes nothing else".
func TestFillStagesEveryTrackAndWritesNothingElseOnTheStick(t *testing.T) {
	t.Parallel()
	root, snap := fillStick(t, 12)
	testfixture.Write(t, root, "Engine Library/Database2/m.db", []byte("the live library, never opened"))
	before := treeSums(t, root)
	st := stageOf(t, snap)
	res := mustFill(t, root, FillOptions{Stage: st})
	if res.Staged != 12 || res.Degraded+res.Missing+res.Unread+res.Left != 0 || !res.Publish {
		t.Fatalf("fill: %+v", res)
	}
	after := treeSums(t, root)
	for p, sum := range before {
		if after[p] != sum {
			t.Errorf("the job changed %s", p)
		}
	}
	rgbDir := "Engine Library/Database2/OverviewData/" + snap.LibraryUUID + "/"
	for p := range after {
		if _, old := before[p]; old {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(p, rgbDir), ".rgb"))
		if !strings.HasPrefix(p, rgbDir) || err != nil || id < 1 || id > 12 {
			t.Errorf("the job wrote %s", p)
		}
	}
	if n := len(after) - len(before); n != 12 {
		t.Fatalf("%d mini waveforms, want 12", n)
	}
	idx, err := st.Index()
	if err != nil || len(idx) != 12 {
		t.Fatalf("stage index %d %v", len(idx), err)
	}
	for k, sig := range idx {
		if tr := snap.Tracks[k]; tr == nil || sig.Row != tr.Row || sig.Rules != rbstate.Rules || sig.Audio.IsZero() || len(sig.ANLZ) != 3 {
			t.Fatalf("staged %s: %+v", k, sig)
		}
		rec, err := st.Get(k)
		if err != nil || !rec.Perf.Complete || rec.Perf.Beats == nil || len(rec.RGB) == 0 || len(rec.Detail) != 2 {
			t.Fatalf("record %s: %v", k, err)
		}
	}
	if st.Verified() != snap.Stamp {
		t.Fatalf("verified %q, want the snapshot's stamp", st.Verified())
	}
}

// A track the stage or the snapshot has for its files is not built again; once the stage has
// verified the stamp, a track the snapshot has built is not even stat'ed. #53: a degraded track
// is not read again while its files are unchanged. Replaces TestRepeatImportDoesNotReadUnneededAnalysis253,
// TestLegacyIdentityDoesNotReadAnalysisPayload and the second half of TestIssue53OneBadTrackKeepsTheRest.
func TestFillSkipsWhatIsStagedOrBuilt(t *testing.T) {
	root, snap := fillStick(t, 6)
	testfixture.Write(t, root, testfixture.LargeTrackPath(3), []byte("not an mp3 at all"))
	writeSnapshot(t, root, func(*rbstate.Snapshot) {})
	st := stageOf(t, snap)
	first := mustFill(t, root, FillOptions{Stage: st})
	if first.Staged != 5 || first.Degraded != 1 || !first.Publish {
		t.Fatalf("first job: %+v", first)
	}
	again := mustFill(t, root, FillOptions{Stage: st})
	if again.Staged+again.Degraded != 0 || again.Publish {
		t.Fatalf("second job, nothing applied meanwhile: %+v (the same records do not ask for another remount)", again)
	}
	applyStage(t, root, st)
	// With the stamp verified the built tracks are not looked at: not even a missing file shows.
	hidden := filepath.Join(t.TempDir(), "Contents")
	if err := os.Rename(filepath.Join(root, "Contents"), hidden); err != nil {
		t.Fatal(err)
	}
	third := mustFill(t, root, FillOptions{Stage: st})
	if third.Staged+third.Degraded+third.Missing+third.Unread != 0 || third.Publish {
		t.Fatalf("job after the window applied everything: %+v", third)
	}
	if err := os.Rename(hidden, filepath.Join(root, "Contents")); err != nil {
		t.Fatal(err)
	}
	// A re-export (new stamp) is compared track by track: nothing changed, nothing is built,
	// the degraded track included.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, "PIONEER/rekordbox/export.pdb"), later, later); err != nil {
		t.Fatal(err)
	}
	writeSnapshot(t, root, func(s *rbstate.Snapshot) {
		for k, tr := range loadSnapshot(t, root).Tracks {
			s.Tracks[k].Built, s.Tracks[k].Perf = tr.Built, tr.Perf
		}
	})
	fourth := mustFill(t, root, FillOptions{Stage: st})
	if fourth.Staged+fourth.Degraded+fourth.Missing+fourth.Unread != 0 || fourth.Publish {
		t.Fatalf("re-export with unchanged files: %+v", fourth)
	}
	if st.Verified() != loadSnapshot(t, root).Stamp {
		t.Fatal("the re-export's stamp was not verified")
	}
	// One changed audio file: that track alone.
	if err := os.Chtimes(filepath.Join(root, testfixture.LargeTrackPath(5)), later, later); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "PIONEER/rekordbox/export.pdb"), later.Add(time.Hour), later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	writeSnapshot(t, root, func(s *rbstate.Snapshot) {
		for k, tr := range loadSnapshot(t, root).Tracks {
			s.Tracks[k].Built, s.Tracks[k].Perf = tr.Built, tr.Perf
		}
	})
	fifth := mustFill(t, root, FillOptions{Stage: st})
	if fifth.Staged != 1 || !fifth.Publish || indexOrder(t, st)[len(indexOrder(t, st))-1] != rbstate.FoldPath(testfixture.LargeTrackPath(5)) {
		t.Fatalf("one changed file: %+v", fifth)
	}
}

// setDeck writes stream-sync's deck-activity file as it is just now.
func setDeck(t *testing.T, active bool) {
	t.Helper()
	if err := writePrivateJSON(deckActivityPath(), DeckActivity{Active: active, UpdatedAtMS: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if IsDeckActive() != active {
		t.Fatalf("deck activity %v", active)
	}
}

// Priority first, newest request first, then every other track sorted by path. A path the view
// does not have is tried a bounded number of times and does not hold the job up; the acks
// survive a restarted queue. Replaces TestOrderTracksForPreparePriorityThenPathSorted,
// TestPriorityImportsTargetTrackFirstInBackground and TestImportInBackgroundConsumesAndAcksPriority.
func TestFillServesThePriorityQueueFirst(t *testing.T) {
	defer SetPriorityDirForTest(t.TempDir())()
	root, snap := fillStick(t, 12)
	q := NewPriorityQueue()
	if _, err := q.Enqueue(root, "../"+testfixture.LargeTrackPath(12), "deck", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(root, "../Contents/does-not-exist.mp3", "focus", 0); err != nil {
		t.Fatal(err)
	}
	seq, err := q.Enqueue(root, "../"+strings.ToUpper(testfixture.LargeTrackPath(7)), "preview", 0)
	if err != nil {
		t.Fatal(err)
	}
	st := stageOf(t, snap)
	res := mustFill(t, root, FillOptions{Stage: st})
	if res.Staged != 12 || res.Left != 0 {
		t.Fatalf("fill: %+v", res)
	}
	order := indexOrder(t, st)
	var rest []string
	for i := 1; i <= 12; i++ {
		if i != 7 && i != 12 {
			rest = append(rest, rbstate.FoldPath(testfixture.LargeTrackPath(i)))
		}
	}
	slices.Sort(rest)
	want := append([]string{rbstate.FoldPath(testfixture.LargeTrackPath(7)), rbstate.FoldPath(testfixture.LargeTrackPath(12))}, rest...)
	if !slices.Equal(order, want) {
		t.Fatalf("stage order\n%q\nwant\n%q", order, want)
	}
	if ack := ReadPriorityAck(root); ack.AckSeq < seq {
		t.Fatalf("ack %+v, want seq %d", ack, seq)
	}
	if p, _ := PendingPriority(root); len(p) != 1 || !strings.Contains(p[0].Path, "does-not-exist") {
		t.Fatalf("pending after the job: %+v (only the unknown path stays, for a later job)", p)
	}
	restarted := NewPriorityQueue()
	seq2, err := restarted.Enqueue(root, "../"+testfixture.LargeTrackPath(7), "deck", 2)
	if err != nil || seq2 <= ReadPriorityAck(root).AckSeq {
		t.Fatalf("restarted queue seq %d %v, want past the ack", seq2, err)
	}
}

// A prioritized track's mini waveform reaches the live OverviewData at once, never over a valid
// file; an empty file is replaced. Replaces TestImportInBackgroundConsumesAndAcksPriority (the
// live .rgb before the remount) and TestRenameStagePreservesValidDestinationRGB.
func TestFillWritesTheMiniWaveformNeverOverAValidOne(t *testing.T) {
	t.Parallel()
	root, snap := fillStick(t, 3)
	dir := filepath.Join(dbDirOf(root), "OverviewData", snap.LibraryUUID)
	id := func(i int) int64 { return snap.Tracks[rbstate.FoldPath(testfixture.LargeTrackPath(i))].EngineID }
	path := func(i int) string { return filepath.Join(dir, strconv.FormatInt(id(i), 10)+".rgb") }
	raw := make([]byte, 24+3*1024+3)
	binary.BigEndian.PutUint64(raw, 1024)
	binary.BigEndian.PutUint64(raw[8:], 1024)
	raw[100] = 9
	engines, err := enginedb.QCompress(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path(1), engines, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path(2), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mustFill(t, root, FillOptions{Stage: stageOf(t, snap)})
	if b, _ := os.ReadFile(path(1)); !bytes.Equal(b, engines) {
		t.Fatal("a valid mini waveform was replaced")
	}
	for _, i := range []int{2, 3} {
		b, err := os.ReadFile(path(i))
		if err != nil {
			t.Fatal(err)
		}
		if raw, err := enginedb.QUncompress(b, 1<<20); err != nil || !enginedb.ValidRGBOverview(raw) {
			t.Fatalf("track %d: mini waveform %d bytes: %v", i, len(b), err)
		}
	}
}

// bigPWV7 gives the fixture's shared .2EX a 600 KB PWV7.
func bigPWV7(t *testing.T, root string) {
	wave := make([]byte, 12+3*200000)
	binary.BigEndian.PutUint32(wave, 3)
	binary.BigEndian.PutUint32(wave[4:], 200000)
	testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.2EX", testfixture.PMAI(testfixture.Section("PWV7", 24, wave)))
}

// With no deck playing the job reads at full speed; while one plays its reads are 64 KiB with a
// pause of at least playingReadGap after each, and a deck that starts playing slows the next
// track. Replaces TestBackgroundPassOptionsIdleVsPlaying and
// TestConfirmedRefsSkipsPacingWhenIdleAndPacesWhenPlaying.
func TestFillYieldsToAPlayingDeck(t *testing.T) {
	defer SetPriorityDirForTest(t.TempDir())()
	old, oldCache := playingReadGap, fromCache
	playingReadGap, fromCache = 10*time.Millisecond, 0 // the fixture's reads come from memory: count them all
	defer func() { playingReadGap, fromCache = old, oldCache }()
	run := func(active bool) time.Duration {
		root, snap := fillStick(t, 1)
		bigPWV7(t, root)
		writeSnapshot(t, root, func(*rbstate.Snapshot) {})
		setDeck(t, active)
		start := time.Now()
		res := mustFill(t, root, FillOptions{Stage: stageOf(t, snap)})
		if res.Staged != 1 {
			t.Fatalf("fill: %+v", res)
		}
		return time.Since(start)
	}
	idle := run(false)
	playing := run(true)
	// 600 KB in 64 KiB reads is 10 paced reads of the .2EX alone.
	if playing < 10*playingReadGap {
		t.Fatalf("with a deck playing the job took %s, want at least %s of pauses", playing, 10*playingReadGap)
	}
	if idle >= playing/2 {
		t.Fatalf("idle %s, playing %s: the idle job paused", idle, playing)
	}
}

// A stop ends the job at once and keeps what it staged; the next job goes on from there and
// only then verifies the stamp. The progress count never goes down and reaches the total.
// Replaces TestIssue253ChecksArePacedAndStopCleanly (the stop), TestSlowStickMakesProgressOnEveryImport
// and TestIssue328BackgroundJobCountsThroughout.
func TestFillStopsCleanlyAndGoesOnLater(t *testing.T) {
	defer SetPriorityDirForTest(t.TempDir())()
	old, oldCache := playingReadGap, fromCache
	playingReadGap, fromCache = 5*time.Millisecond, 0
	defer func() { playingReadGap, fromCache = old, oldCache }()
	root, snap := fillStick(t, 20)
	bigPWV7(t, root)
	writeSnapshot(t, root, func(*rbstate.Snapshot) {})
	setDeck(t, true)
	st := stageOf(t, snap)
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var calls [][2]int
	progress := func(done, total int) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, [2]int{done, total})
		if done == 3 {
			cancel()
		}
	}
	res, err := FillInBackground(ctx, root, FillOptions{Stage: st, Progress: progress})
	if err != nil || res.Left == 0 || res.Staged < 3 || res.Staged+res.Left != 20 {
		t.Fatalf("stopped job: %+v %v", res, err)
	}
	if st.Verified() != "" {
		t.Fatal("a stopped job verified the stamp")
	}
	setDeck(t, false)
	next := mustFill(t, root, FillOptions{Stage: st, Progress: progress})
	if next.Staged != res.Left || next.Left != 0 || st.Verified() != snap.Stamp {
		t.Fatalf("next job: %+v after %+v", next, res)
	}
	last := -1
	for i, c := range calls {
		if c[1] != 20 {
			t.Fatalf("progress %v: total", c)
		}
		if i > 0 && c[0] < last && c[0] != 0 {
			t.Fatalf("progress went down: %v", calls)
		}
		last = c[0]
	}
	if calls[len(calls)-1] != [2]int{20, 20} {
		t.Fatalf("progress ended at %v", calls[len(calls)-1])
	}
}

// The job waits while a window holds the volume, and starts when it lets go.
func TestFillWaitsForTheWindow(t *testing.T) {
	t.Parallel()
	root, snap := fillStick(t, 2)
	unlock, err := rbstate.LockVolume(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan FillResult, 1)
	go func() {
		res, _ := FillInBackground(context.Background(), root, FillOptions{Stage: stageOf(t, snap)})
		done <- res
	}()
	select {
	case res := <-done:
		unlock()
		t.Fatalf("the job ran beside a window: %+v", res)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case res := <-done:
		if res.Staged != 2 {
			t.Fatalf("after the window: %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the job did not start after the window")
	}
}

// Publish counts only what a window would apply: records of the view's current row hash for
// tracks the bridge writes. An Engine-made row (#252) is never staged.
func TestFillPublishesOnlyWhatAWindowWouldApply(t *testing.T) {
	t.Parallel()
	root, snap := fillStick(t, 3)
	native := rbstate.FoldPath(testfixture.LargeTrackPath(2))
	writeSnapshot(t, root, func(s *rbstate.Snapshot) { s.Tracks[native].Native = true })
	st := stageOf(t, snap)
	stale := &rbstate.Staged{Key: rbstate.FoldPath(testfixture.LargeTrackPath(1)), Sig: rbstate.TrackSig{Rules: rbstate.Rules, Row: "an older export"}}
	if err := st.Put(stale); err != nil {
		t.Fatal(err)
	}
	res := mustFill(t, root, FillOptions{Stage: st})
	if res.Staged != 2 || !res.Publish {
		t.Fatalf("fill: %+v", res)
	}
	if _, err := st.Get(native); err == nil {
		t.Fatal("Engine's own row was staged")
	}
	applyStage(t, root, st)
	if err := st.Put(stale); err != nil {
		t.Fatal(err)
	}
	if res = mustFill(t, root, FillOptions{Stage: st}); res.Publish || res.Staged != 0 {
		t.Fatalf("after the window, a record of an older export in the stage: %+v", res)
	}
}

// A track the volume refuses (here a symlink) is logged and left unstaged; it does not stop the
// job, which stages the others and still verifies the stamp: the track was looked at, and the next
// job looks at it again. Replaces TestPrepare252UnreadableFirstTrackCannotStarveResumedMounts (one
// bad track cannot hold the others up; its per-mount stat budget is gone with the deadline).
func TestFillGoesOnPastATrackItCannotRead(t *testing.T) {
	t.Parallel()
	root, snap := fillStick(t, 4)
	bad := filepath.Join(root, testfixture.LargeTrackPath(1))
	if err := os.Rename(bad, bad+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bad+".real", bad); err != nil {
		t.Fatal(err)
	}
	st := stageOf(t, snap)
	res := mustFill(t, root, FillOptions{Stage: st})
	if res.Unread != 1 || res.Staged != 3 || res.Left != 0 || st.Verified() != snap.Stamp {
		t.Fatalf("fill: %+v, verified %q", res, st.Verified())
	}
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bad+".real", bad); err != nil {
		t.Fatal(err)
	}
	if res = mustFill(t, root, FillOptions{Stage: st}); res.Staged != 1 || res.Unread != 0 {
		t.Fatalf("after the file is back: %+v", res)
	}
}

// While a deck plays, a read that went to the stick is followed by a pause as long as it took and
// at least playingReadGap; one the page cache served is not; a stop ends the pause at once.
func TestPacerPausesOnlyAfterTheStick(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	p := &pacer{ctx: ctx, active: func() bool { return true }}
	opts, playing := p.readOptions()
	if !playing || opts.Chunk != 64<<10 || opts.Pause == nil {
		t.Fatalf("playing: %+v", opts)
	}
	measure := func(took time.Duration) time.Duration {
		start := time.Now()
		if err := opts.Pause(1, took); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	if d := measure(100 * time.Microsecond); d > playingReadGap/2 {
		t.Fatalf("a read from the cache paused %s", d)
	}
	if d := measure(fromCache); d < playingReadGap {
		t.Fatalf("a read of the stick paused %s, want at least %s", d, playingReadGap)
	}
	if d := measure(3 * playingReadGap); d < 3*playingReadGap {
		t.Fatalf("a slow read paused %s, want as long as it took", d)
	}
	if p.pauses != 2 {
		t.Fatalf("%d pauses counted", p.pauses)
	}
	cancel()
	start := time.Now()
	if err := opts.Pause(1, time.Hour); err == nil || time.Since(start) > time.Second {
		t.Fatalf("a stopped job paused: %v after %s", err, time.Since(start))
	}
	if _, playing = (&pacer{ctx: context.Background(), active: func() bool { return false }}).readOptions(); playing {
		t.Fatal("no deck playing, yet paced")
	}
}

// With /data at the stage's floor the job stops, says what it staged so a window applies and
// drops it, and does not fail: the next job goes on.
func TestFillStopsAtAFullStageAndPublishesWhatItHas(t *testing.T) {
	root, snap := fillStick(t, 6)
	st := stageOf(t, snap)
	old := putStaged
	defer func() { putStaged = old }()
	puts := 0
	putStaged = func(s *rbstate.Stage, rec *rbstate.Staged) error {
		if puts++; puts > 2 {
			return rbstate.ErrStageFull
		}
		return old(s, rec)
	}
	res := mustFill(t, root, FillOptions{Stage: st})
	if res.Staged != 2 || res.Left != 4 || !res.Publish || st.Verified() != "" {
		t.Fatalf("full stage: %+v, verified %q", res, st.Verified())
	}
	putStaged = old
	applyStage(t, root, st)
	if res = mustFill(t, root, FillOptions{Stage: st}); res.Staged != 4 || res.Left != 0 || st.Verified() != snap.Stamp {
		t.Fatalf("after the window: %+v", res)
	}
}
