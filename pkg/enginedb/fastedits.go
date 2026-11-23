package enginedb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"log"
	"math"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// A track a fast import listed (fingerprint "fast:" + the export record's hash) has no analysis
// until its first full import, minutes later on a large stick (#301). A deck that loads it
// meanwhile has Engine analyse it, and the DJ may edit it then: set, move, rename, recolour or
// delete hot cues, save loops, adjust or lock the grid, rate it (#328). The full import gives
// the row rekordbox's analysis and keeps what the DJ did, field by field, against what the fast
// import left in the row (fastBase):
//
//   - hot cue and saved loop slots, one by one: a slot that still holds what the fast import
//     wrote, or the same moved by Engine's own correction of a marked row, or that is empty on
//     both, is rekordbox's; any other is the DJ's. A loop's active-on-load bit goes with its slot.
//   - the main cue: the DJ's when Engine says it was adjusted (the flag after the hot cues).
//   - the grid: the DJ's when it is locked or its adjusted grid is not Engine's own analysed one
//     (the rule of realignKept), with the Track's BPM; Engine's own analysis is replaced.
//   - the rating: the DJ's when it is not the one the fast import wrote.
//
// What the DJ kept is a deck edit from then on: the row is not what the import wrote, so it has
// no BridgeWritten digest, and the next import keeps it (importKeeps) as it keeps any deck edit.

// engineLoadShifts are what Engine 5.3.0 moves a marked row by when it loads it with a decoder
// attached (one_offset_rule_test: an MP3 by 2292 samples); it clears the mark either way.
var engineLoadShifts = []float64{0, 2292}

// FastBaseFile is the stage's note of what the fast import left in its fast rows, by track id with
// a check of the record, for the publish to tell the DJ's edits made after the snapshot from
// Engine's (MergeLive). It is in the stage only and goes with it.
const FastBaseFile = "fast-base.db"

// fastBase is what a fast import left in a track's row for an export record: the record's rating,
// and a OneLibrary export's hot cues, loops and active-on-load loops without the file's offset,
// with Engine's mark.
type fastBase struct {
	rating      int64
	cues, loops []byte
	active      int64
}

// fastBaseOf is the fastBase of export track t, as the fast import computes it (sync.go).
func fastBaseOf(t rekordbox.Track) fastBase {
	b := fastBase{rating: int64(t.Rating)}
	if len(t.Cues) > 0 {
		if t.BPM >= 499.5 {
			t.BPM = 0
		}
		if p, e := encodeCuesOnly(t, t.Cues); e == nil {
			b.cues, b.loops, b.active = p.QuickCues, p.Loops, int64(p.ActiveLoops)
		}
	}
	return b
}

// deckRow is a fast row as the deck left it.
type deckRow struct {
	beats, cues, loops []byte
	active, mark       sql.NullInt64
	locked             bool
	rating             sql.NullInt64
}

// fastMerge is the row the full import leaves: perf's, with what the DJ did kept. A nil field
// is perf's.
type fastMerge struct {
	beats, cues, loops []byte
	active             any
	grid, rating       bool // the DJ's grid (and the Track's BPM with it), the DJ's rating
}

func (m fastMerge) deck() bool {
	return m.beats != nil || m.cues != nil || m.loops != nil || m.active != nil
}

