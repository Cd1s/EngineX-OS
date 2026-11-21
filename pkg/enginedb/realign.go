package enginedb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"math"
)

// A row Engine has loaded keeps its grid, cues and loops at the next import (sync.go): they
// may hold the deck's edits. They also hold the offset of the import that wrote them, and
// Engine's own shift if it loaded the row while it carried the import mark: a stick imported
// before #178 kept the raw rekordbox positions for good, an M4A the positions without its
// priming (real decks, 2026-11-19). What is still the import's is moved to this import's
// positions here; what the deck changed stays where it is.

// A row is the import's as long as it holds what the import left in it; BridgeWritten has the
// digest of that. Engine's import mark (thirdPartySourceId) said the same until Engine loaded
// the row, but with it Engine shifts the row again (2292 samples for an MP3) whenever it loads
// it with a decoder attached, so no row carries it any more. A row of before this record
// still has the mark, or has lost it to Engine.

// WrittenDigest names the content of a row's grid, cues and loops, however it is compressed.
func WrittenDigest(beats, cues, loops []byte) []byte {
	return writtenDigest(beats, cues, loops)
}

// writtenDigest names the content of a row's grid, cues and loops, however it is compressed.
func writtenDigest(beats, cues, loops []byte) []byte {
	h := sha256.New()
	for _, blob := range [][]byte{beats, cues} {
		raw, err := QUncompress(blob, 8<<20)
		if err != nil {
			raw = blob
		}
		_ = binary.Write(h, big, uint64(len(raw)))
		h.Write(raw)
	}
	h.Write(loops)
	return h.Sum(nil)[:16]
}

// importKeeps reports whether track id's row holds something the import has to keep: Engine
// or the deck has written it since an import did. What it keeps of the import's is moved to
// perf's positions first.
func importKeeps(ctx context.Context, q *stmtCache, id int64, perf Performance) (bool, error) {
	var beats, cues, loops, written []byte
	var mark sql.NullInt64
	var imported, locked bool
	// BridgeAnalysis is optional when the state database is capped. A completed
	// BridgeState row still identifies an imported row whose deck edits must
	// survive a rebuild; a fast row is intentionally excluded until its first
	// full import supplies the source performance.
	e := q.QueryRowContext(ctx, `SELECT p.beatData,p.quickCues,p.loops,p.thirdPartySourceId,COALESCE(t.isBeatGridLocked,0),
		EXISTS(SELECT 1 FROM BridgeState WHERE trackId=p.trackId AND fingerprint NOT LIKE 'fast:%'),(SELECT digest FROM BridgeWritten WHERE trackId=p.trackId)
		FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE p.trackId=?`, id).Scan(&beats, &cues, &loops, &mark, &locked, &imported, &written)
	if e != nil || !imported {
		return false, e
	}
	if written != nil && bytes.Equal(written, writtenDigest(beats, cues, loops)) || written == nil && mark.Valid {
		return false, nil
	}
	if b, c, l, changed := realignKept(beats, cues, loops, perf, locked); changed {
		_, e = q.ExecContext(ctx, `UPDATE PerformanceData SET beatData=?,quickCues=?,loops=? WHERE trackId=?`, b, c, l, id)
	}
	return true, e
}

// recordWritten notes that track id's row is what the import leaves in it, or that it is not.
func recordWritten(ctx context.Context, q *stmtCache, id int64, perf Performance) error {
	var beats, cues, loops []byte
	e := q.QueryRowContext(ctx, `SELECT beatData,quickCues,loops FROM PerformanceData WHERE trackId=?`, id).Scan(&beats, &cues, &loops)
	if e != nil {
		return e
	}
	if digest := writtenDigest(beats, cues, loops); bytes.Equal(digest, writtenDigest(perf.Beats, perf.QuickCues, perf.Loops)) {
		_, e = q.ExecContext(ctx, `INSERT OR REPLACE INTO BridgeWritten(trackId,digest) VALUES(?,?)`, id, digest)
	} else {
		_, e = q.ExecContext(ctx, `DELETE FROM BridgeWritten WHERE trackId=?`, id)
	}
	return e
}

