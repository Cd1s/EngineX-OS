package enginedb

import (
	"context"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
)

func TestRelocateNormalizationPairs(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	cases := []struct {
		name  string
		file  string
		path  string
		title string
	}{
		{name: "latin", file: "Music/Café.mp3", path: "../Old/Cafe\u0301.mp3"},
		{name: "vietnamese", file: "Music/Tiếng Việt.mp3", path: "../Old/Tie\u0302\u0301ng Vie\u0302\u0323t.mp3"},
		{name: "hangul", file: "Music/한글.mp3", path: "../Old/한글.mp3"},
		{name: "kana", file: "Music/がく.mp3", path: "../Old/か\u3099く.mp3"},
	}
	for i, tc := range cases {
		writeAudio(t, root, tc.file, 10+i)
		if !utf8.ValidString(tc.path) {
			t.Fatalf("invalid UTF-8 path for %s", tc.name)
		}
		if norm.NFC.String(tc.file) != tc.file {
			t.Fatalf("fixture file %s is not NFC", tc.file)
		}
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes,title) VALUES(?,?,?,?,?)`, i+1, tc.path, filepath.Base(tc.path), 10+i, tc.name); err != nil {
			t.Fatal(err)
		}
	}

	res, err := Relocate(context.Background(), RelocateOptions{Root: root, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Moves) != len(cases) {
		t.Fatalf("moves=%d, want %d: %+v", len(res.Moves), len(cases), res)
	}
	for _, move := range res.Moves {
		if move.Tier != TierCleaned {
			t.Errorf("track %d tier=%d, want TierCleaned", move.ID, move.Tier)
		}
	}
}