// mergeFast merges the deck's row of a track a fast import left as base into perf.
func mergeFast(row deckRow, base fastBase, perf Performance) fastMerge {
	var m fastMerge
	m.rating = row.rating.Valid && row.rating.Int64 != base.rating
	if row.mark.Valid && row.mark.Int64 != 0 {
		// Engine clears its import mark when it loads the row: never loaded, so nothing of the
		// deck's in it (a rating can be set in the browser, without a load).
		return m
	}
	if row.beats != nil && (row.locked || !engineOwnGrid(row.beats)) {
		m.beats, m.grid = row.beats, true
	}
	rowCues, rowTrailer, ok1 := cueSlots(row.cues)
	baseCues, baseTrailer, ok2 := cueSlots(base.cues)
	newCues, newTrailer, ok3 := cueSlots(perf.QuickCues)
	rowLoops, ok4 := loopSlots(row.loops)
	baseLoops, ok5 := loopSlots(base.loops)
	newLoops, ok6 := loopSlots(perf.Loops)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
		return m // a blob not in Engine's layout: rekordbox's, as before #328
	}
	// The fast import's cues are in the row only if one of them still is where it put it: a
	// track listed fast without them (an in-place import out of time) has none, and Engine's
	// empty slots must not read as the DJ deleting what was never there. A DJ who deleted every
	// one of them gets rekordbox's back, as before #328.
	seen := false
	for i := range rowCues {
		seen = seen || !emptyCue(baseCues[i]) && !emptyCue(rowCues[i]) && shiftedBy(cuePos(baseCues[i]), cuePos(rowCues[i]))
	}
	for i := range rowLoops {
		seen = seen || i < len(baseLoops) && !emptyLoop(baseLoops[i]) && !emptyLoop(rowLoops[i]) && sameLoopAt(rowLoops[i], baseLoops[i])
	}
	baseActive := base.active
	if !seen {
		baseCues, baseTrailer, baseLoops, baseActive = [8][]byte{}, nil, nil, 0
	}
	if row.cues != nil && newTrailer != nil {
		var cues bytes.Buffer
		changed := false
		cues.Write(cueCount)
		for i := range rowCues {
			slot := mergeCue(rowCues[i], baseCues[i], newCues[i])
			changed = changed || !bytes.Equal(slot, newCues[i])
			cues.Write(slot)
		}
		trailer := newTrailer
		if rowTrailer[8] != 0 && (baseTrailer == nil || !sameMainCue(rowTrailer, baseTrailer)) {
			// The DJ's main cue, with rekordbox's as the one imported.
			trailer = append(append([]byte(nil), rowTrailer[:9]...), newTrailer[9:]...)
			changed = changed || !bytes.Equal(trailer, newTrailer)
		}
		cues.Write(trailer)
		if changed {
			if enc, e := QCompress(cues.Bytes()); e == nil {
				m.cues = enc
			}
		}
	}
	if len(rowLoops) == 0 || len(rowLoops) != len(newLoops) {
		return m
	}
	loops := append([]byte(nil), perf.Loops[:8]...)
	active := int64(perf.ActiveLoops)
	changed := false
	for i := range rowLoops {
		slot, bit := newLoops[i], int64(1)<<uint(i)
		var b []byte
		if i < len(baseLoops) {
			b = baseLoops[i]
		}
		slot = mergeLoop(rowLoops[i], b, newLoops[i])
		switch {
		case !bytes.Equal(slot, newLoops[i]) && !(emptyLoop(slot) && emptyLoop(newLoops[i])):
			active = active&^bit | row.active.Int64&bit // the DJ's loop, and whether it is active on load
		case !emptyLoop(rowLoops[i]) && row.active.Int64&bit != baseActive&bit:
			active = active&^bit | row.active.Int64&bit // only switched on or off at the deck
		}
		changed = changed || !bytes.Equal(slot, newLoops[i])
		loops = append(loops, slot...)
	}
	if changed {
		m.loops = loops
	}
	if active != int64(perf.ActiveLoops) {
		m.active = active
	}
	return m
}

// engineOwnGrid reports whether beatData is a grid Engine analysed and nobody adjusted: its
// adjusted grid is its default one (realignKept). One it cannot read counts as Engine's.
func engineOwnGrid(beats []byte) bool {
	raw, e := QUncompress(beats, 8<<20)
	if e != nil {
		return true
	}
	def, adj, ok := beatGrids(raw)
	return !ok || bytes.Equal(def, adj)
}

// cueCount is quickCues' slot count.
var cueCount = []byte{0, 0, 0, 0, 0, 0, 0, 8}

// cueSlots splits uncompressed quickCues into its eight hot cue slots (label length, label,
// position, colour) and the main cue trailer (position, adjusted flag, position as imported).
// A nil blob is eight empty slots and no trailer.
func cueSlots(blob []byte) (slots [8][]byte, trailer []byte, ok bool) {
	if blob == nil {
		return slots, nil, true
	}
	raw, e := QUncompress(blob, 64<<10)
	if e != nil {
		return slots, nil, false
	}
	at, ok := cuePositions(raw)
	if !ok {
		return slots, nil, false
	}
	start := 8
	for i := 0; i < 8; i++ {
		slots[i] = raw[start : at[i]+12]
		start = at[i] + 12
	}
	return slots, raw[at[8]:], true
}

