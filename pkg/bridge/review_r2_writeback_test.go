package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func swapCompactDATCueOrder(t *testing.T, raw []byte) []byte {
	t.Helper()
	broken := append([]byte(nil), raw...)
	var cueSections []int
	for pos := int(binary.BigEndian.Uint32(raw[4:])); pos+12 <= len(raw); {
		if string(raw[pos:pos+4]) == "PCOB" {
			cueSections = append(cueSections, pos)
		}
		pos += int(binary.BigEndian.Uint32(raw[pos+8:]))
	}
	if len(cueSections) != 2 {
		t.Fatalf("fixture has %d PCOB sections", len(cueSections))
	}
	binary.BigEndian.PutUint32(broken[cueSections[0]+12:], 0)
	binary.BigEndian.PutUint32(broken[cueSections[1]+12:], 1)
	return broken
}

func writeReviewFixtureFile(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	dst := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func rewritePPTHSection(t *testing.T, raw []byte, newPath string) []byte {
	t.Helper()
	h := int(binary.BigEndian.Uint32(raw[4:]))
	n := int(binary.BigEndian.Uint32(raw[8:]))
	var secs [][]byte
	for off := h; off < n; {
		sn := int(binary.BigEndian.Uint32(raw[off+8:]))
		sec := append([]byte(nil), raw[off:off+sn]...)
		if string(sec[:4]) == "PPTH" {
			runes := []rune(newPath + "\x00")
			plen := 2 * len(runes)
			p := make([]byte, 16+plen)
			copy(p, "PPTH")
			binary.BigEndian.PutUint32(p[4:], 16)
			binary.BigEndian.PutUint32(p[8:], uint32(len(p)))
			binary.BigEndian.PutUint32(p[12:], uint32(plen))
			for i, r := range runes {
				binary.BigEndian.PutUint16(p[16+2*i:], uint16(r))
			}
			sec = p
		}
		secs = append(secs, sec)
		off += sn
	}
	total := h
	for _, s := range secs {
		total += len(s)
	}
	out := make([]byte, total)
	copy(out, raw[:h])
	binary.BigEndian.PutUint32(out[8:], uint32(total))
	pos := h
	for _, s := range secs {
		copy(out[pos:], s)
		pos += len(s)
	}
	return out
}

func TestReviewRepairNeverUsesSupersededAnalysisPath(t *testing.T) {
	mount := t.TempDir()
	old := "PIONEER/USBANLZ/P001/ANLZ0000.DAT"
	current := "PIONEER/USBANLZ/P002/ANLZ0000.DAT"
	raw, err := os.ReadFile("../rekordbox/testdata/rb_hot_compact.dat")
	if err != nil {
		t.Fatal(err)
	}
	broken := swapCompactDATCueOrder(t, raw)
	for rel, data := range map[string][]byte{
		old: broken, current: raw, ".enginex-backup/2000-01-01/" + old: raw,
	} {
		writeReviewFixtureFile(t, mount, rel, data)
	}
	r, err := compat.NewResolver(mount)
	if err != nil {
		t.Fatal(err)
	}
	snap := rbstate.New("review")
	snap.Tracks["contents/fixturea/track01.mp3"] = &rbstate.Track{
		Key: "contents/fixturea/track01.mp3", Rel: "Contents/FixtureA/Track01.mp3", EngineID: 1, AnalysisPath: current,
		Built: rbstate.TrackSig{ANLZ: []rbstate.FileSig{{Rel: old}}},
	}
	if got := applyRepairChanges(r, mount, snap, nil); len(got) != 0 {
		t.Fatalf("repair selected obsolete analysis %s although the track now owns %s", got[0].analysis, current)
	}
}

func TestReviewRepairRefusesPPTHMismatchAndSeparateGridTarget(t *testing.T) {
	raw, err := os.ReadFile("../rekordbox/testdata/rb_hot_compact.dat")
	if err != nil {
		t.Fatal(err)
	}
	extRaw, err := os.ReadFile("../rekordbox/testdata/rb_hot_compact.ext")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("PPTHMismatchRefused", func(t *testing.T) {
		mount := t.TempDir()
		rel := "PIONEER/USBANLZ/P001/ANLZ0000.DAT"
		foreign := rewritePPTHSection(t, swapCompactDATCueOrder(t, raw), "/Contents/Foreign/OtherTrack.mp3")
		writeReviewFixtureFile(t, mount, rel, foreign)
		writeReviewFixtureFile(t, mount, ".enginex-backup/2000-01-01/"+rel, raw)
		r, err := compat.NewResolver(mount)
		if err != nil {
			t.Fatal(err)
		}
		snap := rbstate.New("review")
		snap.Tracks["contents/fixturea/track01.mp3"] = &rbstate.Track{
			Key: "contents/fixturea/track01.mp3", Rel: "Contents/FixtureA/Track01.mp3", EngineID: 1, AnalysisPath: "/" + rel,
		}
		if got := applyRepairChanges(r, mount, snap, nil); len(got) != 0 {
			t.Fatalf("repair did not refuse ANLZ with mismatched PPTH: %+v", got)
		}
	})

	t.Run("OnlyBrokenVariantSelectedAndSeparateFromOtherGridTarget", func(t *testing.T) {
		mount := t.TempDir()
		datRel := "PIONEER/USBANLZ/P001/ANLZ0000.DAT"
		extRel := "PIONEER/USBANLZ/P001/ANLZ0000.EXT"
		// Make EXT swapped while keeping DAT byte-identical to backup.
		brokenEXT := append([]byte(nil), extRaw...)
		var pco2At []int
		for pos := int(binary.BigEndian.Uint32(extRaw[4:])); pos+12 <= len(extRaw); {
			if string(extRaw[pos:pos+4]) == "PCO2" {
				pco2At = append(pco2At, pos)
			}
			pos += int(binary.BigEndian.Uint32(extRaw[pos+8:]))
		}
		if len(pco2At) != 2 {
			t.Fatalf("expected 2 PCO2 sections, got %d", len(pco2At))
		}
		binary.BigEndian.PutUint32(brokenEXT[pco2At[0]+12:], 0)
		binary.BigEndian.PutUint32(brokenEXT[pco2At[1]+12:], 1)

		writeReviewFixtureFile(t, mount, datRel, raw)
		writeReviewFixtureFile(t, mount, extRel, brokenEXT)
		writeReviewFixtureFile(t, mount, ".enginex-backup/2000-01-01/"+datRel, raw)
		writeReviewFixtureFile(t, mount, ".enginex-backup/2000-01-01/"+extRel, extRaw)

		r, err := compat.NewResolver(mount)
		if err != nil {
			t.Fatal(err)
		}
		tr := &rbstate.Track{
			Key: "contents/fixturea/track01.mp3", Rel: "Contents/FixtureA/Track01.mp3", EngineID: 1, AnalysisPath: "/" + datRel,
		}
		snap := rbstate.New("review")
		snap.Tracks[tr.Key] = tr

		// Seed an existing live grid edit on a DIFFERENT analysis path P002.
		liveGrid := []trackChange{{
			candidate: dbCandidate{key: tr.Key, engineID: 1, track: tr},
			analysis:  "/PIONEER/USBANLZ/P002/ANLZ0000.DAT",
			gridDiff:  true,
			beats:     []rekordbox.Beat{{Number: 1, BPM: 128, TimeMS: 100}},
		}}
		got := applyRepairChanges(r, mount, snap, liveGrid)
		if len(got) != 2 {
			t.Fatalf("expected separate cue-repair change alongside live P002 grid change, got %d (%+v)", len(got), got)
		}
		if got[0].analysis != "/PIONEER/USBANLZ/P002/ANLZ0000.DAT" || got[0].anlzCueRepair {
			t.Fatalf("live grid change on P002 was polluted by P001 cue repair: %+v", got[0])
		}
		rep := got[1]
		if rep.analysis != "/"+datRel || !rep.anlzCueRepair || rep.repairVariant[0] || !rep.repairVariant[1] {
			t.Fatalf("unexpected per-variant repair plan (want DAT=false EXT=true): %+v", rep)
		}
		if !bytes.Equal(rep.restoreVariant[1], extRaw) {
			t.Fatal("expected exact backup restoration bytes for clean-cues EXT variant")
		}
	})
}

func makeSwappedPairFromCues(t *testing.T, baseDAT, baseEXT []byte, cues []rekordbox.Cue) ([]byte, []byte) {
	t.Helper()
	buildOne := func(raw []byte, isExt bool) []byte {
		rewritten, _, err := rekordbox.RewriteANLZCues(raw, cues, isExt, false)
		if err != nil {
			t.Fatal(err)
		}
		h := int(binary.BigEndian.Uint32(rewritten[4:]))
		secs := dumpANLZSections(t, rewritten)
		var pcob0, pcob1, pco20, pco21 []byte
		var body [][]byte
		for _, s := range secs {
			switch {
			case s.tag == "PCOB" && s.kind == 0:
				pcob0 = s.raw
			case s.tag == "PCOB" && s.kind == 1:
				pcob1 = s.raw
			case s.tag == "PCO2" && s.kind == 0:
				pco20 = s.raw
			case s.tag == "PCO2" && s.kind == 1:
				pco21 = s.raw
			default:
				body = append(body, s.raw)
			}
		}
		body = append(body, pcob0, pcob1)
		if isExt {
			body = append(body, pco20, pco21)
		}
		total := h
		for _, b := range body {
			total += len(b)
		}
		out := make([]byte, total)
		copy(out, rewritten[:h])
		binary.BigEndian.PutUint32(out[8:], uint32(total))
		pos := h
		for _, b := range body {
			copy(out[pos:], b)
			pos += len(b)
		}
		return out
	}
	return buildOne(baseDAT, false), buildOne(baseEXT, true)
}

func buildTestTrackDataBlob(sr float64, samples uint64) []byte {
	raw := make([]byte, 44)
	binary.BigEndian.PutUint64(raw[0:8], math.Float64bits(sr))
	binary.BigEndian.PutUint64(raw[8:16], samples)
	c, err := enginedb.QCompress(raw)
	if err != nil {
		panic(err)
	}
	return c
}

type phantomTestEnv struct {
	root     string
	dbDir    string
	dat1Rel  string
	ext1Rel  string
	dat1Path string
	ext1Path string
	onePath  string
	origDAT  []byte
	origEXT  []byte
	rbOffset float64
}

func setupPhantomTestStick(t *testing.T, origCues []rekordbox.Cue) phantomTestEnv {
	t.Helper()
	root := t.TempDir()
	dat1Rel, ext1Rel, _, _ := seedRealRekordboxOneLibraryStick(t, root)
	dat1Path := filepath.Join(root, filepath.FromSlash(dat1Rel))
	ext1Path := filepath.Join(root, filepath.FromSlash(ext1Rel))
	onePath := filepath.Join(root, "PIONEER", "rekordbox", "exportLibrary.db")

	origDAT, err := os.ReadFile(dat1Path)
	if err != nil {
		t.Fatal(err)
	}
	origEXT, err := os.ReadFile(ext1Path)
	if err != nil {
		t.Fatal(err)
	}
	cleanDAT, _, err := rekordbox.RewriteANLZCues(origDAT, origCues, false, false)
	if err != nil {
		t.Fatal(err)
	}
	cleanEXT, _, err := rekordbox.RewriteANLZCues(origEXT, origCues, true, false)
	if err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, root, dat1Rel, cleanDAT)
	testfixture.Write(t, root, ext1Rel, cleanEXT)
	testfixture.Write(t, root, filepath.ToSlash(filepath.Join(".enginex-backup", "2026-11-26", dat1Rel)), cleanDAT)
	testfixture.Write(t, root, filepath.ToSlash(filepath.Join(".enginex-backup", "2026-11-26", ext1Rel)), cleanEXT)
	oneRaw, err := os.ReadFile(onePath)
	if err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, root, ".enginex-backup/2026-11-26/PIONEER/rekordbox/exportLibrary.db", oneRaw)

	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := compat.AudioHeader(r.FullPath("Contents/FixtureA/Track01.mp3"))
	if err != nil || info.RekordboxOffset() <= 0 {
		t.Fatalf("AudioHeader: info=%+v err=%v", info, err)
	}
	return phantomTestEnv{
		root:     root,
		dbDir:    filepath.Join(root, "Engine Library", "Database2"),
		dat1Rel:  dat1Rel,
		ext1Rel:  ext1Rel,
		dat1Path: dat1Path,
		ext1Path: ext1Path,
		onePath:  onePath,
		origDAT:  cleanDAT,
		origEXT:  cleanEXT,
		rbOffset: float64(info.RekordboxOffset()),
	}
}

