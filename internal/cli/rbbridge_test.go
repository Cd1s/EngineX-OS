package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/bridge/bridgetest"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	_ "modernc.org/sqlite"
)

func TestRBBridgeValidation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{}, "specify --mount-path or --prolink"},
		{[]string{"--prolink", "--bind=invalid"}, "bind must be IPv4"},
		{[]string{"--mount-path=/missing-library", "--prolink", "--bind=::1"}, "bind must be IPv4"},
		{[]string{"--mount-path=/x", "extra"}, "unexpected positional"},
	} {
		err := RBBridge(tc.args, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: %v, want %s", tc.args, err, tc.want)
		}
	}
}

func TestRBBridgeRetiredWatchExitsAtOnce(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	// The unit in the player's base /etc runs "--watch --prolink".
	for _, args := range [][]string{{"--watch", "--prolink"}, {"--watch", "--watch-roots", root, "--poll-interval", "1ms"}} {
		var out bytes.Buffer
		start := time.Now()
		if err := RBBridge(args, &out); err != nil || out.Len() != 0 || time.Since(start) > time.Second {
			t.Fatalf("%v: %v %q %s", args, err, &out, time.Since(start))
		}
	}
	if _, err := os.Stat(filepath.Join(root, "Engine Library")); !os.IsNotExist(err) {
		t.Fatal("retired watch imported a volume")
	}
}

// An unchanged export is recognised from its own size and time alone: the
// second run must not need the tracks or analysis files at all.
func TestRBBridgeUnchangedExportSkipsTheVolume(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	run := func() bridge.MountResult {
		t.Helper()
		var out bytes.Buffer
		if err := RBBridge([]string{"--mount-path", root}, &out); err != nil {
			t.Fatal(err)
		}
		var r bridge.MountResult
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatalf("%q: %v", &out, err)
		}
		return r
	}
	if r := run(); r.Bypassed || r.Added == 0 {
		t.Fatal(r)
	}
	db := filepath.Join(root, "Engine Library", "Database2", "m.db")
	// The same export again before the background job has done anything: nothing is written.
	before, _ := os.ReadFile(db)
	if r := run(); r.Added != 0 || r.Updated != 0 || r.PlaylistsChanged {
		t.Fatal(r)
	}
	if after, _ := os.ReadFile(db); !bytes.Equal(before, after) {
		t.Fatal("an unchanged export rewrote m.db while its import was still pending")
	}
	// Once the import is complete an unchanged export costs a stat: the export's other files are not even opened.
	bridgetest.Import(t, root)
	before, _ = os.ReadFile(db)
	if err := os.RemoveAll(filepath.Join(root, "PIONEER", "USBANLZ")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if r := run(); !r.Bypassed {
		t.Fatal(r)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("unchanged export took %s", d)
	}
	if after, _ := os.ReadFile(db); !bytes.Equal(before, after) {
		t.Fatal("unchanged export rewrote m.db")
	}
}

