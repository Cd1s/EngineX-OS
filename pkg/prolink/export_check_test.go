package prolink_test

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/audioheader"
	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestEngineExportRealStructureFixturesAndStrictCheck(t *testing.T) {
	streamDir := t.TempDir()
	oldStreams := prolink.StreamDirs
	prolink.StreamDirs = []string{streamDir}
	t.Cleanup(func() { prolink.StreamDirs = oldStreams })

	// Copy real LAME MP3 fixture into both the Engine library and StreamDirs
	lameFixture, err := os.ReadFile(filepath.Join("..", "..", "internal", "audioheader", "testdata", "01-lame-cbr-info-44100.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(streamDir, "Bicep - Apricots.mp3"), lameFixture, 0o644); err != nil {
		t.Fatal(err)
	}

	_, root, db := testlibrary.Create(t)
	testfixture.Write(t, root, "Music/opener.wav", testfixture.WAV(false))
	testfixture.Write(t, root, "Music/peak_lame.mp3", lameFixture)
	testfixture.Write(t, root, "Music/closer_東京.flac", []byte("fLaC\x00\x00\x00\x22"))

	var cover bytes.Buffer
	if err := jpeg.Encode(&cover, image.NewRGBA(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatal(err)
	}
	bridgeExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(1,'sharedart',?)`, cover.Bytes())

	// Tracks 1 & 2 share albumArtId=1; Track 3 references non-existent albumArtId=99 (must be omitted, never dangling!)
	bridgeExec(t, db, `INSERT INTO Track(id,title,artist,path,filename,length,bpm,bpmAnalyzed,key,albumArtId) VALUES
		(1,'Opener','Ana','../Music/opener.wav','opener.wav',4,120,120,19,1),
		(2,'Peak LAME','Ana','../Music/peak_lame.mp3','peak_lame.mp3',4,128,128,23,1),
		(3,'東京 ナイト 🎛️','Kenji','../Music/closer_東京.flac','closer_東京.flac',4,124,124,0,99)`)

	beat, quick, loops, overview := bridgeEnginePerformance(t)
	bridgeExec(t, db, `UPDATE PerformanceData SET beatData=?,quickCues=?,loops=?,overviewWaveFormData=? WHERE trackId=1`, beat, quick, loops, overview)
	bridgeExec(t, db, `UPDATE PerformanceData SET beatData=?,quickCues=?,loops=?,overviewWaveFormData=? WHERE trackId=2`, beat, quick, loops, overview)

	bridgeExec(t, db, `INSERT INTO Playlist(id,title,parentListId) VALUES(10,'Club Folder',0),(11,'Main Room',10)`)
	bridgeExec(t, db, `INSERT INTO PlaylistEntity(id,listId,trackId,nextEntityId) VALUES(1,11,2,2),(2,11,1,3),(3,11,3,0)`)

	if err := prolink.SyncEngineToPioneerExport(root); err != nil {
		t.Fatalf("SyncEngineToPioneerExport: %v", err)
	}
	bridgeWaitExportComplete(t, root)

	rep, err := prolink.CheckExportDir(root)
	if err != nil {
		t.Fatalf("CheckExportDir failed: %v", err)
	}
	if len(rep.Tracks) != 4 || rep.ArtworksCount != 1 || rep.ANLZChecked != 4 {
		t.Fatalf("unexpected export report: %+v", rep)
	}
	if rep.Tracks[2].ArtworkPath != "" {
		t.Fatalf("missing AlbumArt 99 leaked dangling path %q", rep.Tracks[2].ArtworkPath)
	}

	// Verify PVBR frame count in ANLZ0000.DAT of Track 2 (MP3) matches audioheader.CountMP3AudioFrames * 1152!
	wantFrames, wantSPF, _, err := audioheader.CountMP3AudioFrames(bytes.NewReader(lameFixture), int64(len(lameFixture)))
	if err != nil || wantFrames == 0 {
		t.Fatalf("CountMP3AudioFrames: %d %v", wantFrames, err)
	}
	dat2, err := os.ReadFile(filepath.Join(root, strings.TrimPrefix(rep.Tracks[1].AnalysisPath, "/")))
	if err != nil {
		t.Fatal(err)
	}
	pvbrAt := bytes.Index(dat2, []byte("PVBR"))
	if pvbrAt < 0 {
		t.Fatal("missing PVBR in exported MP3 DAT")
	}
	gotPVBRSamples := binary.BigEndian.Uint32(dat2[pvbrAt+12+1604:])
	if gotPVBRSamples != wantFrames*wantSPF {
		t.Fatalf("PVBR samples=%d, want %d (%d * %d)", gotPVBRSamples, wantFrames*wantSPF, wantFrames, wantSPF)
	}

	// Also verify PWAV (400 cols) + PWV2 (100 cols) are present in Track 1's .DAT
	dat1Parsed := bridgeReadAnalysis(t, root, rep.Tracks[0].AnalysisPath)
	if len(dat1Parsed.Waveforms) != 2 || len(dat1Parsed.Waveforms[0].Data) != 400 || len(dat1Parsed.Waveforms[1].Data) != 100 {
		t.Fatalf("expected PWAV(400)+PWV2(100) in DAT, got %+v", dat1Parsed.Waveforms)
	}

	// Verify CDJ NFS client can mount and stage the validated export without error
	nfs := &prolink.NFSServer{Root: root, StreamDirs: prolink.StreamDirs}
	if err := nfs.Listen("127.63.0.9:0"); err != nil {
		t.Fatal(err)
	}
	defer nfs.Close()
	staged := t.TempDir()
	client := &prolink.NFSClient{IP: net.IPv4(127, 63, 0, 9), Portmap: nfs.PortmapPort()}
	if count, err := prolink.Stage(t.Context(), client, "/C/", staged, 1<<26); err != nil || count < 10 {
		t.Fatalf("NFS stage of validated export failed: count=%d err=%v", count, err)
	}
}

func TestIssue178RoundTripRekordboxToEngineToPioneerMP3AndWAV(t *testing.T) {
	bridgeNoStreams(t)
	_, root, db := testlibrary.Create(t)

	fixtureDir := filepath.Join("..", "..", "internal", "audioheader", "testdata")
	cases := []struct {
		id         int
		rel        string
		srcFixture string // empty = WAV
		sr         int
		wantOffset int
	}{
		{1, "Music/lame44k.mp3", "01-lame-cbr-info-44100.mp3", 44100, 2257},
		{2, "Music/xing48k.mp3", "04-xing-only-48000.mp3", 48000, 1152},
		{3, "Music/hdrless44k.mp3", "05-headerless-lame-44100.mp3", 44100, 0},
		{4, "Music/plain48k.wav", "", 48000, 0},
	}

	origAnalysis := &rekordbox.Analysis{
		Beats: []rekordbox.Beat{
			{Number: 1, BPM: 120.0, TimeMS: 60},
			{Number: 2, BPM: 120.0, TimeMS: 560},
			{Number: 3, BPM: 120.0, TimeMS: 1060},
			{Number: 4, BPM: 120.0, TimeMS: 1560},
		},
		Cues: []rekordbox.Cue{
			{HotCue: 1, TimeMS: 560, Name: "Drop", Color: [4]byte{255, 230, 40, 40}},
			{HotCue: 2, TimeMS: 560, EndMS: 1560, Loop: true, Name: "2Beat", Color: [4]byte{255, 40, 226, 20}},
			{HotCue: 0, TimeMS: 310}, // Memory cue
		},
	}

	for _, tc := range cases {
		var audioBytes []byte
		if tc.srcFixture == "" {
			audioBytes = testfixture.WAV(false)
		} else {
			var err error
			audioBytes, err = os.ReadFile(filepath.Join(fixtureDir, tc.srcFixture))
			if err != nil {
				t.Fatal(err)
			}
		}
		testfixture.Write(t, root, tc.rel, audioBytes)

		hdr, err := compat.AudioHeader(filepath.Join(root, tc.rel))
		if err != nil {
			t.Fatalf("%s AudioHeader: %v", tc.rel, err)
		}
		if hdr.StartOffsetSamples != tc.wantOffset {
			t.Fatalf("%s StartOffsetSamples=%d, want %d", tc.rel, hdr.StartOffsetSamples, tc.wantOffset)
		}

		// Step 1 (Forward #178: Rekordbox -> Engine m.db via enginedb.EncodePerformance)
		rbTrack := rekordbox.Track{
			ID:                 uint32(tc.id),
			Path:               "/" + tc.rel,
			SampleRate:         tc.sr,
			Duration:           4.0,
			BPM:                120.0,
			StartOffsetSamples: hdr.StartOffsetSamples,
		}
		perf, err := enginedb.EncodePerformance(rbTrack, origAnalysis)
		if err != nil {
			t.Fatalf("%s EncodePerformance: %v", tc.rel, err)
		}

		bridgeExec(t, db, `INSERT INTO Track(id,title,artist,path,filename,length,bpm,bpmAnalyzed) VALUES(?,?, 'Artist',?,?,4,120,120)`,
			tc.id, filepath.Base(tc.rel), "../"+tc.rel, filepath.Base(tc.rel))
		bridgeExec(t, db, `UPDATE PerformanceData SET beatData=?,quickCues=?,loops=? WHERE trackId=?`,
			perf.Beats, perf.QuickCues, perf.Loops, tc.id)
	}

	// Step 2 (Reverse #63/#178: Engine m.db -> Pioneer export.pdb + ANLZ)
	if err := prolink.SyncEngineToPioneerExport(root); err != nil {
		t.Fatalf("SyncEngineToPioneerExport: %v", err)
	}
	bridgeWaitExportComplete(t, root)

	rep, err := prolink.CheckExportDir(root)
	if err != nil {
		t.Fatalf("CheckExportDir: %v", err)
	}

	// Step 3: Assert every beat, Hot Cue, Loop, and Memory Cue round-tripped to <= 1 ms of origAnalysis!
	for i, tc := range cases {
		tr := rep.Tracks[i]
		dat := bridgeReadAnalysis(t, root, tr.AnalysisPath)
		ext := bridgeReadAnalysis(t, root, strings.TrimSuffix(tr.AnalysisPath, "DAT")+"EXT")
		dat.Merge(ext)

		// Find Beat 1..4 corresponding to 60, 560, 1060, 1560 ms
		for _, wantBeat := range origAnalysis.Beats {
			matched := false
			for _, gotBeat := range dat.Beats {
				if math.Abs(float64(gotBeat.TimeMS)-float64(wantBeat.TimeMS)) <= 1.0 &&
					gotBeat.Number == wantBeat.Number &&
					math.Abs(gotBeat.BPM-wantBeat.BPM) < 0.05 {
					matched = true
					break
				}
			}
			if !matched {
				t.Fatalf("%s (offset=%d): beat %+v missing or shifted in exported beats %+v",
					tc.rel, tc.wantOffset, wantBeat, dat.Beats)
			}
		}

		for _, wantCue := range origAnalysis.Cues {
			matched := false
			for _, gotCue := range dat.Cues {
				if gotCue.HotCue == wantCue.HotCue && gotCue.Loop == wantCue.Loop {
					if math.Abs(float64(gotCue.TimeMS)-float64(wantCue.TimeMS)) <= 1.0 &&
						(!wantCue.Loop || math.Abs(float64(gotCue.EndMS)-float64(wantCue.EndMS)) <= 1.0) {
						matched = true
						break
					}
				}
			}
			if !matched {
				t.Fatalf("%s (offset=%d): cue %+v shifted in exported cues %+v",
					tc.rel, tc.wantOffset, wantCue, dat.Cues)
			}
		}
	}
}
