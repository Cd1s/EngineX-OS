package bridge_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/audioheader"
	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// #318 (SC LIVE 4, 5.3.0.4): a stick of 48 kHz MP3s written by ffmpeg (Info frame, "Lavc61.19",
// start_pad 576) was imported 47.0 ms (2257 samples) early; about 23.0 ms (1105) is right.
// Every part of a conversion moves by the one number: grid, hot cues, the main cue and
// loops. An ID3v2 tag in front of the stream moves nothing, and the import moves a
// position by what Pro DJ Link's fill does for the same file (checklist rows 346, 371).
func TestIssue318ImportedMP3KeepsRekordboxPlace(t *testing.T) {
	const sr = 48000
	root := t.TempDir()
	tracks := []struct {
		id      int
		fixture string
		// 0: the number is the measured start_pad + 529 (Info class); otherwise the class's
		// number, which no deck has measured yet, is only held equal to the Link fill's.
		want int
	}{
		{1, "07-lavc-cbr-info-48000-id3.mp3", 1105},
		{2, "08-lavc-cbr-info-48000-noid3.mp3", 1105},
		{3, "09-lavc-vbr-xing-48000-id3.mp3", 0},
		{4, "10-lavc-vbr-xing-48000-noid3.mp3", 0},
	}

	exportPath := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	if err := os.MkdirAll(filepath.Dir(exportPath), 0o755); err != nil {
		t.Fatal(err)
	}
	exp, err := sql.Open("sqlite", exportPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = exp.Exec(`
		CREATE TABLE content(content_id INTEGER PRIMARY KEY, title TEXT, path TEXT, bpmx100 INTEGER, length INTEGER, samplingRate INTEGER, analysisDataFilePath TEXT);
		CREATE TABLE playlist(playlist_id INTEGER PRIMARY KEY, playlist_id_parent INTEGER, sequenceNo INTEGER, name TEXT, attribute INTEGER);
		CREATE TABLE playlist_content(playlist_id INTEGER, content_id INTEGER, sequenceNo INTEGER);
		INSERT INTO playlist VALUES(1, NULL, 1, 'Issue318', 0);
	`); err != nil {
		t.Fatal(err)
	}
	for _, tr := range tracks {
		rel := fmt.Sprintf("MUSIC/%s", tr.fixture)
		anlz := fmt.Sprintf("PIONEER/USBANLZ/P%03d/ANLZ0000.DAT", tr.id)
		copyMP3Fixture(t, root, rel, tr.fixture)
		testfixture.Write(t, root, anlz, makeOffsetANLZ(50))
		if _, err = exp.Exec(`INSERT INTO content VALUES(?,?,?,12000,8,?,?)`, tr.id, rel, "/"+rel, sr, "/"+anlz); err != nil {
			t.Fatal(err)
		}
		if _, err = exp.Exec(`INSERT INTO playlist_content VALUES(1,?,?)`, tr.id, tr.id); err != nil {
			t.Fatal(err)
		}
	}
	if err = exp.Close(); err != nil {
		t.Fatal(err)
	}

	res, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	if err != nil || res.Added != len(tracks) || res.Degraded != 0 {
		t.Fatalf("ConvertAtMount: %+v, err=%v", res, err)
	}

	db := dbOpen(t, root)
	defer db.Close()
	var first decodedTrackPositions
	for _, tr := range tracks {
		info, err := audioheader.OpenMP3(filepath.Join(root, "MUSIC", tr.fixture))
		if err != nil {
			t.Fatal(err)
		}
		off := float64(info.RekordboxOffsetSamples)
		if tr.want != 0 && info.RekordboxOffsetSamples != tr.want {
			t.Fatalf("%s: the Link fill's offset is %d, want %d", tr.fixture, info.RekordboxOffsetSamples, tr.want)
		}
		got := readTrackPositions(t, db, tr.id)
		if got.trackSampleRate != sr {
			t.Fatalf("%s: sample rate %v, want %d", tr.fixture, got.trackSampleRate, sr)
		}
		at := func(ms float64) float64 { return ms*sr/1000 - off }
		for _, c := range []struct {
			name      string
			got, want float64
		}{
			{"first beat", got.beat0, at(50)},
			{"second beat", got.beat1, at(550)},
			{"hot cue B", got.hotB, at(550)},
			{"main cue", got.mainCue, at(550)},
			{"loop start", got.loop0Start, at(1050)},
			{"loop end", got.loop0End, at(2050)},
			{"hot loop in", got.loop2Start, math.Max(0, at(20))},
			{"hot loop out", got.loop2End, at(520)},
		} {
			if math.Abs(c.got-c.want) > 1e-6 {
				t.Errorf("%s: %s at sample %v, want %v (rekordbox position - %v)", tr.fixture, c.name, c.got, c.want, off)
			}
		}
		if tr.id == 1 {
			first = got
		} else if tr.id == 2 && got != first {
			t.Errorf("an ID3v2 tag moved a position:\nwith    %+v\nwithout %+v", first, got)
		}
	}
}
