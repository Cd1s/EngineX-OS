package bridge

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

func TestReviewOnDemandUpgradeKeepsPublishedLibrary(t *testing.T) {
	fixture := os.Getenv("REVIEW_LEGACY_FIXTURE")
	create := os.Getenv("REVIEW_CREATE_LEGACY") == "1"
	var mount string
	if fixture == "" {
		mount = nextStick(t, 5)
	} else {
		mount = filepath.Join(fixture, "stick")
		t.Setenv(rbstate.StageEnv, filepath.Join(fixture, "stage"))
		if create {
			testfixture.LargeExport(t, mount, 5)
			mountNow(t, mount, MountOptions{})
			stageAll(t, mount)
		}
	}
	dir := dbDir(mount)
	if create || fixture == "" {
		prepareLegacyPending(t, mount)
	}
	stale, err := rbstate.ReadHeaderFile(filepath.Join(dir, rbstate.SwapName))
	if err != nil {
		t.Fatal(err)
	}
	published := snapshot(t, mount)
	if stale.Rules != published.Rules || stale.Commit == published.Commit {
		t.Fatalf("fixture needs an unpublished next of the same rules: live=%d next=%d", published.Commit, stale.Commit)
	}
	if create {
		return
	}
	deckRows := func() [][]string {
		return query(t, mount, `SELECT
			(SELECT count(*) FROM Track),
			(SELECT count(*) FROM Track WHERE title='title from stale export'),
			(SELECT count(*) FROM PerformanceData WHERE beatData IS NOT NULL),
			(SELECT count(*) FROM PlaylistEntity e JOIN Playlist l ON e.listId=l.id WHERE l.title='Deck set'),
			(SELECT rating FROM Track WHERE id=1),
			(SELECT isPlayed FROM Track WHERE id=1),
			(SELECT timeLastPlayed FROM Track WHERE id=1),
			(SELECT hex(quickCues) FROM PerformanceData WHERE trackId=2)`)
	}
	before := deckRows()
	mountNow(t, mount, MountOptions{OnDemand: true})
	after := deckRows()
	if got := snapshot(t, mount).Commit; got == stale.Commit {
		t.Errorf("published legacy pending commit %d over live commit %d", got, published.Commit)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("rows, stale titles, performance, list entries, rating, played state, timestamp: before=%v after=%v; cue preserved=%t", before[0][:7], after[0][:7], before[0][7] == after[0][7])
	}
	// Stale pending next artifacts from the previous build must be discarded, and live upgrade must
	// materialize a populated tagged browse index without touching m.db edits.
	for _, staleFile := range []string{nextDB, rbstate.SwapName, rbstate.BrowseSwapName} {
		if _, serr := os.Stat(filepath.Join(dir, staleFile)); !os.IsNotExist(serr) {
			t.Errorf("stale %s left after upgrade mount: err=%v", staleFile, serr)
		}
	}
	upgraded := snapshot(t, mount)
	if upgraded.Mode != rbstate.ModeOnDemand || !rbstate.BrowseIndexMatches(dir, upgraded.Header) {
		t.Fatalf("upgraded snapshot Mode=%q / BrowseIndexMatches=false", upgraded.Mode)
	}
	rawBrowse, err := os.ReadFile(filepath.Join(dir, rbstate.BrowseIndexName))
	if err != nil || !strings.Contains(string(rawBrowse), "\nT\t") {
		t.Fatalf("upgraded browse index missing track entries: err=%v\n%s", err, rawBrowse)
	}
}

func prepareLegacyPending(t *testing.T, mount string) {
	t.Helper()
	mountNow(t, mount, MountOptions{Publish: true})
	if n := query(t, mount, "SELECT count(*) FROM PerformanceData WHERE beatData IS NOT NULL")[0][0]; n != "5" {
		t.Fatalf("legacy published performance rows = %s, want 5", n)
	}

	engineWrites(t, mount, "UPDATE Track SET lastEditTime=1700000000")
	makeNextOf(t, mount, func(v *View) {
		v.Tracks[4].Track.Title = "title from stale export"
		v.Tracks[4].Hash = "stale-row-hash"
	})
	dir := dbDir(mount)
	live := filepath.Join(dir, "m.db")
	db, err := sql.Open("sqlite", "file:"+live)
	if err != nil {
		t.Fatal(err)
	}
	pos := 250.0
	if err := enginedb.SetQuickCue(context.Background(), db, 2, 0, "deck edit", "#12AB34", &pos); err != nil {
		db.Close()
		t.Fatal(err)
	}
	p5NativeList(t, db, "Deck set", 0, []int64{1, 2}, false)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	engineWrites(t, mount, "UPDATE Track SET rating=80,isPlayed=1,timeLastPlayed=1700000000,lastEditTime=1800000000 WHERE id=1")
}
