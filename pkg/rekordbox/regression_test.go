package rekordbox

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
)

func TestAlbumFarOffset(t *testing.T) {
	b := testfixture.PDB()
	const ps = testfixture.PageSize
	// Replace the artist lookup table with an album row of subtype 0x84.
	le.PutUint32(b[28+16:], 3)
	p := b[3*ps : 4*ps]
	le.PutUint32(p[8:], 3)
	row := make([]byte, 300)
	le.PutUint16(row, 0x84)
	le.PutUint32(row[12:], 7)
	le.PutUint16(row[22:], 300)
	name := strings.Repeat("专辑", 100)
	row = append(row, EncodeDeviceSQLString(name)...)
	copy(p[40:], row)
	le.PutUint16(p[30:], uint16(len(row)))
	le.PutUint32(b[2*ps+40+64:], 7)
	l, e := ParsePDB(bytes.NewReader(b), int64(len(b)))
	if e != nil {
		t.Fatal(e)
	}
	if l.Tracks[0].Album != name {
		t.Fatal("far album name lost")
	}
}

func TestPWV4FullAndEmptyPreview(t *testing.T) {
	for _, count := range []uint32{0, 1200} {
		payload := make([]byte, 12+6*count)
		be.PutUint32(payload, 6)
		be.PutUint32(payload[4:], count)
		for i := 12; i < len(payload); i++ {
			payload[i] = byte(i)
		}
		a, e := ParseAnalysis(testfixture.PMAI(testfixture.Section("PWV4", 24, payload)))
		if e != nil {
			t.Fatal(e)
		}
		if len(a.Waveforms) != 1 || len(a.Waveforms[0].Data) != int(6*count) {
			t.Fatal("preview truncated", count, a.Waveforms)
		}
	}
}

func TestCueMergeLoopLengthsAndReverseActivity(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		a := &Analysis{}
		extended := Cue{HotCue: 8, TimeMS: 1000, EndMS: 2000, Loop: true, Name: "Saved loop", Extended: true}
		legacy := extended
		legacy.Extended = false
		legacy.Name = ""
		legacy.Active = true
		if reverse {
			a.mergeCue(extended)
			a.mergeCue(legacy)
		} else {
			a.mergeCue(legacy)
			a.mergeCue(extended)
		}
		if len(a.Cues) != 1 || !a.Cues[0].Active || a.Cues[0].Name != "Saved loop" {
			t.Fatal(a.Cues)
		}
	}
	a := &Analysis{}
	a.mergeCue(Cue{TimeMS: 1000, EndMS: 1500, Loop: true, Name: "short", Extended: true})
	a.mergeCue(Cue{TimeMS: 1000, EndMS: 2000, Loop: true, Name: "long", Extended: true})
	if len(a.Cues) != 2 {
		t.Fatal("distinct memory loops merged", a.Cues)
	}
}

func TestSQLiteEmptyFilenameStaysEmptyAndIsRejected(t *testing.T) {
	for _, folder := range []string{"", "/MUSIC"} {
		p := filepath.Join(t.TempDir(), "exportLibrary.db")
		db, e := sql.Open("sqlite", p)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = db.Exec(`CREATE TABLE content(id INTEGER,title TEXT,folder_path TEXT,file_name TEXT); INSERT INTO content VALUES(1,'Bad',?,'')`, folder); e != nil {
			t.Fatal(e)
		}
		l, e := (OneLibraryAdapter{}).Read(context.Background(), db)
		db.Close()
		if e != nil {
			t.Fatal(e)
		}
		if l.Tracks[0].Path != "" {
			t.Fatal("invented path from missing filename", l.Tracks[0].Path)
		}
		if _, e = ReadSQLite(context.Background(), p, nil); e == nil {
			t.Fatal("accepted track without filename")
		}
	}
}