// loopSlots splits a loops blob into its slots (label length, label, start, end, the two set
// flags, colour). A nil blob has none.
func loopSlots(blob []byte) ([][]byte, bool) {
	if blob == nil {
		return nil, true
	}
	at, ok := loopPositions(blob)
	if !ok {
		return nil, false
	}
	var out [][]byte
	start := 8
	for i := 0; i < len(at); i += 2 {
		out = append(out, blob[start:at[i]+22])
		start = at[i] + 22
	}
	return out, true
}

func cuePos(slot []byte) float64 {
	return math.Float64frombits(big.Uint64(slot[len(slot)-12:]))
}

func emptyCue(slot []byte) bool { return slot == nil || cuePos(slot) < 0 }

// mergeCue is a hot cue slot after the full import: rekordbox's (now) where the row still holds
// what the fast import put in it (base), or that moved by Engine's own correction; the DJ's where
// the DJ set, moved or deleted it; the DJ's name and colour at rekordbox's position where the DJ
// renamed or recoloured it without moving it.
func mergeCue(row, base, now []byte) []byte {
	switch {
	case emptyCue(row) && (emptyCue(base) || emptyCue(now)):
		return now
	case emptyCue(row) || emptyCue(base) || !shiftedBy(cuePos(base), cuePos(row)):
		return row // deleted, set, or moved at the deck
	case bytes.Equal(row[:len(row)-12], base[:len(base)-12]) && bytes.Equal(row[len(row)-4:], base[len(base)-4:]) || emptyCue(now):
		if emptyCue(now) {
			return row
		}
		return now
	}
	out := append([]byte(nil), row...)
	copy(out[len(out)-12:len(out)-4], now[len(now)-12:len(now)-4])
	return out
}

// sameMainCue: the main cue of a row's trailer still is base's.
func sameMainCue(row, base []byte) bool {
	return shiftedBy(math.Float64frombits(big.Uint64(base)), math.Float64frombits(big.Uint64(row)))
}

// shiftedBy reports whether position row is base, or base moved by Engine's own correction.
func shiftedBy(base, row float64) bool {
	for _, s := range engineLoadShifts {
		if math.Abs(base-s-row) < 0.5 {
			return true
		}
	}
	return false
}

func loopAt(slot []byte) (start, end float64, set []byte) {
	p := slot[len(slot)-22:]
	return math.Float64frombits(little.Uint64(p)), math.Float64frombits(little.Uint64(p[8:])), p[16:18]
}

func emptyLoop(slot []byte) bool {
	if slot == nil {
		return true
	}
	start, end, set := loopAt(slot)
	return start < 0 && end < 0 || set[0] == 0 && set[1] == 0
}

// sameLoopAt: a row's loop slot still starts and ends where base's does, or moved by Engine's
// own correction.
func sameLoopAt(row, base []byte) bool {
	rs, re, _ := loopAt(row)
	bs, be, _ := loopAt(base)
	for _, s := range engineLoadShifts {
		if math.Abs(bs-s-rs) < 0.5 && math.Abs(be-s-re) < 0.5 {
			return true
		}
	}
	return false
}

// mergeLoop is mergeCue for a saved loop slot: start and end, the set flags and the colour.
func mergeLoop(row, base, now []byte) []byte {
	switch {
	case emptyLoop(row) && (emptyLoop(base) || emptyLoop(now)):
		return now
	case emptyLoop(row) || emptyLoop(base) || !sameLoopAt(row, base):
		return row
	case bytes.Equal(row[:len(row)-22], base[:len(base)-22]) && bytes.Equal(row[len(row)-6:], base[len(base)-6:]) || emptyLoop(now):
		if emptyLoop(now) {
			return row
		}
		return now
	}
	out := append([]byte(nil), row...)
	copy(out[len(out)-22:len(out)-6], now[len(now)-22:len(now)-6])
	return out
}

