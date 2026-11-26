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
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	ctx := context.Background()
	env := setupPhantomTestStick(t, nil)
	supersededDAT := "PIONEER/USBANLZ/P099/ANLZ0000.DAT"
	supersededEXT := "PIONEER/USBANLZ/P099/ANLZ0000.EXT"
	brokenDAT := swapCompactDATCueOrder(t, env.origDAT)
	testfixture.Write(t, env.root, supersededDAT, brokenDAT)
	testfixture.Write(t, env.root, supersededEXT, env.origEXT)
	testfixture.Write(t, env.root, ".enginex-backup/2000-01-01/"+supersededDAT, env.origDAT)
	testfixture.Write(t, env.root, ".enginex-backup/2000-01-01/"+supersededEXT, env.origEXT)

	// Move both snapshot Build history and live ownership so P099 is obsolete and P001 (dat1Rel) is current.
	snap, err := rbstate.Load(env.dbDir)
	if err != nil {
		t.Fatal(err)
	}
	snap.Tracks["contents/fixturea/track01.mp3"].Built.ANLZ = []rbstate.FileSig{{Rel: supersededDAT}, {Rel: supersededEXT}}
	if err := snap.Save(env.dbDir); err != nil {
		t.Fatal(err)
	}
	refreshPhantomSnapStamp(t, env)

	res, err := WriteBackCues(ctx, env.root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues: %v", err)
	}
	if res.TracksChanged != 0 || res.FilesUpdated != 0 {
		t.Fatalf("repair touched files even though current AnalysisPath is clean: %+v", res)
	}
	if cur, _ := os.ReadFile(filepath.Join(env.root, filepath.FromSlash(supersededDAT))); !bytes.Equal(cur, brokenDAT) {
		t.Fatalf("repair modified obsolete analysis %s although the track owns %s", supersededDAT, env.dat1Rel)
	}
}

