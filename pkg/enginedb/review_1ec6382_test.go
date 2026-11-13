package enginedb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
)

// heldBySelf reports how this process holds path: "ro", "rw" or "" (not open).
func heldBySelf(t *testing.T, path string) string {
	t.Helper()
	want, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	fds, _ := os.ReadDir("/proc/self/fd")
	mode := ""
	for _, fd := range fds {
		st, err := os.Stat(filepath.Join("/proc/self/fd", fd.Name()))
		if err != nil || !os.SameFile(st, want) {
			continue
		}
		info, err := os.ReadFile(filepath.Join("/proc/self/fdinfo", fd.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(info), "\n") {
			if v, ok := strings.CutPrefix(line, "flags:"); ok {
				flags, _ := strconv.ParseUint(strings.TrimSpace(v), 8, 64)
				if flags&syscall.O_ACCMODE != syscall.O_RDONLY {
					return "rw"
				}
				mode = "ro"
			}
		}
	}
	return mode
}

// Review of 1ec6382: Relocate opened m.db for writing whenever Engine did not have the drive
// open when it started, and wrote after scanning the whole drive without looking again. Engine
// may open the library during that scan; the search now reads m.db read-only and the write
// waits for a second look.
func TestRelocateLooksAgainBeforeWriting(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	writeAudio(t, root, "New/Exact.mp3", 100)
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(2,'../Old/Exact.mp3','Exact.mp3',100)`); err != nil {
		t.Fatal(err)
	}
	db.Close() // only Relocate holds m.db in this process
	dbDir := filepath.Join(root, "Engine Library", "Database2")
	mdb := filepath.Join(dbDir, "m.db")
	held := ""
	relocateScanned = func() {
		held = heldBySelf(t, mdb)
		holdOpen(t, mdb) // Engine opens the drive while it is scanned
	}
	defer func() { relocateScanned = nil }()
	res, err := Relocate(context.Background(), RelocateOptions{Root: root})
	if held != "ro" {
		t.Errorf("m.db was held %q while the drive was scanned, want read-only", held)
	}
	if err != nil || res.Applied || !res.Queued || len(res.Moves) != 1 {
		t.Fatalf("Engine opened the library during the scan: %+v %v; want the move queued, not written", res, err)
	}
	check, err := sql.Open("sqlite", mdb+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var path string
	if err := check.QueryRow(`SELECT path FROM Track WHERE id=2`).Scan(&path); err != nil || path != "../Old/Exact.mp3" {
		t.Fatalf("m.db written while Engine had it open: %q %v", path, err)
	}
	if m, _ := filepath.Glob(filepath.Join(dbDir, "m.db.bak*")); len(m) != 0 {
		t.Fatalf("backup written beside an open library: %v", m)
	}
}

// Review of 1ec6382: SetQuickCue stored any position it was given; a negative, NaN or infinite
// one became a Hot Cue Engine cannot place.
func TestSetQuickCueRejectsImpossiblePositions(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE PerformanceData (trackId INTEGER PRIMARY KEY, trackData BLOB, quickCues BLOB)`); err != nil {
		t.Fatal(err)
	}
	var cues bytes.Buffer
	binary.Write(&cues, binary.BigEndian, uint64(8))
	for i := 0; i < 8; i++ {
		cues.WriteByte(0)
		binary.Write(&cues, binary.BigEndian, -1.0)
		cues.Write([]byte{0, 0, 0, 0})
	}
	blob, err := QCompress(cues.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO PerformanceData(trackId,quickCues) VALUES(1,?)`, blob); err != nil {
		t.Fatal(err)
	}
	stored := func() []byte {
		var b []byte
		if err := db.QueryRow(`SELECT quickCues FROM PerformanceData WHERE trackId=1`).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, pos := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		p := pos
		if err := SetQuickCue(context.Background(), db, 1, 0, "x", "#FF0000", &p); err == nil {
			t.Errorf("position %v accepted", pos)
		}
		if !bytes.Equal(stored(), blob) {
			t.Fatalf("position %v was written", pos)
		}
	}
	ok := 1500.0
	if err := SetQuickCue(context.Background(), db, 1, 0, "x", "#FF0000", &ok); err != nil {
		t.Fatalf("a valid position: %v", err)
	}
	zero := 0.0
	if err := SetQuickCue(context.Background(), db, 1, 1, "start", "#00FF00", &zero); err != nil {
		t.Fatalf("a cue at the very start: %v", err)
	}
}