// carryLiveEdits is importKeeps for a build that replaces the live library (merge.go): what
// Engine or the deck wrote to the live row of a track the build has imported again goes
// into the build's row, moved to the build's positions like any row an import keeps.
// Only the tracks Engine has written since the snapshot (temp.touched): the build was made
// from the snapshot's row of every other track and has kept what that row held already.
func carryLiveEdits(ctx context.Context, tx *sql.Tx) error {
	if _, e := tx.ExecContext(ctx, bridgeWrittenSchema); e != nil {
		return e
	}
	digest, n := `NULL`, 0 // the live library's state may be an earlier build's
	if e := tx.QueryRowContext(ctx, `SELECT count(*) FROM livebridge.sqlite_master WHERE name='BridgeWritten'`).Scan(&n); e != nil {
		return e
	}
	if n > 0 {
		digest = `(SELECT digest FROM livebridge.BridgeWritten WHERE trackId=m.trackId)`
	}
	// A completed BridgeState row is enough here: temp.changed contains only rows
	// the import owns. BridgeAnalysis may be absent when the state database hit
	// its size cap, but fast rows must wait for their first full import.
	rows, e := tx.QueryContext(ctx, `SELECT m.trackId,l.beatData,l.quickCues,l.loops,l.activeOnLoadLoops,l.thirdPartySourceId,`+digest+`,
		m.beatData,m.quickCues,m.loops,m.activeOnLoadLoops,COALESCE(t.isBeatGridLocked,0)
		FROM main.PerformanceData m JOIN live.PerformanceData l ON l.trackId=m.trackId JOIN live.Track t ON t.id=m.trackId
		WHERE m.trackId IN (SELECT id FROM temp.touched) AND m.trackId IN (SELECT id FROM temp.changed)
		AND EXISTS (SELECT 1 FROM livebridge.BridgeState s WHERE s.trackId=m.trackId AND s.fingerprint NOT LIKE 'fast:%')`)
	if e != nil {
		return e
	}
	type edit struct {
		id                 int64
		beats, cues, loops []byte
		active             any
		build              []byte
	}
	var edits []edit
	for rows.Next() {
		var id int64
		var lb, lc, ll, written, mb, mc, ml []byte
		var la, ma any
		var mark sql.NullInt64
		var locked bool
		if e = rows.Scan(&id, &lb, &lc, &ll, &la, &mark, &written, &mb, &mc, &ml, &ma, &locked); e != nil {
			rows.Close()
			return e
		}
		untouched := written == nil && mark.Valid || written != nil && bytes.Equal(written, writtenDigest(lb, lc, ll))
		if untouched || lb == nil && lc == nil && ll == nil {
			continue
		}
		b, c, l, _ := realignKept(lb, lc, ll, Performance{Beats: mb, QuickCues: mc, Loops: ml}, locked)
		if lb == nil {
			b = mb
		}
		if lc == nil {
			c = mc
		}
		if ll == nil {
			l, la = ml, ma
		}
		edits = append(edits, edit{id, b, c, l, la, writtenDigest(mb, mc, ml)})
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return e
	}
	for _, x := range edits {
		if _, e = tx.ExecContext(ctx, `UPDATE main.PerformanceData SET beatData=?,quickCues=?,loops=?,activeOnLoadLoops=?,thirdPartySourceId=NULL WHERE trackId=?`,
			x.beats, x.cues, x.loops, x.active, x.id); e != nil {
			return e
		}
		if !bytes.Equal(x.build, writtenDigest(x.beats, x.cues, x.loops)) {
			if _, e = tx.ExecContext(ctx, `DELETE FROM bridge.BridgeWritten WHERE trackId=?`, x.id); e != nil {
				return e
			}
		}
	}
	return nil
}