// The command line the hook runs: rb-bridge --mount-path with ENGINEX_HOOK_REMAINING_MS set. On
// 2026-11-24 a deck's re-export with one new playlist ran the full import to the hook's kill,
// showed the old library and started no job, because the quick branch was reachable only
// without the variable, which is how every test and every measurement had run it. With the
// variable the new list is there at that mount and the job is started; and a run the hook
// gives almost no time ends well before its kill with the job started.
func TestMountPathAsTheHookRunsIt(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	started := 0
	old := startImport
	startImport = func(string) error { started++; return nil }
	defer func() { startImport = old }()
	run := func(ms string) bridge.MountResult {
		t.Helper()
		t.Setenv("ENGINEX_HOOK_REMAINING_MS", ms)
		var out bytes.Buffer
		if err := RBBridge([]string{"--mount-path", root}, &out); err != nil {
			t.Fatal(err)
		}
		var r bridge.MountResult
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatalf("%q: %v", &out, err)
		}
		return r
	}
	reexport := func(list string) {
		t.Helper()
		pdb := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
		st, err := os.Stat(pdb)
		if err != nil {
			t.Fatal(err)
		}
		names := [5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}
		testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", testfixture.PDBNamed(names, list))
		later := st.ModTime().Add(time.Hour)
		if err = os.Chtimes(pdb, later, later); err != nil {
			t.Fatal(err)
		}
	}
	if r := run("17000"); r.Added != 5 || !r.Background || started != 1 {
		t.Fatalf("first insert: %+v, job started %d times", r, started)
	}
	bridgetest.Import(t, root)
	if r := run("17000"); !r.Bypassed || started != 1 {
		t.Fatalf("unchanged insert: %+v, job started %d times", r, started)
	}
	reexport("New Set")
	r := run("17000")
	if !r.PlaylistsChanged || r.Added != 0 || r.Bypassed || !r.Background || started != 2 {
		t.Fatalf("re-export with one new list: %+v, job started %d times", r, started)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(root, "Engine Library/Database2/m.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var lists int
	if err = db.QueryRow(`SELECT count(*) FROM Playlist WHERE title='New Set'`).Scan(&lists); err != nil || lists != 1 {
		t.Fatalf("the new list is in the library %d times (%v)", lists, err)
	}
	// Half a second of life: the job is started first, and the run ends on its own.
	reexport("Late Set")
	begin := time.Now()
	if r = run("500"); !r.Background || started != 3 || time.Since(begin) > 5*time.Second {
		t.Fatalf("run with 500 ms left: %+v, job started %d times, took %s", r, started, time.Since(begin))
	}
}

func TestRBBridgeProlinkFlags(t *testing.T) {
	t.Setenv("ENGINEX_PROLINK_STATE", "/tmp/custom-state.json")
	var out bytes.Buffer
	err := RBBridge([]string{"--prolink", "--state-file=/custom.json", "--anlz-root=/custom-anlz", "--bind=invalid"}, &out)
	if err == nil || !strings.Contains(err.Error(), "bind must be IPv4") {
		t.Fatalf("expected bind error, got %v", err)
	}
}

func TestProlinkSettingOnMigration(t *testing.T) {
	for _, tc := range []struct {
		name string
		conf string
		want bool
	}{
		{"only_share", "prolink_share=1\n", true},
		{"only_export", "prolink_export=1\n", true},
		{"both", "prolink_share=1\nprolink_export=1\n", true},
		{"neither", "waveform=rgb\n", false},
		{"both_off", "prolink_share=0\nprolink_export=0\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.conf")
			if err := os.WriteFile(path, []byte(tc.conf), 0600); err != nil {
				t.Fatal(err)
			}
			if got := prolinkSettingOn(path); got != tc.want {
				t.Fatalf("prolinkSettingOn(%q) = %v, want %v", tc.conf, got, tc.want)
			}
		})
	}
}

