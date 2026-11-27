package dbhealer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
)

func isCaseInsensitiveFS(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, ".CaseProbe")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	_, err := os.Lstat(filepath.Join(dir, ".caseprobe"))
	return err == nil
}

func isFSUnsupportedNameErr(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EILSEQ) || errors.Is(err, syscall.ENAMETOOLONG)
}

func writeDistinctTrackFile(t *testing.T, root, rel, payload string) {
	t.Helper()
	p := filepath.Join(root, "Engine Library", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertTrackOpensExactBytes(t *testing.T, db *sql.DB, root string, id int, wantPath, wantName, wantPayload string, wantEdit int64) {
	t.Helper()
	var gotPath, gotName string
	var gotEdit int64
	if err := db.QueryRow(`SELECT path, filename, lastEditTime FROM Track WHERE id=?`, id).Scan(&gotPath, &gotName, &gotEdit); err != nil {
		t.Fatalf("id=%d query: %v", id, err)
	}
	if gotPath != wantPath || gotName != wantName {
		t.Errorf("id=%d got (%q, %q), want (%q, %q)", id, gotPath, gotName, wantPath, wantName)
	}
	if gotEdit != wantEdit {
		t.Errorf("id=%d lastEditTime=%d, want preserved %d", id, gotEdit, wantEdit)
	}
	full := filepath.Join(root, "Engine Library", filepath.FromSlash(gotPath))
	gotBytes, err := os.ReadFile(full)
	if err != nil {
		t.Errorf("id=%d open %q failed: %v", id, gotPath, err)
		return
	}
	if string(gotBytes) != wantPayload {
		t.Errorf("id=%d opened bytes %q, want %q", id, string(gotBytes), wantPayload)
	}
}

// TestS18HealsMacAndExfatPathMismatches covers FAT32/exFAT/host-compatible
// on-disk fixtures (no trailing spaces or literal backslashes on disk) and
// verifies every healed track opens its exact distinct audio bytes.
func TestS18HealsMacAndExfatPathMismatches(t *testing.T) {
	_, root, db := testlibrary.Create(t)

	cases := []struct {
		id       int
		dbPath   string
		diskPath string
		wantPath string
		wantName string
		payload  string
	}{
		{
			id:       1,
			dbPath:   "../Music/Artist /Album /track1.mp3",
			diskPath: "../Music/Artist/Album/track1.mp3",
			wantPath: "../Music/Artist/Album/track1.mp3",
			wantName: "track1.mp3",
			payload:  "audio-01-trailing-space-dirs",
		},
		{
			id:       2,
			dbPath:   "../Music/ LeadingArtist/ LeadingAlbum/track2.mp3",
			diskPath: "../Music/LeadingArtist/LeadingAlbum/track2.mp3",
			wantPath: "../Music/LeadingArtist/LeadingAlbum/track2.mp3",
			wantName: "track2.mp3",
			payload:  "audio-02-leading-space-dirs",
		},
		{
			id:       3,
			dbPath:   "../Music/DotArtist./DotAlbum./track3 .mp3",
			diskPath: "../Music/DotArtist/DotAlbum/track3.mp3",
			wantPath: "../Music/DotArtist/DotAlbum/track3.mp3",
			wantName: "track3.mp3",
			payload:  "audio-03-dot-dir-and-stem-space",
		},
		{
			id:       4,
			dbPath:   "../Music/Cafe\u0301 /nfd-and-space.mp3",
			diskPath: "../Music/Café/nfd-and-space.mp3",
			wantPath: "../Music/Café/nfd-and-space.mp3",
			wantName: "nfd-and-space.mp3",
			payload:  "audio-04-nfd-plus-space",
		},
		{
			id:       5,
			dbPath:   `..\Music\WinSlash\Who’s There?.mp3`,
			diskPath: "../Music/WinSlash/Who's There_.mp3",
			wantPath: "../Music/WinSlash/Who's There_.mp3",
			wantName: "Who's There_.mp3",
			payload:  "audio-05-win-slash-smart-quote-illegal",
		},
		{
			id:       6,
			dbPath:   "../Music/Special/Mix: Part*.aiff",
			diskPath: "../Music/Special/Mix_ Part_.aiff",
			wantPath: "../Music/Special/Mix_ Part_.aiff",
			wantName: "Mix_ Part_.aiff",
			payload:  "audio-06-fat-illegal-underscore",
		},
		{
			id:       7,
			dbPath:   "../Music/Special/Drop?.mp3",
			diskPath: "../Music/Special/Drop.mp3",
			wantPath: "../Music/Special/Drop.mp3",
			wantName: "Drop.mp3",
			payload:  "audio-07-fat-illegal-stripped",
		},
		{
			id:       8,
			dbPath:   "../Music/ExtSpace/Final. mp3",
			diskPath: "../Music/ExtSpace/Final.mp3",
			wantPath: "../Music/ExtSpace/Final.mp3",
			wantName: "Final.mp3",
			payload:  "audio-08-space-after-ext-dot",
		},
		{
			id:       9,
			dbPath:   "../Music/LeadDisk/track9.mp3",
			diskPath: "../Music/LeadDisk/ track9.mp3",
			wantPath: "../Music/LeadDisk/ track9.mp3",
			wantName: " track9.mp3",
			payload:  "audio-09-leading-space-on-disk",
		},
		{
			id:       10,
			dbPath:   "../Music/DotExt/track10.mp3.",
			diskPath: "../Music/DotExt/track10.mp3",
			wantPath: "../Music/DotExt/track10.mp3",
			wantName: "track10.mp3",
			payload:  "audio-10-trailing-dot-ext",
		},
	}

	const origEditTime = int64(1700001234)
	for _, tc := range cases {
		writeDistinctTrackFile(t, root, tc.diskPath, tc.payload)
		if _, err := db.Exec(
			`INSERT INTO Track(id,path,filename,lastEditTime,fileBytes,isAnalyzed) VALUES(?,?,?,?,?,0)`,
			tc.id, tc.dbPath, path.Base(strings.ReplaceAll(tc.dbPath, "\\", "/")), origEditTime, len(tc.payload),
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Track whether the test filesystem already resolves any stored path via
	// kernel normalization (e.g. Linux vfat trailing-dot lookup folding).
	alreadyPlayable := map[int]bool{}
	for _, tc := range cases {
		if strings.ContainsRune(tc.dbPath, '\\') {
			continue
		}
		full := filepath.Join(root, "Engine Library", filepath.FromSlash(tc.dbPath))
		if st, err := os.Lstat(full); err == nil && st.Mode().IsRegular() {
			alreadyPlayable[tc.id] = true
		}
	}

	got, err := HealMountDataOnly(root, 0)
	if err != nil || !got.Healthy {
		t.Fatalf("HealMountDataOnly failed: %+v %v", got, err)
	}
	wantNormalized := len(cases) - len(alreadyPlayable)
	if got.PathsNormalized != wantNormalized {
		t.Fatalf("PathsNormalized=%d, want %d (alreadyPlayable=%v): %+v", got.PathsNormalized, wantNormalized, alreadyPlayable, got)
	}

	check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()

	for _, tc := range cases {
		wantPath := tc.wantPath
		wantName := tc.wantName
		if alreadyPlayable[tc.id] {
			wantPath = tc.dbPath
			wantName = path.Base(strings.ReplaceAll(tc.dbPath, "\\", "/"))
		}
		assertTrackOpensExactBytes(t, check, root, tc.id, wantPath, wantName, tc.payload, origEditTime)
	}
}

// TestS18HostOnlySpellingMismatches covers POSIX/case-sensitive filesystem
// behaviors that cannot be created or are already directly playable on FAT32.
func TestS18HostOnlySpellingMismatches(t *testing.T) {
	t.Run("case mismatch on case-sensitive fs", func(t *testing.T) {
		_, root, db := testlibrary.Create(t)
		const payload = "audio-case-mismatch-unique"
		const origEdit = int64(1700002222)
		writeDistinctTrackFile(t, root, "../Music/Daft Punk/Discovery/One more time.aiff", payload)
		stored := "../Music/Daft Punk/Discovery/One More Time.aiff"
		if _, err := db.Exec(
			`INSERT INTO Track(id,path,filename,lastEditTime,fileBytes,isAnalyzed) VALUES(1,?,?,?,?,0)`,
			stored, "One More Time.aiff", origEdit, len(payload),
		); err != nil {
			t.Fatal(err)
		}
		db.Close()

		ci := isCaseInsensitiveFS(t, filepath.Join(root, "Music"))
		got, err := HealMountDataOnly(root, 0)
		if err != nil || !got.Healthy {
			t.Fatalf("HealMountDataOnly failed: %+v %v", got, err)
		}

		check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer check.Close()

		if ci {
			if got.PathsNormalized != 0 {
				t.Fatalf("case-insensitive FS rewrote already-playable path: %+v", got)
			}
			assertTrackOpensExactBytes(t, check, root, 1, stored, "One More Time.aiff", payload, origEdit)
		} else {
			if got.PathsNormalized != 1 {
				t.Fatalf("PathsNormalized=%d, want 1: %+v", got.PathsNormalized, got)
			}
			assertTrackOpensExactBytes(t, check, root, 1, "../Music/Daft Punk/Discovery/One more time.aiff", "One more time.aiff", payload, origEdit)
		}
	})

	t.Run("trailing space directory on disk", func(t *testing.T) {
		_, root, db := testlibrary.Create(t)
		const payload = "audio-reverse-trailing-space"
		const origEdit = int64(1700003333)
		diskDir := filepath.Join(root, "Music", "ReverseSpace", "Album ")
		if err := os.MkdirAll(diskDir, 0o755); err != nil {
			if isFSUnsupportedNameErr(err) {
				t.Skipf("filesystem rejects trailing-space directory names: %v", err)
			}
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(diskDir, "track9.mp3"), []byte(payload), 0o644); err != nil {
			if isFSUnsupportedNameErr(err) {
				t.Skipf("filesystem rejects trailing-space path: %v", err)
			}
			t.Fatal(err)
		}
		if _, err := db.Exec(
			`INSERT INTO Track(id,path,filename,lastEditTime,fileBytes,isAnalyzed) VALUES(1,'../Music/ReverseSpace/Album/track9.mp3','track9.mp3',?,?,0)`,
			origEdit, len(payload),
		); err != nil {
			t.Fatal(err)
		}
		db.Close()

		got, err := HealMountDataOnly(root, 0)
		if err != nil || !got.Healthy {
			t.Fatalf("HealMountDataOnly failed: %+v %v", got, err)
		}
		if got.PathsNormalized != 1 {
			t.Fatalf("PathsNormalized=%d, want 1: %+v", got.PathsNormalized, got)
		}
		check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer check.Close()
		assertTrackOpensExactBytes(t, check, root, 1, "../Music/ReverseSpace/Album /track9.mp3", "track9.mp3", payload, origEdit)
	})

	t.Run("playable literal backslash filename preserved", func(t *testing.T) {
		_, root, db := testlibrary.Create(t)
		const literalPayload = "literal-backslash-recording"
		const subdirPayload = "subdir-slash-recording-other"
		litPath := filepath.Join(root, "Music", `Set\Intro.mp3`)
		if err := os.MkdirAll(filepath.Dir(litPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(litPath, []byte(literalPayload), 0o644); err != nil {
			if isFSUnsupportedNameErr(err) {
				t.Skipf("filesystem rejects literal backslash filenames: %v", err)
			}
			t.Fatal(err)
		}
		writeDistinctTrackFile(t, root, "../Music/Set/Intro.mp3", subdirPayload)
		if _, err := db.Exec(
			`INSERT INTO Track(id,path,filename,lastEditTime,fileBytes,isAnalyzed) VALUES(1,?,'Set\Intro.mp3',1700004444,?,0)`,
			`../Music/Set\Intro.mp3`, len(literalPayload),
		); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(
			`INSERT INTO Track(id,path,filename,lastEditTime,fileBytes,isAnalyzed) VALUES(2,'../Music/Set/Intro.mp3','Intro.mp3',1700004444,?,0)`,
			len(subdirPayload),
		); err != nil {
			t.Fatal(err)
		}
		db.Close()

		got, err := HealMountDataOnly(root, 0)
		if err != nil || !got.Healthy {
			t.Fatalf("HealMountDataOnly failed: %+v %v", got, err)
		}
		if got.PathsNormalized != 0 {
			t.Fatalf("PathsNormalized=%d, want 0 (playable literal backslash rewritten): %+v", got.PathsNormalized, got)
		}
		check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer check.Close()
		assertTrackOpensExactBytes(t, check, root, 1, `../Music/Set\Intro.mp3`, `Set\Intro.mp3`, literalPayload, 1700004444)
		assertTrackOpensExactBytes(t, check, root, 2, "../Music/Set/Intro.mp3", "Intro.mp3", subdirPayload, 1700004444)
	})
}

func TestS18SafetyGuards(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	write := func(rel, payload string) {
		writeDistinctTrackFile(t, root, rel, payload)
	}

	// 1. Ambiguous sanitized match (both Track_.mp3 and Track.mp3 exist) -> leave untouched.
	ambig := "../Music/Ambig/Track?.mp3"
	write("../Music/Ambig/Track_.mp3", "a1")
	write("../Music/Ambig/Track.mp3", "a2")
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(2,?,'Track?.mp3',2)`, ambig); err != nil {
		t.Fatal(err)
	}

	// 2. Collision with an existing Track.path -> skip without touching SQLite.
	write("../Music/Collide/song.mp3", "owner-song")
	collideSrc := "../Music/Collide /song.mp3"
	collideDst := "../Music/Collide/song.mp3"
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(4,?,'song.mp3',10)`, collideSrc); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(5,?,'song.mp3',10)`, collideDst); err != nil {
		t.Fatal(err)
	}

	// 3. Streaming track with spaces -> never rewrite.
	streamPath := "../Music/Stream /t.mp3"
	write("../Music/Stream/t.mp3", "s")
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,streamingSource) VALUES(6,?,'t.mp3','tidal')`, streamPath); err != nil {
		t.Fatal(err)
	}

	// 4. NFC path in DB with NFD file on disk -> never degrade NFC to NFD (#322).
	nfcDBPath := "../Music/NFCGuard/Café.mp3"
	write("../Music/NFCGuard/Cafe\u0301.mp3", "n")
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(7,?,'Café.mp3',1)`, nfcDBPath); err != nil {
		t.Fatal(err)
	}

	// 5. Healthy suspect-punctuation file (curly quote) -> stays untouched without rewriting.
	curlyPath := "../Music/Don’t Stop/song.mp3"
	write(curlyPath, "curly-audio")
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(8,?,'song.mp3',11)`, curlyPath); err != nil {
		t.Fatal(err)
	}

	// Optional POSIX-only guards (skipped gracefully on FAT32/exFAT where symlinks
	// or trailing-space directory names are rejected).
	wantPaths := map[int]string{
		2: ambig,
		4: collideSrc,
		5: collideDst,
		6: streamPath,
		7: nfcDBPath,
		8: curlyPath,
	}

	exactSpace := "../Music/Exact /keep.mp3"
	if err := os.MkdirAll(filepath.Join(root, "Music", "Exact "), 0o755); err == nil {
		if err := os.WriteFile(filepath.Join(root, "Music", "Exact ", "keep.mp3"), []byte("exact-space"), 0o644); err == nil {
			write("../Music/Exact/keep.mp3", "trimmed-other")
			if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(9,?,'keep.mp3',11)`, exactSpace); err != nil {
				t.Fatal(err)
			}
			wantPaths[9] = exactSpace
		}
	}

	write("../Music/RealDir/real.mp3", "real")
	if err := os.Symlink(filepath.Join(root, "Music", "RealDir"), filepath.Join(root, "Music", "SymDir")); err == nil {
		symPath := "../Music/SymDir /real.mp3"
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(10,?,'real.mp3',4)`, symPath); err != nil {
			t.Fatal(err)
		}
		wantPaths[10] = symPath
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
	for id, want := range wantPaths {
		var p string
		if err := check.QueryRow(`SELECT path FROM Track WHERE id=?`, id).Scan(&p); err != nil {
			t.Fatal(err)
		}
		if p != want {
			t.Errorf("id=%d changed to %q, want %q", id, p, want)
		}
	}
}

// TestS18PathIdentityGuards permanently regresses all Round 1 QA identity,
// ownership, size, ambiguity, backslash, and extension-space findings.
func TestS18PathIdentityGuards(t *testing.T) {
	type scenario struct {
		name     string
		stored   []string
		disk     []string
		want     []string
		bytes    []int
		hostOnly bool
	}
	cases := []scenario{
		{
			name:   "ambiguous exact folder never falls back to sibling",
			stored: []string{"Artist/Set?.mp3"},
			disk:   []string{"Artist/Set_.mp3", "Artist/Set-.mp3", " Artist/Set_.mp3"},
			bytes:  []int{1},
		},
		{
			name:   "nested ambiguity never falls back to outer sibling",
			stored: []string{"Genre/Artist/Set?.mp3"},
			disk:   []string{"Genre/Artist/Set_.mp3", "Genre/Artist/Set-.mp3", " Genre/Artist/Set_.mp3"},
			bytes:  []int{1},
		},
		{
			name:   "absent in exact folder still falls back to unique sibling",
			stored: []string{"Artist/Set?.mp3"},
			disk:   []string{"Artist/Other.mp3", " Artist/Set_.mp3"},
			want:   []string{" Artist/Set_.mp3"},
			bytes:  []int{1},
		},
		{
			name:     "playable literal backslash filename not rewritten",
			stored:   []string{`Set\Intro.mp3`},
			disk:     []string{`Set\Intro.mp3`, "Set/Intro.mp3"},
			bytes:    []int{1},
			hostOnly: true,
		},
		{
			name:   "two missing rows claiming one file both rejected",
			stored: []string{"Mix?.mp3", "Mix*.mp3"},
			disk:   []string{"Mix_.mp3"},
			bytes:  []int{1, 1},
		},
		{
			name:   "existing playable dot-slash owner protects file",
			stored: []string{"./Album/Live.mp3", "Album /Live.mp3"},
			disk:   []string{"Album/Live.mp3"},
			bytes:  []int{1, 1},
		},
		{
			name:   "existing case-variant owner protects file",
			stored: []string{"ALBUM/LIVE.MP3", "Album /Live.mp3"},
			disk:   []string{"Album/Live.mp3"},
			bytes:  []int{1, 1},
		},
		{
			name:   "candidate with known wrong fileBytes rejected",
			stored: []string{"Intro?.mp3"},
			disk:   []string{"Intro_.mp3"},
			bytes:  []int{17},
		},
		{
			name:   "equal size does not settle ambiguous match",
			stored: []string{"Set?.mp3"},
			disk:   []string{"Set_.mp3", "Set-.mp3"},
			bytes:  []int{1},
		},
		{
			name:   "space at start of extension resolved",
			stored: []string{"Final. mp3"},
			disk:   []string{"Final.mp3"},
			want:   []string{"Final.mp3"},
			bytes:  []int{1},
		},
		{
			name:   "space before and after extension dot resolved",
			stored: []string{"Final .  mp3 "},
			disk:   []string{"Final.mp3"},
			want:   []string{"Final.mp3"},
			bytes:  []int{1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath, root, db := testlibrary.Create(t)
			for i, rel := range tc.disk {
				file := filepath.Join(root, "Music", filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					if tc.hostOnly && isFSUnsupportedNameErr(err) {
						t.Skipf("filesystem rejects test path %q: %v", rel, err)
					}
					t.Fatal(err)
				}
				payload := []byte{byte('A' + i)}
				if err := os.WriteFile(file, payload, 0o644); err != nil {
					if tc.hostOnly && isFSUnsupportedNameErr(err) {
						t.Skipf("filesystem rejects test path %q: %v", rel, err)
					}
					t.Fatal(err)
				}
			}
			for i, rel := range tc.stored {
				size := 1
				if i < len(tc.bytes) {
					size = tc.bytes[i]
				}
				if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,?)`,
					i+1, "../Music/"+rel, path.Base(rel), size); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			if _, err := HealMountDataOnly(root, 0); err != nil {
				t.Fatal(err)
			}
			check, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer check.Close()
			want := tc.want
			if want == nil {
				want = tc.stored
			}
			for i, rel := range want {
				var got string
				if err := check.QueryRow(`SELECT path FROM Track WHERE id=?`, i+1).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != "../Music/"+rel {
					t.Errorf("id=%d got %q, want %q", i+1, got, "../Music/"+rel)
				}
			}
		})
	}
}

func TestS18CrossBatchOwnershipConflictRejected(t *testing.T) {
	dbPath, root, db := testlibrary.Create(t)
	writeDistinctTrackFile(t, root, "../Music/Mix_.mp3", "shared-mix")

	// Row 1 and Row 75 sit in different 64-row batches and both claim Mix_.mp3.
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(1,'../Music/Mix?.mp3','Mix?.mp3',10)`); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 74; i++ {
		diskRel := fmt.Sprintf("../Music/Batch/cut%03d.mp3", i)
		dbRel := fmt.Sprintf("../Music/Batch /cut%03d.mp3", i)
		writeDistinctTrackFile(t, root, diskRel, fmt.Sprintf("cut-%03d", i))
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,7)`, i, dbRel, fmt.Sprintf("cut%03d.mp3", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(75,'../Music/Mix*.mp3','Mix*.mp3',10)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	got, err := HealMountDataOnly(root, 0)
	if err != nil || !got.Healthy {
		t.Fatalf("HealMountDataOnly failed: %+v %v", got, err)
	}
	if got.PathsNormalized != 73 {
		t.Fatalf("PathsNormalized=%d, want 73 (only non-conflicting rows): %+v", got.PathsNormalized, got)
	}
	check, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	for id, want := range map[int]string{1: "../Music/Mix?.mp3", 75: "../Music/Mix*.mp3"} {
		var p string
		if err := check.QueryRow(`SELECT path FROM Track WHERE id=?`, id).Scan(&p); err != nil {
			t.Fatal(err)
		}
		if p != want {
			t.Errorf("conflicting id=%d changed to %q, want %q", id, p, want)
		}
	}
}

func TestS18CleanTailCountAndResume(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	dir := filepath.Join(root, "Music")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 65; i++ {
		name := fmt.Sprintf("cut%03d.mp3", i)
		payload := fmt.Sprintf("clean-audio-%03d", i)
		if err := os.WriteFile(filepath.Join(dir, " "+name), []byte(payload), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,?)`,
			i, "../Music/"+name, name, len(payload)); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	prior := normalizeTrackPathBatch
	normalizeTrackPathBatch = cancel
	t.Cleanup(func() { normalizeTrackPathBatch = prior })

	var report Report
	left, err := normalizeTrackPaths(ctx, db, root, &report)
	if err != nil {
		t.Fatal(err)
	}
	var path65 string
	if err := db.QueryRow(`SELECT path FROM Track WHERE id=65`).Scan(&path65); err != nil {
		t.Fatal(err)
	}
	if report.PathsNormalized != 64 || path65 != "../Music/cut065.mp3" {
		t.Fatalf("fixture fixed=%d tail=%q", report.PathsNormalized, path65)
	}
	if left != 1 {
		t.Fatalf("fixed=%d unfinished_id=65 remaining=%d, want 1", report.PathsNormalized, left)
	}
	cursorPath := filepath.Join(root, "Engine Library", "Database2", ".enginex-path-probe-cursor")
	cursorData, err := os.ReadFile(cursorPath)
	if err != nil {
		t.Fatalf("expected cursor file after interrupted pass: %v", err)
	}
	if !strings.Contains(string(cursorData), "last_id=64") {
		t.Fatalf("cursor=%q, want last_id=64", string(cursorData))
	}

	// Second call resumes cleanly past ID 64, repairs ID 65, and deletes the cursor.
	normalizeTrackPathBatch = prior
	var second Report
	left, err = normalizeTrackPaths(context.Background(), db, root, &second)
	if err != nil {
		t.Fatal(err)
	}
	if second.PathsNormalized != 1 || left != 0 {
		t.Fatalf("second pass fixed=%d remaining=%d, want 1/0", second.PathsNormalized, left)
	}
	if err := db.QueryRow(`SELECT path FROM Track WHERE id=65`).Scan(&path65); err != nil {
		t.Fatal(err)
	}
	if path65 != "../Music/ cut065.mp3" {
		t.Fatalf("id=65 path=%q, want ../Music/ cut065.mp3", path65)
	}
	if _, err := os.Lstat(cursorPath); !os.IsNotExist(err) {
		t.Fatalf("cursor file left behind after completion: err=%v", err)
	}
}

func TestS18CleanProbeResumesFromSavedCursor(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	dir := filepath.Join(root, "Music", "Set")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 150; i++ {
		name := fmt.Sprintf("t%03d.mp3", i)
		diskName := name
		if i == 50 || i == 150 {
			diskName = " " + name
		}
		if err := os.WriteFile(filepath.Join(dir, diskName), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,1)`, i, "../Music/Set/"+name, name); err != nil {
			t.Fatal(err)
		}
	}
	var libUUID string
	if err := db.QueryRow(`SELECT COALESCE(uuid, '') FROM Information LIMIT 1`).Scan(&libUUID); err != nil {
		t.Fatal(err)
	}
	cursorPath := filepath.Join(root, "Engine Library", "Database2", ".enginex-path-probe-cursor")
	if err := os.WriteFile(cursorPath, []byte(fmt.Sprintf("v1 last_id=100 uuid=%s\n", strings.TrimSpace(libUUID))), 0o644); err != nil {
		t.Fatal(err)
	}

	// Deadline-bounded mount resumes strictly past last_id=100: skips ID 50,
	// repairs ID 150, completes the tail, and removes the cursor file.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var r1 Report
	rem1, err := normalizeTrackPaths(ctx, db, root, &r1)
	if err != nil {
		t.Fatal(err)
	}
	if r1.PathsNormalized != 1 || rem1 != 0 {
		t.Fatalf("resumed pass fixed=%d remaining=%d, want 1/0", r1.PathsNormalized, rem1)
	}
	var p50, p150 string
	if err := db.QueryRow(`SELECT path FROM Track WHERE id=50`).Scan(&p50); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT path FROM Track WHERE id=150`).Scan(&p150); err != nil {
		t.Fatal(err)
	}
	if p50 != "../Music/Set/t050.mp3" || p150 != "../Music/Set/ t150.mp3" {
		t.Fatalf("after resume p50=%q p150=%q", p50, p150)
	}
	if _, err := os.Lstat(cursorPath); !os.IsNotExist(err) {
		t.Fatalf("cursor file still exists after completing sweep: %v", err)
	}

	// Next sweep starts from ID 1 again and repairs ID 50.
	var r2 Report
	rem2, err := normalizeTrackPaths(context.Background(), db, root, &r2)
	if err != nil {
		t.Fatal(err)
	}
	if r2.PathsNormalized != 1 || rem2 != 0 {
		t.Fatalf("next full sweep fixed=%d remaining=%d, want 1/0", r2.PathsNormalized, rem2)
	}
	if err := db.QueryRow(`SELECT path FROM Track WHERE id=50`).Scan(&p50); err != nil {
		t.Fatal(err)
	}
	if p50 != "../Music/Set/ t050.mp3" {
		t.Fatalf("id=50 path=%q, want ../Music/Set/ t050.mp3", p50)
	}
}

func TestS18PreCancelledContextDoesNoWorkAndReportsRemaining(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	writeDistinctTrackFile(t, root, "../Music/Artist/song.mp3", "hello")
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(1,'../Music/Artist /song.mp3','song.mp3',5)`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var r Report
	rem, err := normalizeTrackPaths(ctx, db, root, &r)
	if err != nil {
		t.Fatal(err)
	}
	if r.PathsNormalized != 0 || rem != 1 {
		t.Fatalf("cancelled ctx fixed=%d remaining=%d, want 0/1", r.PathsNormalized, rem)
	}
}

func TestS18HealthySuspectPunctuationStaysUntouched(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	const origEdit = int64(1700005555)
	paths := []string{
		"../Music/Don’t Stop/song.mp3",
		"../Music/Café/song.mp3",
		"../Music/Normal/song.mp3",
	}
	for i, p := range paths {
		payload := fmt.Sprintf("healthy-audio-%d", i+1)
		writeDistinctTrackFile(t, root, p, payload)
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,lastEditTime,fileBytes,isAnalyzed) VALUES(?,?,?,?,?,0)`, i+1, p, path.Base(p), origEdit, len(payload)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := HealMountDataOnly(root, 0)
	if err != nil || !got.Healthy || got.PathsNormalized != 0 {
		t.Fatalf("healthy mount unexpected heal result: %+v %v", got, err)
	}
	check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	for i, p := range paths {
		assertTrackOpensExactBytes(t, check, root, i+1, p, path.Base(p), fmt.Sprintf("healthy-audio-%d", i+1), origEdit)
	}
}

func TestS18MediaAcceptance(t *testing.T) {
	dbPath, root, db := testlibrary.Create(t)
	cases := [][3]string{
		{"Artist / Album/Cut.mp3", "Artist/Album/Cut.mp3", "first recording"},
		{"Mix?.mp3", "Mix_.mp3", "second recording"},
		{"Final. mp3", "Final.mp3", "third recording"},
	}
	for i, tc := range cases {
		file := filepath.Join(root, "Music", tc[1])
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(tc[2]), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,lastEditTime,fileBytes,isAnalyzed) VALUES(?,?,?,?,?,0)`,
			i+1, "../Music/"+tc[0], filepath.Base(tc[0]), 1700000000, len(tc[2])); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := HealMountDataOnly(root, 0)
	if err != nil || !r.Healthy {
		t.Fatalf("HealMountDataOnly failed: %+v %v", r, err)
	}
	if r.PathsNormalized != len(cases) {
		t.Fatalf("PathsNormalized=%d, want %d", r.PathsNormalized, len(cases))
	}
	check, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	for i, tc := range cases {
		assertTrackOpensExactBytes(t, check, root, i+1, "../Music/"+tc[1], filepath.Base(tc[1]), tc[2], 1700000000)
	}
}
