package prolink

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestStageCountsExistingFilesTowardBudget(t *testing.T) {
	usb := t.TempDir()
	testfixture.Export(t, usb)
	srv := &NFSServer{Root: usb}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c := &NFSClient{IP: net.IPv4(127, 0, 0, 1), Portmap: srv.PortmapPort()}
	dst := t.TempDir()

	// Pre-populate dst with 4 KiB of staged content exceeding a 2 KiB budget.
	pre := filepath.Join(dst, "Music", "preexisting.bin")
	if err := os.MkdirAll(filepath.Dir(pre), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pre, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	copied, err := Stage(context.Background(), c, "/C/", dst, 2048)
	if err != nil {
		t.Fatalf("Stage failed: %v", err)
	}
	if copied != 0 {
		t.Fatalf("Stage copied %d new files despite existing dst files already exceeding max budget", copied)
	}
}

func TestAnnounceLoopUpdatesFirstFlagAndRechecksPDBBeforeClaim(t *testing.T) {
	now := time.Now()
	r := &exportRun{
		probed:  true,
		mounted: true,
		ip:      net.IPv4(127, 63, 0, 1),
		mac:     net.HardwareAddr{2, 0, 0, 0, 0, 1},
		peers: map[string]exportPeer{
			"expired": {number: 4, seen: now.Add(-11 * time.Second)},
		},
	}
	r.chooseLocked(now)
	if !r.first || r.announcement()[0x25] != 2 {
		t.Fatalf("after stale peer expired, r.first=%v b[0x25]=%d, want true and 2", r.first, r.announcement()[0x25])
	}
	r.peers["active"] = exportPeer{number: 1, seen: now}
	r.chooseLocked(now)
	if r.first || r.announcement()[0x25] != 1 {
		t.Fatalf("after peer joined, r.first=%v b[0x25]=%d, want false and 1", r.first, r.announcement()[0x25])
	}

	// If export.pdb changes during the initial probe window (< 1.5s), timer.C must not claim a player number.
	root := t.TempDir()
	testfixture.Export(t, root)
	cfg := testExportConfig(root)
	cfg.ProbeDuration = 80 * time.Millisecond
	e := NewExporter(cfg)
	defer e.Close()
	if err := e.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, "PIONEER", "rekordbox", "export.pdb"), future, future); err != nil {
		t.Fatal(err)
	}
	time.Sleep(130 * time.Millisecond)
	if num := e.Status().Number; num != 0 {
		t.Fatalf("Exporter claimed player %d despite export.pdb changing before probe timer fired", num)
	}
}

