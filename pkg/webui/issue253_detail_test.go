package webui

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
)

// #253: a state row of 5.3.0.6 names the track's ANLZ files on the stick instead of holding a copy
// of the waveform; the display reads the PWV7 (else the PWV5) from there, as the resolver finds the
// file, and keeps the library, path and duration guards.
func TestBridgeDetailsFollowTheReferenceToTheANLZFiles(t *testing.T) {
	root := t.TempDir()
	wave := make([]byte, 12+3*600)
	binary.BigEndian.PutUint32(wave, 3)
	binary.BigEndian.PutUint32(wave[4:], 600)
	copy(wave[12:], bytes.Repeat([]byte{100, 50, 25}, 600))
	testfixture.Write(t, root, "PIONEER/USBANLZ/P001/0000ABCD/ANLZ0000.2EX", testfixture.PMAI(testfixture.Section("PWV7", 24, wave)))
	color := make([]byte, 12+2*600)
	binary.BigEndian.PutUint32(color, 2)
	binary.BigEndian.PutUint32(color[4:], 600)
	testfixture.Write(t, root, "PIONEER/USBANLZ/P002/0000ABCE/ANLZ0000.EXT", testfixture.PMAI(testfixture.Section("PWV5", 24, color)))
	// An invalid PWV7 (one byte an entry) does not displace the .EXT's valid PWV5.
	bad := make([]byte, 12+600)
	binary.BigEndian.PutUint32(bad, 1)
	binary.BigEndian.PutUint32(bad[4:], 600)
	testfixture.Write(t, root, "PIONEER/USBANLZ/P003/0000ABCF/ANLZ0000.2EX", testfixture.PMAI(testfixture.Section("PWV7", 24, bad)))
	testfixture.Write(t, root, "PIONEER/USBANLZ/P003/0000ABCF/ANLZ0000.EXT", testfixture.PMAI(testfixture.Section("PWV5", 24, color)))
	dir := filepath.Join(root, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, ".rb-bridge-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE BridgeAnalysis(trackId INTEGER PRIMARY KEY, sourceJSON BLOB); CREATE TABLE BridgeState(path TEXT PRIMARY KEY, trackId INTEGER)`); err != nil {
		t.Fatal(err)
	}
	ref := func(id uint32, files ...string) []byte {
		b := binary.LittleEndian.AppendUint32([]byte("RBA2"), id)
		for _, f := range files {
			b = binary.LittleEndian.AppendUint16(b, uint16(len(f)))
			b = append(b, f...)
		}
		return b
	}
	if err = os.Symlink(filepath.Join(root, "PIONEER/USBANLZ/P001"), filepath.Join(root, "PIONEER/USBANLZ/LINK")); err != nil {
		t.Fatal(err)
	}
	rows := map[int][]byte{
		1: ref(1, "PIONEER/USBANLZ/P001/0000ABCD/ANLZ0000.2EX", "PIONEER/USBANLZ/P001/0000ABCD/ANLZ0000.EXT"),
		2: ref(2, "pioneer/usbanlz/p001/0000abcd/anlz0000.2ex"), // the export's spelling, not the volume's
		3: ref(3, "PIONEER/USBANLZ/P002/0000ABCE/ANLZ0000.2EX", "PIONEER/USBANLZ/P002/0000ABCE/ANLZ0000.EXT"),
		4: ref(4, "../../PIONEER/USBANLZ/P001/0000ABCD/ANLZ0000.2EX"),
		5: ref(5, "PIONEER/USBANLZ/LINK/0000ABCD/ANLZ0000.2EX"),
		6: ref(6, "PIONEER/USBANLZ/P009/MISSING/ANLZ0000.2EX"),
		7: ref(7),
		8: ref(8, "PIONEER/USBANLZ/P003/0000ABCF/ANLZ0000.2EX", "PIONEER/USBANLZ/P003/0000ABCF/ANLZ0000.EXT"),
	}
	for id, b := range rows {
		if _, err = db.Exec(`INSERT INTO BridgeAnalysis VALUES(?,?)`, id, b); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO BridgeState VALUES(?,?)`, "../Contents/"+string(rune('a'+id))+".mp3", id); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	file := filepath.Join(dir, "m.db")
	for id, bands := range map[int]int{1: 1800, 2: 1800, 3: 1800, 8: 1800} {
		w := bridgeDisplayWave(ctx, file, "../Contents/"+string(rune('a'+id))+".mp3", int64(id), 44100, 4000)
		if w == nil || len(w.Bands) != bands {
			t.Fatalf("track %d: detail not read from its ANLZ file: %+v", id, w)
		}
	}
	if w := bridgeDisplayWave(ctx, file, "../Contents/b.mp3", 1, 44100, 4000); !bytes.Equal(w.Bands[:3], []byte{100, 50, 25}) {
		t.Fatalf("PWV7 bands %v", w.Bands[:3])
	}
	for _, id := range []int{4, 5, 6, 7} {
		if bridgeDisplayWave(ctx, file, "../Contents/"+string(rune('a'+id))+".mp3", int64(id), 44100, 4000) != nil {
			t.Fatalf("track %d: a reference outside the volume, through a symlink or to nothing was read", id)
		}
	}
	for _, tc := range []struct {
		file, rel string
		duration  float64
	}{{filepath.Join(dir, "stm.db"), "../Contents/b.mp3", 4000}, {file, "../Contents/c.mp3", 4000}, {file, "../Contents/b.mp3", 4500}} {
		if bridgeDisplayWave(ctx, tc.file, tc.rel, 1, 44100, tc.duration) != nil {
			t.Fatalf("guard lost: %+v", tc)
		}
	}
}