// WriteFastBase notes in stage, beside its snapshot, the fastBase of every track of l that the
// snapshot's state has as a fast row of l's record: the publish needs it for the DJ's edits made
// on such a row after the snapshot (carryFastEdits). It is the state's own entry for the row
// (BridgeFastBase) where it has one, else what the fast import computes from l. A stage without a
// state has none.
func WriteFastBase(ctx context.Context, stage string, l *rekordbox.Library) error {
	path := filepath.Join(stage, FastBaseFile)
	tmp := path + ".tmp"
	for _, p := range []string{path, tmp, tmp + "-journal"} {
		if e := os.Remove(p); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	state := filepath.Join(stage, stateFile)
	if _, e := os.Stat(state); errors.Is(e, os.ErrNotExist) {
		return nil
	}
	fast, stored, e := fastRows(ctx, state)
	if e != nil || len(fast) == 0 {
		return e
	}
	db, e := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: tmp}).String()+"?_pragma=journal_mode(MEMORY)&_pragma=synchronous(OFF)")
	if e != nil {
		return e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	// About 25 bytes a track without cues: the track id is the row id, the record a 64-bit check.
	if _, e = tx.ExecContext(ctx, `CREATE TABLE FastBase(trackId INTEGER PRIMARY KEY, record INTEGER NOT NULL, rating INTEGER NOT NULL, quickCues BLOB, loops BLOB, active INTEGER NOT NULL)`); e != nil {
		return e
	}
	insert, e := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO FastBase VALUES(?,?,?,?,?,?)`)
	if e != nil {
		return e
	}
	defer insert.Close()
	for _, t := range l.Tracks {
		if e = ctx.Err(); e != nil {
			return e
		}
		if t.RecordHash == "" {
			t.RecordHash = rekordbox.RecordHash(t)
		}
		fp := "fast:" + t.RecordHash
		ids := fast[fp]
		if len(ids) == 0 {
			continue
		}
		computed := fastBaseOf(t)
		for _, id := range ids {
			b := computed
			if s, ok := stored[id]; ok && s.record == recordCheck(fp) {
				b = s.fastBase
			}
			if _, e = insert.ExecContext(ctx, id, recordCheck(fp), b.rating, b.cues, b.loops, b.active); e != nil {
				return e
			}
		}
	}
	if e = insert.Close(); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	if e = db.Close(); e != nil {
		return e
	}
	if e = syncPath(tmp); e != nil {
		return e
	}
	return os.Rename(tmp, path)
}

// recordCheck names a fast row's fingerprint in 64 bits.
func recordCheck(fingerprint string) int64 {
	h := sha256.Sum256([]byte(fingerprint))
	return int64(big.Uint64(h[:]))
}

// storedBase is a BridgeFastBase entry: the base and the check of the fingerprint it is for.
type storedBase struct {
	fastBase
	record int64
}

// fastRows are the track ids of the fast rows of the state database at path, by fingerprint, and
// the state's BridgeFastBase entries for them (none in a state of an earlier build).
func fastRows(ctx context.Context, path string) (map[string][]int64, map[int64]storedBase, error) {
	db, e := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String())
	if e != nil {
		return nil, nil, e
	}
	defer db.Close()
	rows, e := db.QueryContext(ctx, `SELECT trackId,fingerprint FROM BridgeState WHERE fingerprint LIKE 'fast:%'`)
	if e != nil {
		return nil, nil, e
	}
	out := map[string][]int64{}
	for rows.Next() {
		var id int64
		var fp string
		if e = rows.Scan(&id, &fp); e != nil {
			rows.Close()
			return nil, nil, e
		}
		out[fp] = append(out[fp], id)
	}
	rows.Close()
	if e = rows.Err(); e != nil || len(out) == 0 {
		return out, nil, e
	}
	stored := map[int64]storedBase{}
	var n int
	if e = db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='BridgeFastBase'`).Scan(&n); e != nil || n == 0 {
		return out, stored, e
	}
	rows, e = db.QueryContext(ctx, `SELECT f.trackId,f.record,f.rating,f.quickCues,f.loops,f.active FROM BridgeFastBase f JOIN BridgeState s ON s.trackId=f.trackId WHERE s.fingerprint LIKE 'fast:%'`)
	if e != nil {
		return nil, nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var b storedBase
		if e = rows.Scan(&id, &b.record, &b.rating, &b.cues, &b.loops, &b.active); e != nil {
			return nil, nil, e
		}
		stored[id] = b
	}
	return out, stored, rows.Err()
}