func refreshPhantomSnapStamp(t *testing.T, env phantomTestEnv) {
	t.Helper()
	r, err := compat.NewResolver(env.root)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := rbstate.Load(env.dbDir)
	if err != nil {
		t.Fatal(err)
	}
	sources, stamp, err := exportNow(env.root, snap.Header.Layout)
	if err != nil {
		t.Fatal(err)
	}
	snap.Header.Sources, snap.Header.Stamp = sources, stamp
	libs, _, err := ReadSources(context.Background(), r, env.root, sources, newKeyCache())
	if err != nil {
		t.Fatal(err)
	}
	for _, lib := range libs {
		if lib.Lib == nil {
			continue
		}
		for _, libTr := range lib.Lib.Tracks {
			if st := snap.Tracks[rbstate.FoldPath(libTr.Path)]; st != nil {
				for i := range st.Refs {
					if st.Refs[i].Kind == lib.Kind && st.Refs[i].ID == libTr.ID {
						st.Refs[i].RecordHash = libTr.RecordHash
					}
				}
			}
		}
	}
	if err := snap.Save(env.dbDir); err != nil {
		t.Fatal(err)
	}
}

func insertOneLibCues(t *testing.T, onePath string, contentID uint32, hasMod int, cueUpd sql.NullInt64, anlzUpd sql.NullInt64, cues []rekordbox.Cue) {
	t.Helper()
	db, err := sql.Open("sqlite", onePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM cue WHERE content_id=?`, contentID); err != nil {
		t.Fatal(err)
	}
	for i, c := range cues {
		outUsec := int64(-1)
		out150 := int64(-1)
		if c.Loop && c.EndMS > c.TimeMS {
			outUsec = int64(c.EndMS) * 1000
			out150 = (int64(c.EndMS)*150 + 500) / 1000
		}
		if _, err := db.Exec(`INSERT INTO cue(cue_id, content_id, kind, colorTableIndex, cueComment, isActiveLoop, beatLoopNumerator, beatLoopDenominator, inUsec, outUsec, in150FramePerSec, out150FramePerSec) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			100+i, contentID, c.HotCue, int(c.ColorCode), c.Name, 0, 0, 0, int64(c.TimeMS)*1000, outUsec, (int64(c.TimeMS)*150+500)/1000, out150); err != nil {
			t.Fatal(err)
		}
	}
	var updVal any
	if cueUpd.Valid {
		updVal = cueUpd.Int64
	}
	var anlzVal any
	if anlzUpd.Valid {
		anlzVal = anlzUpd.Int64
	}
	if _, err := db.Exec(`UPDATE content SET hasModified=?, cueUpdateCount=?, analysisDataUpdateCount=? WHERE content_id=?`, hasMod, updVal, anlzVal, contentID); err != nil {
		t.Fatal(err)
	}
}