// A deck's journal got every Pro DJ Link packet from rb-bridge (25,660 lines in
// 30 min with a computer on the link): it now gets a device when it appears and
// when it has been silent for 10 s; packets need verbose_log=1.
func TestRBBridgeLogsDevicesNotPackets(t *testing.T) {
	var lines []string
	logf := func(v ...any) { lines = append(lines, fmt.Sprint(v...)) }
	l := linkLog{seen: map[string]time.Time{}}
	now := time.Now()
	computer := prolink.Device{Address: "192.168.1.138", Number: 17, Name: "rekordbox"}
	for i := range 50 {
		l.packet(logf, computer, now.Add(time.Duration(i)*100*time.Millisecond))
	}
	l.packet(logf, prolink.Device{Address: "192.168.1.238", Number: 4, Name: "CDJ-3000"}, now)
	if l.expire(logf, now.Add(10*time.Second)) {
		t.Fatal("published a loss before the timeout")
	}
	if !l.expire(logf, now.Add(12*time.Second)) {
		t.Fatal("CDJ loss did not dirty the snapshot")
	}
	if !l.expire(logf, now.Add(16*time.Second)) {
		t.Fatal("computer loss did not dirty the snapshot")
	}
	if l.expire(logf, now.Add(17*time.Second)) {
		t.Fatal("unchanged snapshot dirtied again")
	}
	want := []string{
		`rb-bridge: Pro DJ Link device seen: 192.168.1.138 player 17 "rekordbox"`,
		`rb-bridge: Pro DJ Link device seen: 192.168.1.238 player 4 "CDJ-3000"`,
		`rb-bridge: Pro DJ Link device lost: 192.168.1.238 player 4 "CDJ-3000"`,
		`rb-bridge: Pro DJ Link device lost: 192.168.1.138 player 17 "rekordbox"`,
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("journal:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestRBBridgeWriteBackOnEjectFlag(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	var out bytes.Buffer
	if err := RBBridge([]string{"--mount-path", root}, &out); err != nil {
		t.Fatalf("initial mount import: %v", err)
	}
	bridgetest.Import(t, root)
	out.Reset()
	if err := RBBridge([]string{"--mount-path", root, "--writeback-cues", "--on-eject"}, &out); err != nil {
		t.Fatalf("writeback --on-eject: %v", err)
	}
	var res struct {
		TracksChecked int `json:"tracksChecked"`
		TracksChanged int `json:"tracksChanged"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode writeback result %q: %v", &out, err)
	}
	if res.TracksChecked == 0 || res.TracksChanged != 0 {
		t.Fatalf("unexpected untouched writeback result: %+v", res)
	}
}

func TestVerifyRemountPublishedFailsClosedOnStatAndSnapshotErrors(t *testing.T) {
	// 1. Stat error on pending swap artifact (Database2 is a regular file -> ENOTDIR) must fail verification.
	mount1 := t.TempDir()
	eng1 := filepath.Join(mount1, "Engine Library")
	if err := os.MkdirAll(eng1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(eng1, "Database2"), []byte("not-a-dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyRemountPublished(mount1, remountPending{hadSwap: true}); err == nil {
		t.Fatal("verifyRemountPublished passed when Database2 stat returned ENOTDIR")
	}

	// 2. Corrupt/missing snapshot header when verifying pending state change must fail verification.
	mount2 := t.TempDir()
	db2 := filepath.Join(mount2, "Engine Library", "Database2")
	if err := os.MkdirAll(db2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyRemountPublished(mount2, remountPending{hadPendingSnap: true, snapCommit: 7}); err == nil {
		t.Fatal("verifyRemountPublished passed when snapshot header could not be read")
	}

	// 3. readRemountPending must fail closed on stat error or corrupt swap header.
	if _, err := readRemountPending(mount1); err == nil {
		t.Fatal("readRemountPending passed when Database2 stat returned ENOTDIR")
	}
	mount3 := t.TempDir()
	db3 := filepath.Join(mount3, "Engine Library", "Database2")
	if err := os.MkdirAll(db3, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(db3, rbstate.SwapName), []byte("corrupt-swap"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readRemountPending(mount3); err == nil {
		t.Fatal("readRemountPending passed with corrupt swap snapshot header")
	}

	// 4. Discarded pending swap (files removed without promoting commit into m.db) must fail with ErrRemountDiscarded.
	mount4 := t.TempDir()
	db4 := filepath.Join(mount4, "Engine Library", "Database2")
	if err := os.MkdirAll(db4, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tmpLive := t.TempDir()
	_, uuid1, commit1, err := enginedb.BuildLibrary(ctx, tmpLive, "", []enginedb.TrackRow{{Rel: "Contents/a.mp3", Track: rekordbox.Track{Title: "a"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(tmpLive, "m.db"), filepath.Join(db4, "m.db")); err != nil {
		t.Fatal(err)
	}
	liveSnap := rbstate.New(uuid1)
	liveSnap.Commit = commit1
	liveSnap.Mode = rbstate.ModeFull
	if err := liveSnap.Save(db4); err != nil {
		t.Fatal(err)
	}
	tmpNext := t.TempDir()
	_, _, commit2, err := enginedb.BuildLibrary(ctx, tmpNext, uuid1, []enginedb.TrackRow{
		{Rel: "Contents/a.mp3", Track: rekordbox.Track{Title: "a"}},
		{Rel: "Contents/b.mp3", Track: rekordbox.Track{Title: "b"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	nextDBConn, err := sql.Open("sqlite", filepath.Join(tmpNext, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	commit2 = commit1 + 1
	if _, err := nextDBConn.Exec(`UPDATE Information SET lastRekordBoxLibraryImportReadCounter=?`, commit2); err != nil {
		_ = nextDBConn.Close()
		t.Fatal(err)
	}
	_ = nextDBConn.Close()
	if commit2 == commit1 {
		t.Fatalf("expected distinct commit2 (%d) != commit1 (%d)", commit2, commit1)
	}
	if err := os.Rename(filepath.Join(tmpNext, "m.db"), filepath.Join(db4, ".rb-next.db")); err != nil {
		t.Fatal(err)
	}
	swapSnap := rbstate.New(uuid1)
	swapSnap.Commit = commit2
	swapSnap.Applied = 1700000100
	swapSnap.Mode = rbstate.ModeOnDemand
	if err := rbstate.SaveBrowseIndexAs(filepath.Join(db4, rbstate.BrowseSwapName), swapSnap); err != nil {
		t.Fatal(err)
	}
	if err := swapSnap.SaveAs(filepath.Join(db4, rbstate.SwapName)); err != nil {
		t.Fatal(err)
	}
	pre, err := readRemountPending(mount4)
	if err != nil || !pre.hadSwap || pre.wantUUID != uuid1 || pre.wantCommit != commit2 {
		t.Fatalf("readRemountPending = %+v, err=%v", pre, err)
	}
	// Simulate discard (pending files removed, live m.db stayed on commit1).
	_ = os.Remove(filepath.Join(db4, ".rb-next.db"))
	_ = os.Remove(filepath.Join(db4, rbstate.SwapName))
	_ = os.Remove(filepath.Join(db4, rbstate.BrowseSwapName))
	if err := verifyRemountPublished(mount4, pre); !errors.Is(err, ErrRemountDiscarded) {
		t.Fatalf("verifyRemountPublished on discarded swap = %v, want ErrRemountDiscarded", err)
	}
	// Now promote the next identity + snapshot + matching browse index -> must succeed.
	tmpNext2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(db4, rbstate.SnapshotName), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = tmpNext2
	if err := swapSnap.Save(db4); err != nil {
		t.Fatal(err)
	}
	if err := rbstate.SaveBrowseIndex(db4, swapSnap); err != nil {
		t.Fatal(err)
	}
	// Put commit2 DB into m.db.
	tmpPub := t.TempDir()
	_, _, commitPub, err := enginedb.BuildLibrary(ctx, tmpPub, uuid1, []enginedb.TrackRow{
		{Rel: "Contents/a.mp3", Track: rekordbox.Track{Title: "a"}},
		{Rel: "Contents/b.mp3", Track: rekordbox.Track{Title: "b"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pubDBConn, err := sql.Open("sqlite", filepath.Join(tmpPub, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	commitPub = commit2
	if _, err := pubDBConn.Exec(`UPDATE Information SET lastRekordBoxLibraryImportReadCounter=?`, commitPub); err != nil {
		_ = pubDBConn.Close()
		t.Fatal(err)
	}
	_ = pubDBConn.Close()
	swapSnap.Commit = commitPub
	if err := swapSnap.Save(db4); err != nil {
		t.Fatal(err)
	}
	if err := rbstate.SaveBrowseIndex(db4, swapSnap); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(tmpPub, "m.db"), filepath.Join(db4, "m.db")); err != nil {
		t.Fatal(err)
	}
	prePub := remountPending{hadNextDB: true, hadSwap: true, hadBrowseSwap: true, wantUUID: uuid1, wantCommit: commitPub, wantMode: rbstate.ModeOnDemand}
	if err := verifyRemountPublished(mount4, prePub); err != nil {
		t.Fatalf("verifyRemountPublished on matching promoted state failed: %v", err)
	}
}

func TestRBBridgeWriteBackDryRunFlag(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	pdbPath := filepath.Join(root, "PIONEER", "rekordbox", "export.pdb")
	origPDB, err := os.ReadFile(pdbPath)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := RBBridge([]string{"--mount-path", root}, &out); err != nil {
		t.Fatalf("initial mount import: %v", err)
	}
	bridgetest.Import(t, root)
	// Trigger trigger_after_update_Track has 1-second strftime('%s') granularity; advance past the
	// import second so lastEditTime strictly moves when Track.genre is edited below.
	time.Sleep(1100 * time.Millisecond)

	db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET genre='EditedDryRunGenre' WHERE id=1`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	out.Reset()
	if err := RBBridge([]string{"--mount-path", root, "--writeback-cues", "--on-eject", "--dry-run"}, &out); err != nil {
		t.Fatalf("writeback --dry-run: %v", err)
	}
	var res struct {
		TracksChecked int `json:"tracksChecked"`
		TracksChanged int `json:"tracksChanged"`
		FilesUpdated  int `json:"filesUpdated"`
		Changes       []struct {
			EngineID int64    `json:"engineId"`
			Rel      string   `json:"rel"`
			Fields   []string `json:"fields"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode dry-run writeback result %q: %v", &out, err)
	}
	if res.TracksChanged != 1 || res.FilesUpdated != 0 || len(res.Changes) != 1 || res.Changes[0].EngineID != 1 || len(res.Changes[0].Fields) == 0 {
		t.Fatalf("unexpected dry-run result: %+v", res)
	}
	afterPDB, err := os.ReadFile(pdbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterPDB, origPDB) {
		t.Fatal("dry-run modified export.pdb on stick")
	}
}
