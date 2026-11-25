package dbhealer

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
)

func TestS18HealsMacAndExfatPathMismatches(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	write := func(rel string) {
		p := filepath.Join(root, "Engine Library", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("audio-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		id       int
		dbPath   string
		diskPath string
		wantPath string
		wantName string
	}{
		{
			id:       1,
			dbPath:   "../Music/Artist /Album /track1.mp3",
			diskPath: "../Music/Artist/Album/track1.mp3",
			wantPath: "../Music/Artist/Album/track1.mp3",
			wantName: "track1.mp3",
		},
		{
			id:       2,
			dbPath:   "../Music/ LeadingArtist/ LeadingAlbum/track2.mp3",
			diskPath: "../Music/LeadingArtist/LeadingAlbum/track2.mp3",
			wantPath: "../Music/LeadingArtist/LeadingAlbum/track2.mp3",
			wantName: "track2.mp3",
		},
		{
			id:       3,
			dbPath:   "../Music/DotArtist./DotAlbum./track3 .mp3",
			diskPath: "../Music/DotArtist/DotAlbum/track3.mp3",
			wantPath: "../Music/DotArtist/DotAlbum/track3.mp3",
			wantName: "track3.mp3",
		},
		{
			id:       4,
			dbPath:   "../Music/Daft Punk/Discovery/One More Time.aiff",
			diskPath: "../Music/Daft Punk/Discovery/One more time.aiff",
			wantPath: "../Music/Daft Punk/Discovery/One more time.aiff",
			wantName: "One more time.aiff",
		},
		{
			id:       5,
			dbPath:   "../Music/Café /nfd-and-space.mp3",
			diskPath: "../Music/Café/nfd-and-space.mp3",
			wantPath: "../Music/Café/nfd-and-space.mp3",
			wantName: "nfd-and-space.mp3",
		},
		{
			id:       6,
			dbPath:   `..\Music\WinSlash\Who’s There?.mp3`,
			diskPath: "../Music/WinSlash/Who's There_.mp3",
			wantPath: "../Music/WinSlash/Who's There_.mp3",
			wantName: "Who's There_.mp3",
		},
		{
			id:       7,
			dbPath:   "../Music/Special/Mix: Part*.aiff",
			diskPath: "../Music/Special/Mix_ Part_.aiff",
			wantPath: "../Music/Special/Mix_ Part_.aiff",
			wantName: "Mix_ Part_.aiff",
		},
		{
			id:       8,
			dbPath:   "../Music/Special/Drop?.mp3",
			diskPath: "../Music/Special/Drop.mp3",
			wantPath: "../Music/Special/Drop.mp3",
			wantName: "Drop.mp3",
		},
		{
			id:       9,
			dbPath:   "../Music/ReverseSpace/Album/track9.mp3",
			diskPath: "../Music/ReverseSpace/Album /track9.mp3",
			wantPath: "../Music/ReverseSpace/Album /track9.mp3",
			wantName: "track9.mp3",
		},
		{
			id:       10,
			dbPath:   "../Music/DotExt/track10.mp3.",
			diskPath: "../Music/DotExt/track10.mp3",
			wantPath: "../Music/DotExt/track10.mp3",
			wantName: "track10.mp3",
		},
	}

	const origEditTime = int64(1700001234)
	for _, tc := range cases {
		write(tc.diskPath)
		if _, err := db.Exec(
			`INSERT INTO Track(id,path,filename,lastEditTime,fileBytes,isAnalyzed) VALUES(?,?,?,?,11,0)`,
			tc.id, tc.dbPath, filepath.Base(tc.dbPath), origEditTime,
		); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	got, err := HealMountDataOnly(root, 0)
	if err != nil || !got.Healthy {
		t.Fatalf("HealMountDataOnly failed: %+v %v", got, err)
	}
	if got.PathsNormalized != len(cases) {
		t.Fatalf("PathsNormalized=%d, want %d: %+v", got.PathsNormalized, len(cases), got)
	}

	check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()

	for _, tc := range cases {
		var gotPath, gotName string
		var gotEdit int64
		if err := check.QueryRow(`SELECT path, filename, lastEditTime FROM Track WHERE id=?`, tc.id).Scan(&gotPath, &gotName, &gotEdit); err != nil {
			t.Fatalf("id=%d query: %v", tc.id, err)
		}
		if gotPath != tc.wantPath || gotName != tc.wantName {
			t.Errorf("id=%d got (%q, %q), want (%q, %q)", tc.id, gotPath, gotName, tc.wantPath, tc.wantName)
		}
		if gotEdit != origEditTime {
			t.Errorf("id=%d lastEditTime=%d, want preserved %d", tc.id, gotEdit, origEditTime)
		}
	}
}

func TestS18SafetyGuards(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	write := func(rel string) {
		p := filepath.Join(root, "Engine Library", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Exact path with trailing space already exists on disk -> do not rewrite.
	exactSpace := "../Music/Exact /keep.mp3"
	write(exactSpace)
	write("../Music/Exact/keep.mp3")
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(1,?,'keep.mp3',1)`, exactSpace); err != nil {
		t.Fatal(err)
	}

	// 2. Ambiguous sanitized match (both Track_.mp3 and Track.mp3 exist) -> leave untouched.
	ambig := "../Music/Ambig/Track?.mp3"
	write("../Music/Ambig/Track_.mp3")
	write("../Music/Ambig/Track.mp3")
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(2,?,'Track?.mp3',1)`, ambig); err != nil {
		t.Fatal(err)
	}

	// 3. Symlink directory or file -> never follow or rewrite.
	write("../Music/RealDir/real.mp3")
	if err := os.Symlink(filepath.Join(root, "Music", "RealDir"), filepath.Join(root, "Music", "SymDir")); err != nil {
		t.Fatal(err)
	}
	symPath := "../Music/SymDir /real.mp3"
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(3,?,'real.mp3',1)`, symPath); err != nil {
		t.Fatal(err)
	}

	// 4. Collision with an existing Track.path -> skip without touching SQLite.
	write("../Music/Collide/song.mp3")
	collideSrc := "../Music/Collide /song.mp3"
	collideDst := "../Music/Collide/song.mp3"
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(4,?,'song.mp3',1)`, collideSrc); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(5,?,'song.mp3',1)`, collideDst); err != nil {
		t.Fatal(err)
	}

	// 5. Streaming track with spaces -> never rewrite.
	streamPath := "../Music/Stream /t.mp3"
	write("../Music/Stream/t.mp3")
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,streamingSource) VALUES(6,?,'t.mp3','tidal')`, streamPath); err != nil {
		t.Fatal(err)
	}

	// 6. NFC path in DB with NFD file on disk -> never degrade NFC to NFD (#322).
	nfcDBPath := "../Music/NFCGuard/Café.mp3"
	write("../Music/NFCGuard/Café.mp3")
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(7,?,'Café.mp3',1)`, nfcDBPath); err != nil {
		t.Fatal(err)
	}

	db.Close()

	got, err := HealMountDataOnly(root, 0)
	if err != nil || !got.Healthy {
		t.Fatalf("HealMountDataOnly failed: %+v %v", got, err)
	}
	if got.PathsNormalized != 0 {
		t.Fatalf("expected 0 normalized paths under safety guards, got %d: %+v", got.PathsNormalized, got)
	}

	check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	for id, want := range map[int]string{
		1: exactSpace,
		2: ambig,
		3: symPath,
		4: collideSrc,
		5: collideDst,
		6: streamPath,
		7: nfcDBPath,
	} {
		var p string
		if err := check.QueryRow(`SELECT path FROM Track WHERE id=?`, id).Scan(&p); err != nil {
			t.Fatal(err)
		}
		if p != want {
			t.Errorf("id=%d changed to %q, want %q", id, p, want)
		}
	}
}