func setMDBQuickCues(t *testing.T, dbDir string, trackID int64, rbOffset float64, padMS map[int]uint32) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var slots [8]struct {
		name    string
		samples float64
		color   [4]byte
	}
	for i := range slots {
		slots[i].samples = -1
		if ms, ok := padMS[i]; ok {
			slots[i].name = fmt.Sprintf("Cue %d", i+1)
			slots[i].samples = float64(ms)*44.1 - rbOffset
			if slots[i].samples < 0 {
				slots[i].samples = 0
			}
			slots[i].color = engineDefaultPadColors[i]
		}
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, quickCues=? WHERE trackId=?`,
		buildTestTrackDataBlob(44100, 650*44100), buildEngineQuickCuesBlob(slots), trackID); err != nil {
		t.Fatal(err)
	}
}

func TestWriteBackPhantomCleanupContent76DBRowsWhenANLZAlreadyClean(t *testing.T) {
	ctx := context.Background()
	env := setupPhantomTestStick(t, nil)

	// Content-76 leftover state: ANLZ pair is already clean empty layout matching backup,
	// m.db has no cues, while exportLibrary.db still carries 8x23ms phantom rows + marks.
	setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, nil)
	var eight []rekordbox.Cue
	for pad := uint32(1); pad <= 8; pad++ {
		eight = append(eight, rekordbox.Cue{HotCue: pad, TimeMS: 23, Name: fmt.Sprintf("Cue %d", pad), ColorCode: byte(pad)})
	}
	insertOneLibCues(t, env.onePath, 1, 1, sql.NullInt64{Int64: 1, Valid: true}, sql.NullInt64{}, eight)
	refreshPhantomSnapStamp(t, env)

	res, err := WriteBackCues(ctx, env.root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues: %v", err)
	}
	if res.FilesUpdated != 1 || !res.OneLibraryUpdated {
		t.Fatalf("expected only exportLibrary.db updated (1 file), got %+v", res)
	}
	if cur, _ := os.ReadFile(env.dat1Path); !bytes.Equal(cur, env.origDAT) {
		t.Fatal("clean DAT was unexpectedly modified")
	}
	if cur, _ := os.ReadFile(env.ext1Path); !bytes.Equal(cur, env.origEXT) {
		t.Fatal("clean EXT was unexpectedly modified")
	}

	oneDB, err := sql.Open("sqlite", env.onePath)
	if err != nil {
		t.Fatal(err)
	}
	var cnt, mod int
	var upd sql.NullInt64
	if err := oneDB.QueryRow(`SELECT (SELECT count(*) FROM cue WHERE content_id=1), COALESCE(hasModified,0), cueUpdateCount FROM content WHERE content_id=1`).Scan(&cnt, &mod, &upd); err != nil {
		_ = oneDB.Close()
		t.Fatal(err)
	}
	_ = oneDB.Close()
	if cnt != 0 || mod != 0 || upd.Valid {
		t.Fatalf("DB marks/rows not cleared: cues=%d hasModified=%d cueUpdateCount=%v", cnt, mod, upd)
	}

	// NOTE 8 check: second pass sees RepairStamp match and skips readLibraries completely.
	readCalls := 0
	writeBackReadLibsHook = func() { readCalls++ }
	defer func() { writeBackReadLibsHook = nil }()
	res2, err := WriteBackCues(ctx, env.root, WriteBackOptions{})
	if err != nil || res2.FilesUpdated != 0 || res2.TracksChanged != 0 || readCalls != 0 {
		t.Fatalf("expected 0-read / 0-write post-repair pass, got res=%+v readCalls=%d err=%v", res2, readCalls, err)
	}
}

func TestWriteBackPhantomCleanupFullSetAcrossANLZOneLibAndMDB(t *testing.T) {
	ctx := context.Background()
	env := setupPhantomTestStick(t, nil)

	var eight []rekordbox.Cue
	mdbPads := map[int]uint32{}
	for pad := 0; pad < 8; pad++ {
		eight = append(eight, rekordbox.Cue{HotCue: uint32(pad + 1), TimeMS: 23, Name: fmt.Sprintf("Cue %d", pad+1), ColorCode: byte(pad + 1)})
		mdbPads[pad] = 23
	}
	brokenDAT, brokenEXT := makeSwappedPairFromCues(t, env.origDAT, env.origEXT, eight)
	testfixture.Write(t, env.root, env.dat1Rel, brokenDAT)
	testfixture.Write(t, env.root, env.ext1Rel, brokenEXT)
	insertOneLibCues(t, env.onePath, 1, 1, sql.NullInt64{Int64: 1, Valid: true}, sql.NullInt64{}, eight)
	setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, mdbPads)
	refreshPhantomSnapStamp(t, env)

	res, err := WriteBackCues(ctx, env.root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues: %v", err)
	}
	if !res.OneLibraryUpdated || res.FilesUpdated != 3 {
		t.Fatalf("expected 3 updated files (DAT, EXT, exportLibrary.db), got %+v", res)
	}
	if cur, _ := os.ReadFile(env.dat1Path); !bytes.Equal(cur, env.origDAT) {
		t.Fatal("DAT not restored to clean original")
	}
	if cur, _ := os.ReadFile(env.ext1Path); !bytes.Equal(cur, env.origEXT) {
		t.Fatal("EXT not restored to clean original")
	}
	oneDB, _ := sql.Open("sqlite", env.onePath)
	var cnt, mod int
	var upd sql.NullInt64
	if err := oneDB.QueryRow(`SELECT (SELECT count(*) FROM cue WHERE content_id=1), COALESCE(hasModified,0), cueUpdateCount FROM content WHERE content_id=1`).Scan(&cnt, &mod, &upd); err != nil {
		_ = oneDB.Close()
		t.Fatal(err)
	}
	_ = oneDB.Close()
	if cnt != 0 || mod != 0 || upd.Valid {
		t.Fatalf("OneLibrary not cleaned: cnt=%d mod=%d upd=%v", cnt, mod, upd)
	}
	mDB, _ := sql.Open("sqlite", filepath.Join(env.dbDir, "m.db"))
	var td, qc, lp []byte
	var al int64
	if err := mDB.QueryRow(`SELECT trackData, quickCues, loops, COALESCE(activeOnLoadLoops,0) FROM PerformanceData WHERE trackId=1`).Scan(&td, &qc, &lp, &al); err != nil {
		_ = mDB.Close()
		t.Fatal(err)
	}
	_ = mDB.Close()
	deck, err := enginedb.ParseDeckCuesAndLoops(td, qc, lp, al)
	if err != nil {
		t.Fatal(err)
	}
	for s, hc := range deck.HotCues {
		if hc != nil {
			t.Fatalf("expected m.db phantom slot %d cleared, still set to %+v", s, hc)
		}
	}
}

func TestWriteBackPhantomCleanupPartialSetKeepingRealCue(t *testing.T) {
	ctx := context.Background()
	origReal := []rekordbox.Cue{{HotCue: 1, TimeMS: 10000}}
	env := setupPhantomTestStick(t, origReal)

	partial := []rekordbox.Cue{{HotCue: 1, TimeMS: 10000}}
	mdbPads := map[int]uint32{0: 10000}
	for pad := 1; pad < 8; pad++ {
		partial = append(partial, rekordbox.Cue{HotCue: uint32(pad + 1), TimeMS: 23})
		mdbPads[pad] = 23
	}
	brokenDAT, brokenEXT := makeSwappedPairFromCues(t, env.origDAT, env.origEXT, partial)
	testfixture.Write(t, env.root, env.dat1Rel, brokenDAT)
	testfixture.Write(t, env.root, env.ext1Rel, brokenEXT)
	insertOneLibCues(t, env.onePath, 1, 1, sql.NullInt64{Int64: 1, Valid: true}, sql.NullInt64{}, partial)
	setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, mdbPads)
	refreshPhantomSnapStamp(t, env)

	res, err := WriteBackCues(ctx, env.root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues: %v", err)
	}
	if !res.OneLibraryUpdated {
		t.Fatalf("expected OneLibrary updated, got %+v", res)
	}
	r, _ := compat.NewResolver(env.root)
	gotANLZ := readANLZCues(r, env.dat1Rel)
	if len(gotANLZ) != 1 || gotANLZ[0].HotCue != 1 || gotANLZ[0].TimeMS != 10000 {
		t.Fatalf("expected real 10000ms Hot Cue 1 kept in ANLZ, got %+v", gotANLZ)
	}
	lib, err := rekordbox.ReadSQLiteWhole(context.Background(), env.onePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(lib.Tracks) == 0 || len(lib.Tracks[0].Cues) != 1 || lib.Tracks[0].Cues[0].HotCue != 1 || lib.Tracks[0].Cues[0].TimeMS != 10000 {
		t.Fatalf("expected real 10000ms Hot Cue 1 kept in OneLibrary, got %+v", lib.Tracks[0].Cues)
	}
	// Restored back to original cue set -> DB marks also reset cleanly.
	oneDB, _ := sql.Open("sqlite", env.onePath)
	var mod int
	var upd sql.NullInt64
	_ = oneDB.QueryRow(`SELECT COALESCE(hasModified,0), cueUpdateCount FROM content WHERE content_id=1`).Scan(&mod, &upd)
	_ = oneDB.Close()
	if mod != 0 || upd.Valid {
		t.Fatalf("expected restored original cue track marks reset, got mod=%d upd=%v", mod, upd)
	}
	mDB, _ := sql.Open("sqlite", filepath.Join(env.dbDir, "m.db"))
	var td, qc, lp []byte
	var al int64
	_ = mDB.QueryRow(`SELECT trackData, quickCues, loops, COALESCE(activeOnLoadLoops,0) FROM PerformanceData WHERE trackId=1`).Scan(&td, &qc, &lp, &al)
	_ = mDB.Close()
	deck, _ := enginedb.ParseDeckCuesAndLoops(td, qc, lp, al)
	if deck.HotCues[0] == nil {
		t.Fatal("real Hot Cue 1 cleared from m.db")
	}
	for s := 1; s < 8; s++ {
		if deck.HotCues[s] != nil {
			t.Fatalf("phantom slot %d remained in m.db", s)
		}
	}
}

func TestWriteBackPhantomCleanupKeepsSingleCueAtStart(t *testing.T) {
	ctx := context.Background()
	single := []rekordbox.Cue{{HotCue: 1, TimeMS: 23}}
	env := setupPhantomTestStick(t, single)

	brokenDAT, brokenEXT := makeSwappedPairFromCues(t, env.origDAT, env.origEXT, single)
	testfixture.Write(t, env.root, env.dat1Rel, brokenDAT)
	testfixture.Write(t, env.root, env.ext1Rel, brokenEXT)
	insertOneLibCues(t, env.onePath, 1, 1, sql.NullInt64{Int64: 1, Valid: true}, sql.NullInt64{}, single)
	setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, map[int]uint32{0: 23})
	refreshPhantomSnapStamp(t, env)

	if _, err := WriteBackCues(ctx, env.root, WriteBackOptions{}); err != nil {
		t.Fatalf("WriteBackCues: %v", err)
	}
	r, _ := compat.NewResolver(env.root)
	gotANLZ := readANLZCues(r, env.dat1Rel)
	if len(gotANLZ) != 1 || gotANLZ[0].HotCue != 1 || gotANLZ[0].TimeMS != 23 {
		t.Fatalf("single cue at start (23ms) must stay, got %+v", gotANLZ)
	}
	lib, err := rekordbox.ReadSQLiteWhole(context.Background(), env.onePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(lib.Tracks[0].Cues) != 1 || lib.Tracks[0].Cues[0].TimeMS != 23 {
		t.Fatalf("single cue at start dropped from OneLibrary: %+v", lib.Tracks[0].Cues)
	}
}

func TestWriteBackPhantomCleanupNeverRemovesMDBCueAtAnotherTime(t *testing.T) {
	ctx := context.Background()
	t.Run("WithMDBTruth", func(t *testing.T) {
		env := setupPhantomTestStick(t, nil)
		// ANLZ holds r1 off-by-one shifted cue on Pad 2 (45000ms) + phantoms on Pads 3..8 at 23ms,
		// while m.db holds the real Hot Cue 1 at 45000ms.
		anlzBroken := []rekordbox.Cue{{HotCue: 2, TimeMS: 45000}}
		mdbPads := map[int]uint32{0: 45000}
		for pad := 3; pad <= 8; pad++ {
			anlzBroken = append(anlzBroken, rekordbox.Cue{HotCue: uint32(pad), TimeMS: 23})
			mdbPads[pad-1] = 23
		}
		brokenDAT, brokenEXT := makeSwappedPairFromCues(t, env.origDAT, env.origEXT, anlzBroken)
		testfixture.Write(t, env.root, env.dat1Rel, brokenDAT)
		testfixture.Write(t, env.root, env.ext1Rel, brokenEXT)
		insertOneLibCues(t, env.onePath, 1, 1, sql.NullInt64{Int64: 1, Valid: true}, sql.NullInt64{}, anlzBroken)
		setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, mdbPads)
		refreshPhantomSnapStamp(t, env)

		if _, err := WriteBackCues(ctx, env.root, WriteBackOptions{}); err != nil {
			t.Fatalf("WriteBackCues: %v", err)
		}
		r, _ := compat.NewResolver(env.root)
		gotANLZ := readANLZCues(r, env.dat1Rel)
		if len(gotANLZ) != 1 || gotANLZ[0].HotCue != 1 || gotANLZ[0].TimeMS != 45000 {
			t.Fatalf("expected shifted Pad 2 fixed to real m.db Pad 1 @ 45000ms without losing cue, got %+v", gotANLZ)
		}
	})

	t.Run("WithoutMDBTruthFixesLayoutAndLeavesCueValues", func(t *testing.T) {
		env := setupPhantomTestStick(t, nil)
		// No trackData in m.db -> repair removes phantoms and fixes layout, leaving the real 45000ms cue untouched.
		db, _ := sql.Open("sqlite", filepath.Join(env.dbDir, "m.db"))
		_, _ = db.Exec(`UPDATE PerformanceData SET trackData=NULL WHERE trackId=1`)
		_ = db.Close()
		anlzBroken := []rekordbox.Cue{{HotCue: 2, TimeMS: 45000}, {HotCue: 3, TimeMS: 23}, {HotCue: 4, TimeMS: 23}}
		brokenDAT, brokenEXT := makeSwappedPairFromCues(t, env.origDAT, env.origEXT, anlzBroken)
		testfixture.Write(t, env.root, env.dat1Rel, brokenDAT)
		testfixture.Write(t, env.root, env.ext1Rel, brokenEXT)
		insertOneLibCues(t, env.onePath, 1, 1, sql.NullInt64{Int64: 1, Valid: true}, sql.NullInt64{}, anlzBroken)
		refreshPhantomSnapStamp(t, env)

		if _, err := WriteBackCues(ctx, env.root, WriteBackOptions{}); err != nil {
			t.Fatalf("WriteBackCues: %v", err)
		}
		r, _ := compat.NewResolver(env.root)
		gotANLZ := readANLZCues(r, env.dat1Rel)
		if len(gotANLZ) != 1 || gotANLZ[0].HotCue != 2 || gotANLZ[0].TimeMS != 45000 {
			t.Fatalf("without m.db truth, expected layout repaired and Pad 2 @ 45000ms left intact, got %+v", gotANLZ)
		}
	})
}

func TestWriteBackOneLibraryMinimalCuePredicateKillsMutation(t *testing.T) {
	ctx := context.Background()
	const cueMS = uint32(28264)
	root := t.TempDir()
	dat1Rel, ext1Rel, _, _ := seedRealRekordboxOneLibraryStick(t, root)
	rawDAT, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dat1Rel)))
	if err != nil {
		t.Fatal(err)
	}
	rawEXT, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ext1Rel)))
	if err != nil {
		t.Fatal(err)
	}
	repDAT, _, err := rekordbox.RewriteANLZCues(rawDAT, []rekordbox.Cue{{HotCue: 1, TimeMS: cueMS}}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	repEXT, _, err := rekordbox.RewriteANLZCues(rawEXT, []rekordbox.Cue{{HotCue: 1, TimeMS: cueMS}}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, root, dat1Rel, repDAT)
	testfixture.Write(t, root, ext1Rel, repEXT)
	onePath := filepath.Join(root, "PIONEER", "rekordbox", "exportLibrary.db")
	dbDir := filepath.Join(root, "Engine Library", "Database2")

	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := compat.AudioHeader(r.FullPath("Contents/FixtureA/Track01.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	rbOffset := float64(info.RekordboxOffset())
	setMDBQuickCues(t, dbDir, 1, rbOffset, map[int]uint32{0: cueMS})

	// Set Synced != nil (takeOver == false!) and bump m.db lastEditTime so row enters buildChanges
	// and ANLZ agrees with m.db (anlzDiff == false), forcing evaluation of the line-984 predicate!
	bumpTrackAndUnsyncStamp := func() {
		db, _ := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
		_, _ = db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+10 WHERE id=1`)
		_ = db.Close()
		snap, err := rbstate.Load(dbDir)
		if err != nil {
			t.Fatal(err)
		}
		tr := snap.Tracks["contents/fixturea/track01.mp3"]
		tr.Synced = []byte("force-candidate-read-with-takeOver-false")
		sources, stamp, _ := exportNow(root, snap.Header.Layout)
		snap.Header.Sources, snap.Header.Stamp = sources, stamp
		if libs, _, lerr := ReadSources(ctx, r, root, sources, newKeyCache()); lerr == nil {
			for _, lib := range libs {
				if lib.Lib == nil {
					continue
				}
				for _, libTr := range lib.Lib.Tracks {
					if libTr.ID == 1 {
						for i := range tr.Refs {
							if tr.Refs[i].Kind == lib.Kind {
								tr.Refs[i].RecordHash = libTr.RecordHash
							}
						}
					}
				}
			}
		}
		if err := snap.Save(dbDir); err != nil {
			t.Fatal(err)
		}
	}

	// Subtest 1: OneLibrary holds default "Cue 1" + ColorCode=33 matching ANLZ's compact 28264ms cue
	// -> cuesValuesMatchIgnoringMinimalMissing must suppress spurious OneLibrary rewrite!
	insertOneLibCues(t, onePath, 1, 0, sql.NullInt64{}, sql.NullInt64{}, []rekordbox.Cue{{HotCue: 1, TimeMS: cueMS, Name: "Cue 1", ColorCode: 33}})
	bumpTrackAndUnsyncStamp()
	res1, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues minimal-match: %v", err)
	}
	if res1.TracksRead != 1 || res1.TracksChanged != 0 || res1.OneLibraryUpdated || res1.FilesUpdated != 0 {
		t.Fatalf("minimal default label/color difference caused spurious write: %+v", res1)
	}

	// Subtest 2: OneLibrary holds a genuinely different cue time (12000ms vs 28264ms in ANLZ & m.db)
	// with takeOver == false and anlzDiff == false -> OneLibrary MUST be updated!
	insertOneLibCues(t, onePath, 1, 0, sql.NullInt64{}, sql.NullInt64{}, []rekordbox.Cue{{HotCue: 1, TimeMS: 12000, Name: "Cue 1", ColorCode: 33}})
	bumpTrackAndUnsyncStamp()
	res2, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues real-onelib-diff: %v", err)
	}
	if res2.TracksRead != 1 || !res2.OneLibraryUpdated || res2.FilesUpdated != 1 {
		t.Fatalf("expected OneLibrary update when cue time genuinely differed, got %+v", res2)
	}
	lib, err := rekordbox.ReadSQLiteWhole(context.Background(), onePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(lib.Tracks) == 0 || len(lib.Tracks[0].Cues) != 1 || lib.Tracks[0].Cues[0].TimeMS != cueMS {
		t.Fatalf("OneLibrary cue was not updated to %d ms: %+v", cueMS, lib.Tracks[0].Cues)
	}
}