func TestEngineBridgeClampsNegativePreRollCuesAndLoops(t *testing.T) {
	const rate = 44100.0
	var q bytes.Buffer
	binary.Write(&q, binary.BigEndian, uint64(8))
	for slot := 1; slot <= 8; slot++ {
		name := ""
		sample := float64(-1)
		switch slot {
		case 1:
			name = "LeadIn"
			sample = -22050 // -500 ms pre-roll hot cue
		case 2:
			name = "Drop"
			sample = 88200 // 2000 ms hot cue
		}
		q.WriteByte(byte(len(name)))
		q.WriteString(name)
		binary.Write(&q, binary.BigEndian, sample)
		q.Write([]byte{255, 255, 0, 0})
	}
	// main_cue at -4410 samples (-100 ms pre-roll), adjusted=1, default_cue=-1
	binary.Write(&q, binary.BigEndian, float64(-4410))
	q.WriteByte(1)
	binary.Write(&q, binary.BigEndian, float64(-1))

	cues, err := engineQuickCues(q.Bytes(), rate)
	if err != nil {
		t.Fatalf("engineQuickCues rejected negative pre-roll cue: %v", err)
	}
	if len(cues) != 3 || cues[0].HotCue != 1 || cues[0].TimeMS != 0 || cues[1].HotCue != 2 || cues[1].TimeMS != 2000 || cues[2].HotCue != 0 || cues[2].TimeMS != 0 {
		t.Fatalf("unexpected clamped cues: %+v", cues)
	}

	var lp bytes.Buffer
	binary.Write(&lp, binary.LittleEndian, uint64(3))
	writeLoop := func(name string, start, end float64) {
		lp.WriteByte(byte(len(name)))
		lp.WriteString(name)
		binary.Write(&lp, binary.LittleEndian, start)
		binary.Write(&lp, binary.LittleEndian, end)
		lp.Write([]byte{1, 1, 255, 0, 255, 0})
	}
	// Slot 1: spans pre-roll (-500 ms) into track (+1000 ms) -> clamps to 0..1000 ms and merges with HotCue 1
	writeLoop("LeadLoop", -22050, 44100)
	// Slot 2: purely pre-roll (-1000 ms .. -200 ms) -> skipped without failing
	writeLoop("PurePreRoll", -44100, -8820)
	// Slot 3: normal loop (4000 ms .. 8000 ms) -> preserved
	writeLoop("Chorus", 176400, 352800)

	merged, err := engineLoops(lp.Bytes(), rate, cues)
	if err != nil {
		t.Fatalf("engineLoops rejected negative pre-roll loop: %v", err)
	}
	var gotLead, gotChorus bool
	for _, c := range merged {
		if c.Name == "PurePreRoll" {
			t.Fatalf("purely pre-roll loop was not skipped: %+v", c)
		}
		if c.HotCue == 1 && c.Loop && c.TimeMS == 0 && c.EndMS == 1000 {
			gotLead = true
		}
		if c.Name == "Chorus" && c.Loop && c.TimeMS == 4000 && c.EndMS == 8000 {
			gotChorus = true
		}
	}
	if !gotLead || !gotChorus {
		t.Fatalf("expected clamped LeadLoop and preserved Chorus loop, got: %+v", merged)
	}
}

