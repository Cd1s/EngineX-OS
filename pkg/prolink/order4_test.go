package prolink

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
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

func TestObserveAndBuildSnapshotBeatPhaseWithPitchAndPQTZOffset(t *testing.T) {
	// 600-beat PQTZ grid starting at 230 ms (500 ms per beat at 120.00 BPM).
	beats := make([]rekordbox.Beat, 600)
	for i := range beats {
		beats[i] = rekordbox.Beat{
			Number: uint16((i % 4) + 1),
			BPM:    120.0,
			TimeMS: uint32(230 + i*500),
		}
	}

	disc := NewDiscovery()
	disc.SetResolver(func(Device) (*rekordbox.Analysis, string, string) {
		return &rekordbox.Analysis{
			Beats:     beats,
			Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: []byte{10, 20, 30}}},
		}, "Offset Track", "Artist"
	})

	ip := net.IPv4(192, 168, 1, 21)
	mkStatus := func(beat uint32) Device {
		pkt := make([]byte, 0xd0)
		copy(pkt, "Qspt1WmJOL")
		pkt[10] = 0x0a
		copy(pkt[11:], "CDJ-3000")
		pkt[0x21] = 1
		pkt[0x28] = 1
		pkt[0x29] = 3
		binary.BigEndian.PutUint32(pkt[0x2c:], 9)
		pkt[0x89] = 0x40 | 0x20
		binary.BigEndian.PutUint32(pkt[0x8c:], uint32(math.Round(0x100000*1.08))) // +8% pitch -> 129.60 EffectiveBPM
		binary.BigEndian.PutUint16(pkt[0x92:], 12000)                             // 120.00 BPM
		binary.BigEndian.PutUint32(pkt[0xa0:], beat)
		pkt[0xa6] = byte(((beat - 1) % 4) + 1)
		dev, err := ParsePacket(pkt, 50002, ip)
		if err != nil {
			t.Fatal(err)
		}
		return dev
	}

	// Beat 2 must resolve to PQTZ[1].TimeMS = 730 ms (even though ParsePacket set coarse 500 ms != 0).
	obs2 := disc.Observe(mkStatus(2))
	if obs2.PlayPositionMS != 730 {
		t.Fatalf("Beat 2 PlayPositionMS = %d, want PQTZ 730 ms", obs2.PlayPositionMS)
	}
	if obs2.BeatPhase > 0.01 && obs2.BeatPhase < 0.99 {
		t.Fatalf("Beat 2 on-beat BeatPhase = %.4f, want ~0.0", obs2.BeatPhase)
	}

	// Beat 600 (> 512 JSON beat cap) must still resolve to full PQTZ[599].TimeMS = 299730 ms.
	obs600 := disc.Observe(mkStatus(600))
	if obs600.PlayPositionMS != 299730 {
		t.Fatalf("Beat 600 PlayPositionMS = %d, want full PQTZ 299730 ms", obs600.PlayPositionMS)
	}

	// CDJ-3000 0x0b precise position at 980 ms (exact midpoint between beat 2 at 730 ms and beat 3 at 1230 ms)
	// while pitched +8% must report BeatPhase = 0.50, not drift by EffectiveBPM.
	posPkt := make([]byte, 0x3c)
	copy(posPkt, "Qspt1WmJOL")
	posPkt[10] = 0x0b
	copy(posPkt[11:], "CDJ-3000")
	posPkt[0x1f] = 1
	posPkt[0x21] = 1
	binary.BigEndian.PutUint16(posPkt[0x22:], 0x0018)
	binary.BigEndian.PutUint32(posPkt[0x24:], 310)
	binary.BigEndian.PutUint32(posPkt[0x28:], 980)  // 980 ms track time
	binary.BigEndian.PutUint32(posPkt[0x2c:], 800)  // +8.00% pitch
	binary.BigEndian.PutUint32(posPkt[0x38:], 1296) // 129.6 EffectiveBPM
	devPos, err := ParsePacket(posPkt, 50001, ip)
	if err != nil {
		t.Fatal(err)
	}
	obsMid := disc.Observe(devPos)
	if math.Abs(obsMid.BeatPhase-0.5) > 0.01 {
		t.Fatalf("Observe 0x0b midpoint BeatPhase = %.4f, want 0.50", obsMid.BeatPhase)
	}
	snap := BuildSnapshot([]Device{obsMid})
	if math.Abs(snap.Devices[0].BeatPhase-0.5) > 0.01 {
		t.Fatalf("BuildSnapshot PQTZ midpoint BeatPhase = %.4f, want 0.50", snap.Devices[0].BeatPhase)
	}

	// Fallback without PQTZ beat grid: track-time PlayPositionMS=5250 ms at 120 BPM pitched +8% (EffectiveBPM=129.6)
	// is halfway through beat 11 (BeatPhase = 0.50), never divided by EffectiveBPM beat length.
	noGrid := Device{
		Number:         2,
		LastSeen:       time.Now(),
		BPM:            120.0,
		Pitch:          8.0,
		EffectiveBPM:   129.6,
		PlayPositionMS: 5250,
	}
	snapNoGrid := BuildSnapshot([]Device{noGrid})
	if math.Abs(snapNoGrid.Devices[0].BeatPhase-0.5) > 0.01 {
		t.Fatalf("BuildSnapshot un-gridded pitched BeatPhase = %.4f, want 0.50", snapNoGrid.Devices[0].BeatPhase)
	}
}

