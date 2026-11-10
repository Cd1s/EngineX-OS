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

func TestSQLiteSkipsOrphanPlaylistEntries(t *testing.T) {
	db, e := sql.Open("sqlite", ":memory:")
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, e = db.Exec(`CREATE TABLE content(id INTEGER,title TEXT,folder_path TEXT,file_name TEXT); INSERT INTO content VALUES(1,'A','/MUSIC','a.wav');
CREATE TABLE playlist(id INTEGER,name TEXT); INSERT INTO playlist VALUES(1,'Live');
CREATE TABLE playlist_content(playlist_id INTEGER,content_id INTEGER,sequenceNo INTEGER); INSERT INTO playlist_content VALUES(1,1,1),(99,1,1)`); e != nil {
		t.Fatal(e)
	}
	l, e := (OneLibraryAdapter{}).Read(context.Background(), db)
	if e != nil || len(l.Playlists) != 1 || len(l.Playlists[0].Tracks) != 1 {
		t.Fatalf("orphan entry not skipped: %+v %v", l, e)
	}
}

func TestAnalysisIgnoresTrailingPadding(t *testing.T) {
	b := append(testfixture.DAT(), make([]byte, 512)...)
	if a, e := ParseAnalysis(b); e != nil || len(a.Beats) == 0 {
		t.Fatalf("padded ANLZ rejected: %v", e)
	}
}

func TestPDBUnanalyzedBPMOverflow(t *testing.T) {
	b := testfixture.PDB()
	const ps = testfixture.PageSize
	// Mutate track 1 tempo to 0xFFFFFFFF (unanalyzed/raw in Rekordbox 5)
	le.PutUint32(b[2*ps+40+56:], 0xffffffff)
	l, err := ParsePDB(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("ParsePDB failed on 0xFFFFFFFF BPM: %v", err)
	}
	if l.Tracks[0].BPM != 0 {
		t.Fatalf("expected 0 BPM for unanalyzed track, got %f", l.Tracks[0].BPM)
	}
}

func TestPDBOrphanPlaylistEntries(t *testing.T) {
	b := testfixture.PDB()
	const ps = testfixture.PageSize
	// Table 4 (type 8, playlist entries) is at page 6 (index 6*ps)
	p := b[6*ps : 7*ps]
	// Add an entry pointing to non-existent track 999
	entryBytes := make([]byte, 12)
	le.PutUint32(entryBytes, 99)      // order
	le.PutUint32(entryBytes[4:], 999) // non-existent track
	le.PutUint32(entryBytes[8:], 12)  // playlist 12
	off := 40 + int(le.Uint16(p[30:]))
	copy(p[off:], entryBytes)
	le.PutUint16(p[30:], uint16(off+len(entryBytes)-40))
	count := int(le.Uint16(p[24:]))
	base := ps - (count/16)*36
	le.PutUint16(p[base-6-2*(count%16):], uint16(off-40))
	flags := le.Uint16(p[base-4:])
	le.PutUint16(p[base-4:], flags|1<<uint(count%16))
	le.PutUint16(p[24:], uint16(count+1))

	l, err := ParsePDB(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("ParsePDB rejected orphan playlist entry: %v", err)
	}
	for _, p := range l.Playlists {
		for _, tid := range p.Tracks {
			if tid == 999 {
				t.Fatalf("orphan track 999 was not filtered from playlist %d", p.ID)
			}
		}
	}
}

func TestValidateResilientDuplicateSiblingPlaylists(t *testing.T) {
	l := &Library{
		Tracks: []Track{
			{ID: 1, Path: "/Contents/test.mp3", BPM: 120.0, Duration: 180},
		},
		Playlists: []Playlist{
			{ID: 1, ParentID: 0, Name: "Favorites", Tracks: []uint32{1}},
			{ID: 2, ParentID: 0, Name: "Favorites", Tracks: []uint32{1}},
		},
	}
	if err := l.Validate(); err != nil {
		t.Fatalf("Validate failed on duplicate sibling playlists: %v", err)
	}
	if l.Playlists[0].Name == l.Playlists[1].Name {
		t.Fatalf("duplicate sibling playlist was not disambiguated: %q vs %q", l.Playlists[0].Name, l.Playlists[1].Name)
	}
}

func TestValidateResilientDanglingParent(t *testing.T) {
	l := &Library{
		Tracks: []Track{
			{ID: 1, Path: "/Contents/test.mp3", BPM: 120.0, Duration: 180},
		},
		Playlists: []Playlist{
			{ID: 10, ParentID: 999, Name: "Subtree Parent", Folder: true},
			{ID: 20, ParentID: 10, Name: "Nested Folder", Folder: true},
			{ID: 30, ParentID: 20, Name: "Leaf Playlist", Tracks: []uint32{1}},
		},
	}
	if err := l.Validate(); err != nil {
		t.Fatalf("Validate failed on dangling parent: %v", err)
	}
	byID := map[uint32]Playlist{}
	for _, p := range l.Playlists {
		byID[p.ID] = p
	}
	if byID[10].ParentID != 0 {
		t.Fatalf("expected root of orphan subtree (10) to be reset to 0, got %d", byID[10].ParentID)
	}
	if byID[20].ParentID != 10 {
		t.Fatalf("expected nested folder (20) to retain parent 10, got %d", byID[20].ParentID)
	}
	if byID[30].ParentID != 20 {
		t.Fatalf("expected leaf playlist (30) to retain parent 20, got %d", byID[30].ParentID)
	}
}