func TestEngineBridgePreservesChildBeforeParentDuplicateNamesAndFolderTracks(t *testing.T) {
	origStreamDirs := StreamDirs
	t.Cleanup(func() { StreamDirs = origStreamDirs })
	streamDir := t.TempDir()
	StreamDirs = []string{streamDir}
	if err := os.WriteFile(filepath.Join(streamDir, "deezer_1.wav"), testfixture.WAV(true), 0o644); err != nil {
		t.Fatal(err)
	}

	_, root, db := testlibrary.Create(t)
	for id := 1; id <= 4; id++ {
		rel := filepath.Join("Music", string(rune('a'+id-1))+".wav")
		testfixture.Write(t, root, rel, testfixture.WAV(true))
		if _, err := db.Exec(`INSERT INTO Track(id,title,artist,path,filename,length) VALUES(?,?, 'Artist', ?, ?, 4)`,
			id, "Track "+string(rune('0'+id)), "../"+filepath.ToSlash(rel), filepath.Base(rel)); err != nil {
			t.Fatal(err)
		}
	}

	// 1) Child playlist ID=2 ("Moved Into Later Folder") has parent ID=10 ("2026 Club"), where child.id < parent.id,
	//    AND parent ID=10 ALSO holds direct track 1 in Engine!
	// 2) Two root playlists share the title "Favorites" (IDs 3 and 4) with distinct tracks 2 and 3.
	// 3) User also has a root playlist named "EngineX Streaming" (ID 5) with track 4 while StreamDirs is non-empty.
	for _, stmt := range []string{
		`INSERT INTO Playlist(id,title,parentListId) VALUES (2,'Moved Into Later Folder',10)`,
		`INSERT INTO Playlist(id,title,parentListId) VALUES (3,'Favorites',0)`,
		`INSERT INTO Playlist(id,title,parentListId) VALUES (4,'Favorites',999)`,
		`INSERT INTO Playlist(id,title,parentListId) VALUES (5,'EngineX Streaming',0)`,
		`INSERT INTO Playlist(id,title,parentListId) VALUES (10,'2026 Club',0)`,
		`INSERT INTO PlaylistEntity(id,listId,trackId,nextEntityId) VALUES (1,10,1,0)`,
		`INSERT INTO PlaylistEntity(id,listId,trackId,nextEntityId) VALUES (2,2,2,0)`,
		`INSERT INTO PlaylistEntity(id,listId,trackId,nextEntityId) VALUES (3,3,2,0)`,
		`INSERT INTO PlaylistEntity(id,listId,trackId,nextEntityId) VALUES (4,4,3,0)`,
		`INSERT INTO PlaylistEntity(id,listId,trackId,nextEntityId) VALUES (5,5,4,0)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	if err := SyncEngineToPioneerExport(root); err != nil {
		t.Fatalf("SyncEngineToPioneerExport: %v", err)
	}
	lib, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER", "rekordbox", "export.pdb"))
	if err != nil {
		t.Fatalf("ReadPDB: %v", err)
	}

	byID := map[uint32]rekordbox.Playlist{}
	var clubFolderID uint32
	for _, p := range lib.Playlists {
		byID[p.ID] = p
		if p.ParentID == 0 && p.Folder && p.Name == "2026 Club" {
			clubFolderID = p.ID
		}
	}
	if clubFolderID == 0 {
		t.Fatalf("missing root folder '2026 Club': %+v", lib.Playlists)
	}

	var movedUnderClub, directClubTracks bool
	var fav1, fav2, userStreamLeaf bool
	for _, p := range lib.Playlists {
		if p.Name == "Moved Into Later Folder" && p.ParentID == clubFolderID && len(p.Tracks) == 1 {
			movedUnderClub = true
		}
		if p.ParentID == clubFolderID && !p.Folder && p.Name == "2026 Club" && len(p.Tracks) == 1 && p.Tracks[0] == 1 {
			directClubTracks = true
		}
		if p.ParentID == 0 && !p.Folder && p.Name == "Favorites" && len(p.Tracks) == 1 && p.Tracks[0] == 2 {
			fav1 = true
		}
		if p.ParentID == 0 && !p.Folder && p.Name == "Favorites (2)" && len(p.Tracks) == 1 && p.Tracks[0] == 3 {
			fav2 = true
		}
		if p.ParentID == 0 && !p.Folder && p.Name == "EngineX Streaming (2)" && len(p.Tracks) == 1 && p.Tracks[0] == 4 {
			userStreamLeaf = true
		}
	}
	if !movedUnderClub {
		t.Fatalf("child playlist (id=2 < parent=10) was flattened instead of nested under '2026 Club': %+v", lib.Playlists)
	}
	if !directClubTracks {
		t.Fatalf("tracks directly on Engine parent playlist '2026 Club' were dropped: %+v", lib.Playlists)
	}
	if !fav1 || !fav2 {
		t.Fatalf("duplicate root 'Favorites' playlists overwrote each other instead of disambiguating: %+v", lib.Playlists)
	}
	if !userStreamLeaf {
		t.Fatalf("user playlist named 'EngineX Streaming' collided with StreamFolder: %+v", lib.Playlists)
	}

	// Re-syncing after an m.db touch must preserve the exact playlist structure and IDs.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(root, "Engine Library", "Database2", "m.db"), future, future); err != nil {
		t.Fatal(err)
	}
	if err := SyncEngineToPioneerExport(root); err != nil {
		t.Fatalf("second SyncEngineToPioneerExport: %v", err)
	}
	lib2, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER", "rekordbox", "export.pdb"))
	if err != nil {
		t.Fatalf("second ReadPDB: %v", err)
	}
	if len(lib2.Playlists) != len(lib.Playlists) {
		t.Fatalf("re-sync changed playlist count: got %d, want %d (%+v)", len(lib2.Playlists), len(lib.Playlists), lib2.Playlists)
	}
}