// realignKept returns the row's three blobs moved to perf's positions. The row's default
// grid says how far off it is: it has to be perf's grid, every marker the same distance
// away, or the row is left alone (rekordbox's grid has changed since, or it is not ours).
func realignKept(beats, cues, loops []byte, perf Performance, locked bool) ([]byte, []byte, []byte, bool) {
	rowRaw, e1 := QUncompress(beats, 8<<20)
	newRaw, e2 := QUncompress(perf.Beats, 8<<20)
	rowDef, rowAdj, ok1 := beatGrids(rowRaw)
	newDef, _, ok2 := beatGrids(newRaw)
	if e1 != nil || e2 != nil || !ok1 || !ok2 || len(rowDef) != len(newDef) || len(rowDef) < 8+24 || !bytes.Equal(rowRaw[:8], newRaw[:8]) {
		return beats, cues, loops, false
	}
	at := func(grid []byte, i int) float64 { return math.Float64frombits(little.Uint64(grid[8+24*i:])) }
	shift := at(rowDef, 0) - at(newDef, 0)
	for i := 0; i < (len(rowDef)-8)/24; i++ {
		if d := at(rowDef, i) - at(newDef, i) - shift; !(math.Abs(d) < 0.01) {
			return beats, cues, loops, false
		}
	}
	if !(math.Abs(shift) >= 0.5 && math.Abs(shift) <= 16384) {
		return beats, cues, loops, false
	}
	changed := false
	// A grid the deck has not adjusted is the import's.
	if !locked && bytes.Equal(rowDef, rowAdj) {
		beats, changed = perf.Beats, true
	}
	// A position is still the import's when it is this import's of the same slot, the same
	// distance away. Slot by slot: a cue the DJ named or added on the deck moves the bytes
	// of every slot after it, and must not keep the import's other cues where they were.
	move := func(row, now []byte, rowAt, nowAt []int, order binary.ByteOrder) bool {
		moved := false
		for i, off := range rowAt {
			p, n := math.Float64frombits(order.Uint64(row[off:])), math.Float64frombits(order.Uint64(now[nowAt[i]:]))
			if p >= 0 && n >= 0 && p != n && math.Abs(p-(n+shift)) < 0.5 {
				order.PutUint64(row[off:], math.Float64bits(n))
				moved = true
			}
		}
		return moved
	}
	if rowCues, e := QUncompress(cues, 64<<10); e == nil {
		newCues, e := QUncompress(perf.QuickCues, 64<<10)
		rowAt, ok1 := cuePositions(rowCues)
		newAt, ok2 := cuePositions(newCues)
		if e == nil && ok1 && ok2 && move(rowCues, newCues, rowAt, newAt, big) {
			if enc, e := QCompress(rowCues); e == nil {
				cues, changed = enc, true
			}
		}
	}
	rowAt, ok1 := loopPositions(loops)
	newAt, ok2 := loopPositions(perf.Loops)
	if ok1 && ok2 && len(rowAt) == len(newAt) {
		out := append([]byte(nil), loops...)
		if move(out, perf.Loops, rowAt, newAt, little) {
			loops, changed = out, true
		}
	}
	return beats, cues, loops, changed
}

// beatGrids returns the default and the adjusted grid of uncompressed beatData, each as its
// marker count and markers.
func beatGrids(raw []byte) (def, adj []byte, ok bool) {
	next := func(from int) ([]byte, int) {
		if from < 0 || len(raw)-from < 8 {
			return nil, -1
		}
		n := big.Uint64(raw[from:])
		if n > uint64(len(raw)-from-8)/24 {
			return nil, -1
		}
		end := from + 8 + 24*int(n)
		return raw[from:end], end
	}
	def, end := next(17)
	adj, end = next(end)
	return def, adj, end > 0
}

// cuePositions returns where the positions are in uncompressed quickCues: the eight hot cues,
// then the main cue and the main cue as imported.
func cuePositions(raw []byte) ([]int, bool) {
	if len(raw) < 8 || big.Uint64(raw) != 8 {
		return nil, false
	}
	at, off := make([]int, 0, 10), 8
	for i := 0; i < 8; i++ {
		if off >= len(raw) || len(raw)-off-1 < int(raw[off])+12 {
			return nil, false
		}
		off += 1 + int(raw[off])
		at = append(at, off)
		off += 12
	}
	if off+17 != len(raw) {
		return nil, false
	}
	return append(at, off, off+9), true
}

// loopPositions returns where each slot's start and end are in a loops blob; one that is
// not set reads -1 there.
func loopPositions(blob []byte) ([]int, bool) {
	if len(blob) < 8 || blob[0] > 8 || !bytes.Equal(blob[1:8], make([]byte, 7)) {
		return nil, false
	}
	var at []int
	off := 8
	for i := 0; i < int(blob[0]); i++ {
		if off >= len(blob) || len(blob)-off-1 < int(blob[off])+22 {
			return nil, false
		}
		off += 1 + int(blob[off])
		at = append(at, off, off+8)
		off += 22
	}
	return at, off == len(blob)
}