func TestReviewRepairRefusesPPTHMismatchAndPreservesMergedGrid(t *testing.T) {
	ctx := context.Background()

	t.Run("LivePPTHMismatchRefused", func(t *testing.T) {
		env := setupPhantomTestStick(t, nil)
		foreign := rewritePPTHSection(t, swapCompactDATCueOrder(t, env.origDAT), "/Contents/Foreign/OtherTrack.mp3")
		testfixture.Write(t, env.root, env.dat1Rel, foreign)
		refreshPhantomSnapStamp(t, env)

		res, err := WriteBackCues(ctx, env.root, WriteBackOptions{})
		if err != nil {
			t.Fatalf("WriteBackCues: %v", err)
		}
		if res.TracksChanged != 0 || res.FilesUpdated != 0 {
			t.Fatalf("repair did not refuse ANLZ with mismatched live PPTH: %+v", res)
		}
		if cur, _ := os.ReadFile(env.dat1Path); !bytes.Equal(cur, foreign) {
			t.Fatal("mismatched live PPTH DAT was modified")
		}
	})

	t.Run("BackupPPTHMismatchRefused", func(t *testing.T) {
		env := setupPhantomTestStick(t, nil)
		testfixture.Write(t, env.root, env.dat1Rel, swapCompactDATCueOrder(t, env.origDAT))
		foreignBak := rewritePPTHSection(t, env.origDAT, "/Contents/Foreign/OtherTrack.mp3")
		testfixture.Write(t, env.root, filepath.ToSlash(filepath.Join(".enginex-backup", "2026-11-26", env.dat1Rel)), foreignBak)
		refreshPhantomSnapStamp(t, env)

		res, err := WriteBackCues(ctx, env.root, WriteBackOptions{})
		if err != nil {
			t.Fatalf("WriteBackCues: %v", err)
		}
		if res.TracksChanged != 0 || res.FilesUpdated != 0 {
			t.Fatalf("repair did not refuse ANLZ with mismatched backup PPTH: %+v", res)
		}
	})

	t.Run("OnlyBrokenVariantSelectedAndLiveGridMergedWithoutClobber", func(t *testing.T) {
		env := setupPhantomTestStick(t, nil)
		// Break ONLY EXT PCO2 order while leaving DAT byte-identical to backup.
		brokenEXT := append([]byte(nil), env.origEXT...)
		var pco2At []int
		for pos := int(binary.BigEndian.Uint32(env.origEXT[4:])); pos+12 <= len(env.origEXT); {
			if string(env.origEXT[pos:pos+4]) == "PCO2" {
				pco2At = append(pco2At, pos)
			}
			pos += int(binary.BigEndian.Uint32(env.origEXT[pos+8:]))
		}
		if len(pco2At) != 2 {
			t.Fatalf("expected 2 PCO2 sections, got %d", len(pco2At))
		}
		binary.BigEndian.PutUint32(brokenEXT[pco2At[0]+12:], 0)
		binary.BigEndian.PutUint32(brokenEXT[pco2At[1]+12:], 1)
		testfixture.Write(t, env.root, env.ext1Rel, brokenEXT)

		// Also shift Track 1's live m.db beat grid on the same target (no cue change).
		r, err := compat.NewResolver(env.root)
		if err != nil {
			t.Fatal(err)
		}
		offset, ok := trackRekordboxOffset(r, "Contents/FixtureA/Track01.mp3")
		if !ok {
			t.Fatal("missing audio offset")
		}
		const sr = 44100.0
		const step = 60.0 * sr / 132.0
		defGrid := []struct {
			pos      float64
			idx      int64
			numBeats uint32
		}{
			{pos: 0, idx: 0, numBeats: 8},
			{pos: 8 * 20671.875, idx: 8, numBeats: 0},
		}
		adjGrid := []struct {
			pos      float64
			idx      int64
			numBeats uint32
		}{
			{pos: 5000.0, idx: 0, numBeats: 8},
			{pos: 5000.0 + 8*step, idx: 8, numBeats: 0},
		}
		beatBlob := buildEngineBeatDataBlob(sr, 650*sr, defGrid, adjGrid, true)
		db, err := sql.Open("sqlite", filepath.Join(env.dbDir, "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE PerformanceData SET beatData=? WHERE trackId=1`, beatBlob); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET lastEditTime=lastEditTime+10 WHERE id=1`); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		_ = db.Close()
		refreshPhantomSnapStamp(t, env)

		wantBeats, _, err := decodeEngineBeatGrid(beatBlob, offset)
		if err != nil {
			t.Fatal(err)
		}

		res, err := WriteBackCues(ctx, env.root, WriteBackOptions{})
		if err != nil {
			t.Fatalf("WriteBackCues: %v", err)
		}
		if res.TracksChanged != 1 || res.FilesUpdated != 3 { // DAT grid + EXT cue-repair+grid + exportLibrary.db grid
			t.Fatalf("unexpected merged repair+grid result: %+v", res)
		}
		gotDAT, err := os.ReadFile(env.dat1Path)
		if err != nil {
			t.Fatal(err)
		}
		gotEXT, err := os.ReadFile(env.ext1Path)
		if err != nil {
			t.Fatal(err)
		}
		if rekordbox.ANLZNeedsCueRepair(gotEXT, true) {
			t.Fatal("EXT still needs cue repair after merged grid+repair write")
		}
		// Only broken EXT cues + DAT/EXT grid sections changed; DAT cue sections remained untouched.
		assertANLZNonTargetUnchanged(t, "merged DAT", env.origDAT, gotDAT, map[string]bool{"PQTZ": true})
		assertANLZNonTargetUnchanged(t, "merged EXT", brokenEXT, gotEXT, map[string]bool{"PCOB": true, "PCO2": true, "PQT2": true})
		if gotBeats := readANLZBeats(r, env.dat1Rel); !beatsEquivalent(gotBeats, wantBeats, 2.5, 0.05) {
			t.Fatalf("merged repair clobbered live grid edit: got %+v want %+v", gotBeats, wantBeats)
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

func TestWriteBackAdjustedMainCueExportsAsMemoryCueAndKeepsExistingMemoryCues(t *testing.T) {
	orig := []rekordbox.Cue{{TimeMS: 1000}}
	unadj := enginedb.DeckCues{SampleRate: 48000, MainCue: 96000, MainCueAdjusted: false}
	if got := mergeEngineCues(orig, unadj, 0, false); len(got) != 1 || got[0].TimeMS != 1000 {
		t.Fatalf("unadjusted main cue leaked into export: %+v", got)
	}
	adj := enginedb.DeckCues{SampleRate: 48000, MainCue: 96000, MainCueAdjusted: true}
	got := mergeEngineCues(orig, adj, 0, false)
	var seen1000, seen2000 bool
	for _, c := range got {
		if c.HotCue == 0 && !c.Loop && c.TimeMS == 1000 {
			seen1000 = true
		}
		if c.HotCue == 0 && !c.Loop && c.TimeMS == 2000 {
			seen2000 = true
		}
	}
	if !seen1000 || !seen2000 {
		t.Fatalf("adjusted main cue merge=%+v, want both 1000ms and 2000ms memory cues", got)
	}

	// Full ParseDeckCuesAndLoops -> WriteBackCues round-trip verifying adj=0 stays ignored and adj=1 writes back.
	ctx := context.Background()
	root := t.TempDir()
	dat1Rel, ext1Rel, _, _ := seedRealRekordboxOneLibraryStick(t, root)
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	offset, ok := trackRekordboxOffset(r, "Contents/FixtureA/Track01.mp3")
	if !ok {
		t.Fatal("missing offset")
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var origQC []byte
	if err := db.QueryRow(`SELECT quickCues FROM PerformanceData WHERE trackId=1`).Scan(&origQC); err != nil {
		t.Fatal(err)
	}
	rawQC, err := enginedb.QUncompress(origQC, 1<<20)
	if err != nil || len(rawQC) < 17 {
		t.Fatalf("QUncompress quickCues: len=%d err=%v", len(rawQC), err)
	}
	makeQC := func(mainSamples float64, adjusted byte) []byte {
		buf := append([]byte(nil), rawQC...)
		off := len(buf) - 17
		binary.BigEndian.PutUint64(buf[off:], math.Float64bits(mainSamples))
		buf[off+8] = adjusted
		binary.BigEndian.PutUint64(buf[off+9:], math.Float64bits(mainSamples))
		comp, cerr := enginedb.QCompress(buf)
		if cerr != nil {
			t.Fatal(cerr)
		}
		return comp
	}

	// Load-generated unadjusted main cue (adjusted=0) -> 0 changes!
	mainSamples := math.Round(2.0*44100.0 - offset)
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=1`, makeQC(mainSamples, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+5 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	resUnadj, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil || resUnadj.TracksChanged != 0 {
		t.Fatalf("unadjusted mainCue triggered write-back: %+v err=%v", resUnadj, err)
	}

	// Deliberate user-adjusted main cue at 2000ms (adjusted=1) -> written to ANLZ + OneLibrary!
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=1`, makeQC(mainSamples, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+5 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	resAdj, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil || resAdj.TracksChanged != 1 || resAdj.FilesUpdated == 0 {
		t.Fatalf("adjusted mainCue failed to write back: %+v err=%v", resAdj, err)
	}
	datBytes, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dat1Rel)))
	if err != nil {
		t.Fatal(err)
	}
	extBytes, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ext1Rel)))
	if err != nil {
		t.Fatal(err)
	}
	aDat, err := rekordbox.ParseAnalysis(datBytes)
	if err != nil {
		t.Fatal(err)
	}
	aExt, err := rekordbox.ParseAnalysis(extBytes)
	if err != nil {
		t.Fatal(err)
	}
	aDat.Merge(aExt)
	foundMem := false
	for _, c := range aDat.Cues {
		if c.HotCue == 0 && !c.Loop && c.TimeMS == 2000 {
			foundMem = true
		}
	}
	if !foundMem {
		t.Fatalf("adjusted 2000ms main cue missing from written ANLZ: %+v", aDat.Cues)
	}
}

func TestWriteBackPreservesSubTwoPointFiveMSGridNudge(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dat1Rel, ext1Rel, _, _ := seedRealRekordboxOneLibraryStick(t, root)
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	offset, ok := trackRekordboxOffset(r, "Contents/FixtureA/Track01.mp3")
	if !ok {
		t.Fatal("missing offset")
	}
	aExtOrig, err := rekordbox.ParseAnalysis(mustReadFile(t, filepath.Join(root, filepath.FromSlash(ext1Rel))))
	if err != nil {
		t.Fatal(err)
	}
	aDatOrig, err := rekordbox.ParseAnalysis(mustReadFile(t, filepath.Join(root, filepath.FromSlash(dat1Rel))))
	if err != nil {
		t.Fatal(err)
	}
	aDatOrig.Merge(aExtOrig)
	if len(aDatOrig.Beats) < 2 {
		t.Fatalf("fixture has %d beats", len(aDatOrig.Beats))
	}

	const sr = 44100.0
	orig0US := float64(aDatOrig.Beats[0].TimeMS)*1000.0 + float64(aDatOrig.Beats[0].SubUS)
	base0 := orig0US*sr/1e6 - offset
	lastIdx := len(aDatOrig.Beats) - 1
	origLastUS := float64(aDatOrig.Beats[lastIdx].TimeMS)*1000.0 + float64(aDatOrig.Beats[lastIdx].SubUS)
	baseLast := origLastUS*sr/1e6 - offset
	nudgeSamples := 1.5 * sr / 1000.0 // 1.5 ms (< 2.5 ms old threshold)
	nudgeBeats := uint32(lastIdx)

	defGrid := []struct {
		pos      float64
		idx      int64
		numBeats uint32
	}{
		{pos: base0, idx: 0, numBeats: nudgeBeats},
		{pos: baseLast, idx: int64(lastIdx), numBeats: 0},
	}

	db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Unadjusted 2.0 ms drift (def == adj, so adjDiff == false) -> stays within 2.5 ms tolerance (0 writes).
	driftSamples := 2.0 * sr / 1000.0
	unadjGrid := []struct {
		pos      float64
		idx      int64
		numBeats uint32
	}{
		{pos: base0 + driftSamples, idx: 0, numBeats: nudgeBeats},
		{pos: baseLast + driftSamples, idx: int64(lastIdx), numBeats: 0},
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET beatData=? WHERE trackId=1`, buildEngineBeatDataBlob(sr, 20*sr, unadjGrid, unadjGrid, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+5 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	resUnadj, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil || resUnadj.TracksChanged != 0 {
		t.Fatalf("unadjusted 2.0 ms drift triggered grid write-back: %+v err=%v", resUnadj, err)
	}

	// Explicit adjusted +1.5 ms bulk nudge (adjDiff == true) -> MUST write back!
	adjGrid := []struct {
		pos      float64
		idx      int64
		numBeats uint32
	}{
		{pos: base0 + nudgeSamples, idx: 0, numBeats: nudgeBeats},
		{pos: baseLast + nudgeSamples, idx: int64(lastIdx), numBeats: 0},
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET beatData=? WHERE trackId=1`, buildEngineBeatDataBlob(sr, 20*sr, defGrid, adjGrid, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+5 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	resAdj, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil || resAdj.TracksChanged != 1 || len(resAdj.Changes) != 1 || !strings.Contains(strings.Join(resAdj.Changes[0].Fields, ","), "grid:anlz(") {
		t.Fatalf("adjusted 1.5 ms nudge was dropped: %+v err=%v", resAdj, err)
	}
	aDatNew, err := rekordbox.ParseAnalysis(mustReadFile(t, filepath.Join(root, filepath.FromSlash(dat1Rel))))
	if err != nil {
		t.Fatal(err)
	}
	aExtNew, err := rekordbox.ParseAnalysis(mustReadFile(t, filepath.Join(root, filepath.FromSlash(ext1Rel))))
	if err != nil {
		t.Fatal(err)
	}
	aDatNew.Merge(aExtNew)
	gotUS := float64(aDatNew.Beats[0].TimeMS)*1000.0 + float64(aDatNew.Beats[0].SubUS)
	origUS := float64(aDatOrig.Beats[0].TimeMS)*1000.0 + float64(aDatOrig.Beats[0].SubUS)
	if math.Abs((gotUS-origUS)-1500.0) > 20.0 {
		t.Fatalf("beat[0] shift=%.1f us, want ~1500 us (orig=%.1f got=%.1f)", gotUS-origUS, origUS, gotUS)
	}

	// Clearing Synced on Track 1 to force candidate re-read after write-back must still be a zero-change no-op!
	dbDir := filepath.Join(root, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	snap.Tracks["contents/fixturea/track01.mp3"].Synced = nil
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}
	resNoop, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil || resNoop.TracksChanged != 0 {
		t.Fatalf("re-read after 1.5 ms grid write-back triggered duplicate write: %+v err=%v", resNoop, err)
	}
}

func mustReadFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func seedSixTrackUnsettledStick(t *testing.T, root string) {
	t.Helper()
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	testfixture.LargeExport(t, root, 6)
	fix3WritePlainOneLibrary(t, root)
	onePath := filepath.Join(root, "PIONEER", "rekordbox", "exportLibrary.db")
	odb, err := sql.Open("sqlite", onePath)
	if err != nil {
		t.Fatal(err)
	}
	for id := 3; id <= 6; id++ {
		if _, err := odb.Exec(
			`INSERT INTO content(content_id,title,bpmx100,length,path,samplingRate,djComment,analysisDataFilePath,genre_id,label_id) VALUES(?,?,12400,4,?,44100,'','/PIONEER/USBANLZ/P001/ANLZ0000.DAT',20,30)`,
			id, fmt.Sprintf("Track %d", id), "/"+testfixture.LargeTrackPath(id),
		); err != nil {
			_ = odb.Close()
			t.Fatal(err)
		}
	}
	_ = odb.Close()
	if _, err := ConvertAtMount(context.Background(), root, MountOptions{Full: true}); err != nil {
		t.Fatalf("ConvertAtMount(Full): %v", err)
	}
	if _, err := FillInBackground(context.Background(), root, FillOptions{}); err != nil {
		t.Fatalf("FillInBackground: %v", err)
	}
	res, err := ConvertAtMount(context.Background(), root, MountOptions{Full: true})
	if err != nil || !res.Pending.None() {
		t.Fatalf("ConvertAtMount(Full #2) = (%+v, %v), want Pending.None()", res, err)
	}
	dir := dbDir(root)
	snap, err := rbstate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	mdb, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer mdb.Close()
	for _, tr := range snap.Tracks {
		var edit int64
		if err := mdb.QueryRow(`SELECT COALESCE(lastEditTime,0) FROM Track WHERE id=?`, tr.EngineID).Scan(&edit); err != nil {
			t.Fatal(err)
		}
		tr.Edit = edit
		tr.Synced = nil
	}
	if err := snap.Save(dir); err != nil {
		t.Fatal(err)
	}
}

func TestWriteBackOnEjectSkipsBulkHistoricalSettleWhileWritingRealEdit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	seedSixTrackUnsettledStick(t, root)

	dir := dbDir(root)
	snap, err := rbstate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Edit only Track 6 on the deck (both quickCues and lastEditTime bumped); leave Tracks 1..5
	// with Synced == nil and Edit == lastEditTime (historical unsettled backlog).
	target := snap.Tracks[rbstate.FoldPath(testfixture.LargeTrackPath(6))]
	if target == nil {
		t.Fatal("missing track 6 in snapshot")
	}
	mdb, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer mdb.Close()
	qc, err := enginedb.QCompress(makeMainCueBlob(500, 500, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=?`, qc, target.EngineID); err != nil {
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`UPDATE Track SET comment='Eject Real Edit' WHERE id=?`, target.EngineID); err != nil {
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+10 WHERE id=?`, target.EngineID); err != nil {
		t.Fatal(err)
	}

	res, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, OnEject: true})
	if err != nil {
		t.Fatalf("WriteBackCues(OnEject): %v", err)
	}
	if res.TracksRead != 1 || res.TracksChanged != 1 || res.PendingSettle != 5 {
		t.Fatalf("OnEject read/settled wrong subset: %+v, want TracksRead=1 TracksChanged=1 PendingSettle=5", res)
	}
	if len(res.Changes) != 1 || res.Changes[0].EngineID != target.EngineID {
		t.Fatalf("OnEject changes = %+v, want engineID %d", res.Changes, target.EngineID)
	}
	after, err := rbstate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Tracks[target.Key].Synced) == 0 {
		t.Fatal("dirty track 6 was not marked Synced after OnEject write-back")
	}
	unsettled := 0
	for k, tr := range after.Tracks {
		if k != target.Key && tr.Synced == nil {
			unsettled++
		}
	}
	if unsettled != 5 {
		t.Fatalf("remaining unsettled count = %d, want 5", unsettled)
	}
}

func TestCueWatcherSettlesAdoptedLibraryInBoundedIdleBatchesWithCheckpoints(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	seedSixTrackUnsettledStick(t, root)
	dir := dbDir(root)

	// 1. Bounded SettleBatch=2 checkpoints 2 tracks per pass (6 -> 4 -> 2 -> 0).
	var sess CueSession
	for _, want := range []struct {
		read, pending, synced int
	}{
		{read: 2, pending: 4, synced: 2},
		{read: 2, pending: 2, synced: 4},
		{read: 2, pending: 0, synced: 6},
		{read: 0, pending: 0, synced: 6},
	} {
		res, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, SettleBatch: 2, Session: &sess})
		if err != nil {
			t.Fatalf("batched settle: %v", err)
		}
		if res.TracksRead != want.read || res.PendingSettle != want.pending || res.TracksChanged != 0 {
			t.Fatalf("batched settle got %+v, want read=%d pending=%d changed=0", res, want.read, want.pending)
		}
		snap, err := rbstate.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		gotSynced := 0
		for _, tr := range snap.Tracks {
			if len(tr.Synced) > 0 {
				gotSynced++
			}
		}
		if gotSynced != want.synced {
			t.Fatalf("checkpointed Synced=%d, want %d", gotSynced, want.synced)
		}
	}

	// 2. CueWatcher schedules idle settle automatically when mounting a stick with Synced == nil.
	snap, err := rbstate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range snap.Tracks {
		tr.Synced = nil
	}
	if err := snap.Save(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(SetCueRunDirForTest(t.TempDir()))
	prevFlag := setUSBPreparingFlagPath(filepath.Join(t.TempDir(), "absent-flag"))
	t.Cleanup(func() { setUSBPreparingFlagPath(prevFlag) })
	w := CueWatcherForTest([]string{root}, 20*time.Millisecond, 50*time.Millisecond)
	var passes atomic.Int32
	settled := make(chan struct{})
	WrapCuePassForTest(w, func(next CuePass) CuePass {
		return func(pctx context.Context, m string, opts WriteBackOptions) (WriteBackResult, error) {
			passes.Add(1)
			r, err := next(pctx, m, opts)
			if err == nil && r.PendingSettle == 0 {
				select {
				case <-settled:
				default:
					close(settled)
				}
			}
			return r, err
		}
	})
	wctx, cancel := context.WithCancel(ctx)
	wdone := make(chan struct{})
	go func() {
		defer close(wdone)
		_ = w.Run(wctx)
	}()
	select {
	case <-settled:
	case <-time.After(5 * time.Second):
		cancel()
		<-wdone
		t.Fatalf("CueWatcher did not settle Synced==nil stick in idle time (passes=%d)", passes.Load())
	}
	cancel()
	<-wdone
	if after, err := rbstate.Load(dir); err != nil {
		t.Fatal(err)
	} else {
		for k, tr := range after.Tracks {
			if len(tr.Synced) == 0 {
				t.Fatalf("track %s still Synced==nil after CueWatcher idle settle", k)
			}
		}
	}
}