func syncPath(p string) error {
	f, e := os.Open(p)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

// carryFastEdits is mergeFast for a build that replaces the live library (merge.go): a track the
// live library still has as a fast row, which Engine wrote after the snapshot, keeps what the DJ
// did on it. The build's row (rekordbox's, with what the DJ had done before the snapshot) stands
// for perf; the stage's FastBaseFile for what the fast import left.
func carryFastEdits(ctx context.Context, tx *sql.Tx) error {
	rows, e := tx.QueryContext(ctx, `SELECT m.trackId,s.fingerprint,f.record,l.beatData,l.quickCues,l.loops,l.activeOnLoadLoops,l.thirdPartySourceId,COALESCE(lt.isBeatGridLocked,0),lt.rating,lt.bpm,lt.bpmAnalyzed,
		f.rating,f.quickCues,f.loops,f.active,m.beatData,m.quickCues,m.loops,m.activeOnLoadLoops
		FROM main.PerformanceData m JOIN live.PerformanceData l ON l.trackId=m.trackId JOIN live.Track lt ON lt.id=m.trackId
		JOIN livebridge.BridgeState s ON s.trackId=m.trackId JOIN fastbase.FastBase f ON f.trackId=m.trackId
		WHERE m.trackId IN (SELECT id FROM temp.touched) AND m.trackId IN (SELECT id FROM temp.changed)`)
	if e != nil {
		return e
	}
	type edit struct {
		id                 int64
		beats, cues, loops []byte
		active             any
		m                  fastMerge
		rating             sql.NullInt64
		bpm, bpmA          any
		build              []byte
	}
	var edits []edit
	for rows.Next() {
		var id int64
		var row deckRow
		var base fastBase
		var mb, mc, ml []byte
		var ma sql.NullInt64
		var bpm, bpmA any
		var fp string
		var record int64
		if e = rows.Scan(&id, &fp, &record, &row.beats, &row.cues, &row.loops, &row.active, &row.mark, &row.locked, &row.rating, &bpm, &bpmA,
			&base.rating, &base.cues, &base.loops, &base.active, &mb, &mc, &ml, &ma); e != nil {
			rows.Close()
			return e
		}
		if recordCheck(fp) != record {
			continue // the live row is not the fast row of the record noted (no longer fast, or another export)
		}
		m := mergeFast(row, base, Performance{Beats: mb, QuickCues: mc, Loops: ml, ActiveLoops: uint32(ma.Int64)})
		if !m.deck() && !m.rating {
			continue
		}
		x := edit{id: id, beats: mb, cues: mc, loops: ml, active: ma, m: m, rating: row.rating, bpm: bpm, bpmA: bpmA, build: writtenDigest(mb, mc, ml)}
		if m.beats != nil {
			x.beats = m.beats
		}
		if m.cues != nil {
			x.cues = m.cues
		}
		if m.loops != nil {
			x.loops = m.loops
		}
		if m.active != nil {
			x.active = m.active
		}
		edits = append(edits, x)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return e
	}
	for _, x := range edits {
		if x.m.deck() {
			if _, e = tx.ExecContext(ctx, `UPDATE main.PerformanceData SET beatData=?,quickCues=?,loops=?,activeOnLoadLoops=? WHERE trackId=?`,
				x.beats, x.cues, x.loops, x.active, x.id); e != nil {
				return e
			}
			if !bytes.Equal(x.build, writtenDigest(x.beats, x.cues, x.loops)) {
				if _, e = tx.ExecContext(ctx, `DELETE FROM bridge.BridgeWritten WHERE trackId=?`, x.id); e != nil {
					return e
				}
			}
		}
		if x.m.grid {
			if _, e = tx.ExecContext(ctx, `UPDATE main.Track SET bpm=?,bpmAnalyzed=? WHERE id=?`, x.bpm, x.bpmA, x.id); e != nil {
				return e
			}
		}
		if x.m.rating {
			if _, e = tx.ExecContext(ctx, `UPDATE main.Track SET rating=? WHERE id=?`, x.rating, x.id); e != nil {
				return e
			}
		}
	}
	return nil
}

// fastRow is what the deck did on a row a fast import left, and the Track values it keeps. base is
// what the imports left in the row, when it is known.
type fastRow struct {
	m         fastMerge
	rating    sql.NullInt64
	bpm, bpmA any
	base      fastBase
	known     bool
}

// fastBaseSchema keeps, for each row an import leaves fast, what that import left in it (fastBase),
// with a check of the fingerprint it left: the row's import in full needs it to tell what the deck
// changed, also after the stick was re-exported while the row was fast and that record is gone
// (#328 review R2). An entry whose check is not the row's fingerprint is not the row's.
const fastBaseSchema = `CREATE TABLE IF NOT EXISTS bridge.BridgeFastBase(trackId INTEGER PRIMARY KEY, record INTEGER NOT NULL, rating INTEGER NOT NULL, quickCues BLOB, loops BLOB, active INTEGER NOT NULL)`

// noteFastBase records b as what the import left in track id's row, which it leaves with fingerprint.
func noteFastBase(ctx context.Context, q *stmtCache, id int64, fingerprint string, b fastBase) error {
	_, e := q.ExecContext(ctx, `INSERT OR REPLACE INTO BridgeFastBase(trackId,record,rating,quickCues,loops,active) VALUES(?,?,?,?,?,?)`,
		id, recordCheck(fingerprint), b.rating, b.cues, b.loops, b.active)
	return e
}

// storedFastBase is what noteFastBase recorded for track id's row with fingerprint.
func storedFastBase(ctx context.Context, q *stmtCache, id int64, fingerprint string) (fastBase, bool, error) {
	var b fastBase
	var record int64
	e := q.QueryRowContext(ctx, `SELECT record,rating,quickCues,loops,active FROM BridgeFastBase WHERE trackId=?`, id).Scan(&record, &b.rating, &b.cues, &b.loops, &b.active)
	if errors.Is(e, sql.ErrNoRows) {
		return fastBase{}, false, nil
	}
	if e != nil || record != recordCheck(fingerprint) {
		return fastBase{}, false, e
	}
	return b, true, nil
}

// fastRowEdits is mergeFast for track id's row, which a fast import left with fingerprint previous,
// for export record t or for an earlier export of the track (re-exported while the row was fast).
// A row a full import has written (BridgeWritten or BridgeAnalysis: one a fast fingerprint only
// sends to be read again) is importKeeps' to judge, not this.
//
// What the imports left in the row is the base: BridgeFastBase's entry for previous, or for a row
// of t that an earlier build left fast without one, what its fast import computed from t. Each
// field the deck did not change takes t's (perf's); each one it changed stays (#328 review R2,
// the maintainer's rule).
//
// With no base (a row an earlier build left for an earlier export, or its entry lost) nothing the
// deck can change is overwritten: its rating, and in a row Engine loaded its grid with the Track's
// BPM, its cues and loops; one log line names the track (#328 review R1). A row Engine never
// loaded has nothing of the deck's but its rating.
func fastRowEdits(ctx context.Context, q *stmtCache, id int64, previous string, t rekordbox.Track, perf Performance) (fastRow, error) {
	var r fastRow
	var row deckRow
	var imported bool
	e := q.QueryRowContext(ctx, `SELECT p.beatData,p.quickCues,p.loops,p.activeOnLoadLoops,p.thirdPartySourceId,COALESCE(t.isBeatGridLocked,0),t.rating,t.bpm,t.bpmAnalyzed,
		EXISTS(SELECT 1 FROM BridgeWritten WHERE trackId=p.trackId) OR EXISTS(SELECT 1 FROM BridgeAnalysis WHERE trackId=p.trackId)
		FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE p.trackId=?`, id).Scan(
		&row.beats, &row.cues, &row.loops, &row.active, &row.mark, &row.locked, &row.rating, &r.bpm, &r.bpmA, &imported)
	if e != nil || imported {
		if errors.Is(e, sql.ErrNoRows) {
			e = nil
		}
		return fastRow{}, e
	}
	r.rating = row.rating
	if r.base, r.known, e = storedFastBase(ctx, q, id, previous); e != nil {
		return fastRow{}, e
	}
	if !r.known && previous == "fast:"+t.RecordHash {
		r.base, r.known = fastBaseOf(t), true
	}
	if r.known {
		r.m = mergeFast(row, r.base, perf)
		return r, nil
	}
	r.m.rating = row.rating.Valid
	if !row.mark.Valid || row.mark.Int64 == 0 {
		r.m.cues, r.m.loops = row.cues, row.loops
		if row.active.Valid {
			r.m.active = row.active.Int64
		}
		if row.beats != nil && (row.locked || !engineOwnGrid(row.beats)) {
			r.m.beats, r.m.grid = row.beats, true // Engine's own analysis is not the deck's: rekordbox's grid
		}
	}
	if r.m.deck() || row.rating.Valid && row.rating.Int64 != int64(t.Rating) {
		log.Printf("rb-bridge: track %d changed in the export before its row was read in full: what the deck has of it stays", id)
	}
	return r, nil
}