func TestExportRebindPlayingStatusHandoffFirmwareAndDBServerMetadata(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)

	curIP := net.IPv4(127, 63, 0, 11)
	cfg := testExportConfig(root)
	cfg.IP = curIP
	cfg.BeatAddr = "127.63.0.11:0"
	// Observe intentionally nil: beat socket must still open for 0x26/0x27 master handoff.
	e := NewExporter(cfg)
	defer e.Close()

	now := time.Now()
	e.SetDeck(ExportDeck{Playing: true, Master: false, BPM: 125.0, Pitch: -4.0, Beat: 2.25, At: now})
	if err := e.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	waitNumber(t, e, 4, 300*time.Millisecond)

	e.mu.Lock()
	r1 := e.run
	e.mu.Unlock()
	r1.mu.Lock()
	beatConn := r1.beat
	stPkt := r1.statusPacket()
	r1.mu.Unlock()

	if beatConn == nil {
		t.Fatal("r.beat was not opened when Observe == nil")
	}
	wantPitch := uint32(math.Round(0x100000 * 0.96))
	if stPkt[0x27] != 1 || stPkt[0x7b] != 3 || stPkt[0x8b] != 0x7a {
		t.Fatalf("playing status left idle play-state bytes: 0x27=%d 0x7b=%d 0x8b=%#x", stPkt[0x27], stPkt[0x7b], stPkt[0x8b])
	}
	for _, off := range []int{0x8c, 0x98, 0xc0, 0xc4} {
		if got := binary.BigEndian.Uint32(stPkt[off:]); got != wantPitch {
			t.Fatalf("pitch at %#x = %#x, want %#x", off, got, wantPitch)
		}
	}
	if binary.BigEndian.Uint32(stPkt[0xa0:]) != 3 || stPkt[0xa6] != 3 {
		t.Fatalf("non-master playing deck missing beat/in-bar: beat=%#x inBar=%d", binary.BigEndian.Uint32(stPkt[0xa0:]), stPkt[0xa6])
	}

	// Non-master deck must NOT grant 0x26 master handoff.
	peerUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 63, 0, 12)})
	if err != nil {
		t.Fatal(err)
	}
	defer peerUDP.Close()
	ask := (&exportRun{player: 2}).handoffPacket(0x26, 0x28)
	_, _ = peerUDP.WriteToUDP(ask, beatConn.LocalAddr().(*net.UDPAddr))
	time.Sleep(30 * time.Millisecond)
	r1.mu.Lock()
	nonMasterHandoff := r1.handoff
	r1.mu.Unlock()
	if nonMasterHandoff != 0 {
		t.Fatalf("non-master deck granted handoff to %d", nonMasterHandoff)
	}

	// Promote to Master, trigger Rebind onto a new IP, and confirm ExportDeck survives Rebind.
	e.SetDeck(ExportDeck{Playing: true, Master: true, BPM: 128.0, Pitch: 2.5, Beat: 1.5, At: time.Now()})
	e.mu.Lock()
	e.cfg.IP = net.IPv4(127, 63, 0, 13)
	e.cfg.BeatAddr = "127.63.0.13:0"
	e.mu.Unlock()
	if err := e.Rebind(); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	waitNumber(t, e, 4, 300*time.Millisecond)

	e.mu.Lock()
	r2 := e.run
	e.mu.Unlock()
	r2.mu.Lock()
	reboundPkt := r2.statusPacket()
	statusAddr := r2.status.LocalAddr().(*net.UDPAddr)
	beatAddr := r2.beat.LocalAddr().(*net.UDPAddr)
	r2.mu.Unlock()
	if reboundPkt[0x89]&(0x40|0x20) != (0x40|0x20) || binary.BigEndian.Uint16(reboundPkt[0x92:]) != 12800 {
		t.Fatalf("Rebind lost ExportDeck state: % x", reboundPkt[0x89:0x96])
	}

	// Grant handoff as Master, then send a peer CDJ-3000 0x0a packet with Master=true and firmware "3.41":
	// sticky handoff must clear and outgoing firmware must match "3.41".
	_, _ = peerUDP.WriteToUDP(ask, beatAddr)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		r2.mu.Lock()
		h := r2.handoff
		r2.mu.Unlock()
		if h == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	peerStatus := make([]byte, 0x11c)
	copy(peerStatus, magic)
	peerStatus[10] = 0x0a
	copy(peerStatus[11:], "CDJ-3000")
	peerStatus[0x21] = 2
	copy(peerStatus[0x7c:0x80], "3.41")
	peerStatus[0x89] = 0x40 | 0x20 // Playing + Master
	binary.BigEndian.PutUint32(peerStatus[0x8c:], 0x100000)
	binary.BigEndian.PutUint16(peerStatus[0x92:], 12800)
	_, _ = peerUDP.WriteToUDP(peerStatus, statusAddr)

	clearedAndUpgraded := false
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		r2.mu.Lock()
		pkt := r2.statusPacket()
		r2.mu.Unlock()
		if pkt[0x9f] == 0xff && string(pkt[0x7c:0x80]) == "3.41" {
			clearedAndUpgraded = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !clearedAndUpgraded {
		r2.mu.Lock()
		pkt := r2.statusPacket()
		r2.mu.Unlock()
		t.Fatalf("expected handoff cleared (0xff) and firmware upgraded to 3.41, got handoff=%#x fw=%q", pkt[0x9f], pkt[0x7c:0x80])
	}

	// DBServer 0x2002 metadata rounding: BPM=127.9999 -> 12800, Duration=239.9 -> 240.
	dbs := &DBServer{
		Library: &rekordbox.Library{
			Tracks: []rekordbox.Track{{ID: 1, Path: "/Music/t.mp3", Title: "T", BPM: 127.9999, Duration: 239.9}},
		},
		Number: func() byte { return 4 },
	}
	dbs.tracks = map[uint32]rekordbox.Track{1: dbs.Library.Tracks[0]}
	metaItems, err := dbs.query(dbMessage{kind: 0x2002, args: []dbField{dbNumber(0x01010301), dbNumber(1)}})
	if err != nil {
		t.Fatalf("DBServer 0x2002 query: %v", err)
	}
	var gotDur, gotBPM uint32
	for _, it := range metaItems {
		if it.kind == 0xb {
			gotDur = it.id
		}
		if it.kind == 0xd {
			gotBPM = it.id
		}
	}
	if gotDur != 240 || gotBPM != 12800 {
		t.Fatalf("DBServer 0x2002 truncated float metadata: dur=%d (want 240) bpm=%d (want 12800)", gotDur, gotBPM)
	}
}
