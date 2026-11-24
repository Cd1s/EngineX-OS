package bridge

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
)

func TestPort5NullTrackPathDoesNotBreakReexport(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	db := p5DB(t, mount)
	if _, err := db.Exec(`INSERT INTO Track(path,filename) VALUES(NULL,'cloud-only')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	pdb := testfixture.PDBNamed([5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}, "New Set")
	p5Reexport(t, mount, pdb)
	mountNow(t, mount, MountOptions{})
	if got := query(t, mount, `SELECT count(*) FROM Track WHERE path IS NULL`); got[0][0] != "1" {
		t.Fatalf("NULL path rows=%s, want 1", got[0][0])
	}
}

func TestPort5UnchangedWindowLeavesStickAndDatabaseFilesUntouched(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	beforeStick := treeSums(t, filepath.Join(mount, "PIONEER"))
	beforeMusic := treeSums(t, filepath.Join(mount, "MUSIC"))
	beforeDB := treeSums(t, dbDir(mount))
	first := mountNow(t, mount, MountOptions{})
	if !first.Bypassed {
		t.Fatalf("first unchanged window: %+v", first)
	}
	if got := treeSums(t, filepath.Join(mount, "PIONEER")); !reflect.DeepEqual(got, beforeStick) {
		t.Fatal("PIONEER changed on an unchanged window")
	}
	if got := treeSums(t, filepath.Join(mount, "MUSIC")); !reflect.DeepEqual(got, beforeMusic) {
		t.Fatal("audio files changed on an unchanged window")
	}
	second := mountNow(t, mount, MountOptions{})
	if !second.Bypassed {
		t.Fatalf("second unchanged window: %+v", second)
	}
	if got := treeSums(t, dbDir(mount)); !reflect.DeepEqual(got, beforeDB) {
		t.Fatal("Database2 changed on the second unchanged window")
	}
}

func TestPort5PlaylistOnlyReexportVerifiesStampForWriteback(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	pdb := testfixture.PDBNamed([5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}, "New Set")
	p5Reexport(t, mount, pdb)
	res := mountNow(t, mount, MountOptions{})
	if !res.PlaylistsChanged || res.Added != 0 || res.Updated != 0 || res.Removed != 0 {
		t.Fatalf("playlist-only reexport: %+v", res)
	}
	if got := snapshot(t, mount).Pending.Verify; !got {
		t.Fatal("playlist-only reexport did not request verification")
	}
	if _, err := FillInBackground(context.Background(), mount, FillOptions{}); err != nil {
		t.Fatal(err)
	}
	state, err := ImportState(mount)
	if err != nil {
		t.Fatal(err)
	}
	if state.Header.Pending.Verify || !state.Matches() {
		t.Fatalf("post-fill import state: pending verify=%v matches=%v", state.Header.Pending.Verify, state.Matches())
	}
	pass, err := WriteBackCues(context.Background(), mount, WriteBackOptions{SkipPreparingCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	if pass.TracksChecked == 0 {
		t.Fatalf("write-back skipped after verified playlist-only reexport: %+v", pass)
	}
}

func TestPort5PlaylistOnlyReexportDoesNotChangeTrackFiles(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	before := treeSums(t, filepath.Join(mount, "MUSIC"))
	pdb := testfixture.PDBNamed([5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}, "New Set")
	p5Reexport(t, mount, pdb)
	mountNow(t, mount, MountOptions{})
	if got := treeSums(t, filepath.Join(mount, "MUSIC")); !reflect.DeepEqual(got, before) {
		t.Fatal("playlist-only reexport changed audio files")
	}
}

func TestPort5WritebackGateUsesCurrentExportAfterFill(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	pdb := testfixture.PDBNamed([5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}, "New Set")
	p5Reexport(t, mount, pdb)
	mountNow(t, mount, MountOptions{})
	if _, err := FillInBackground(context.Background(), mount, FillOptions{}); err != nil {
		t.Fatal(err)
	}
	state, err := ImportState(mount)
	if err != nil || !state.Matches() || state.Header.Pending.Verify {
		t.Fatalf("state after fill: %+v %v", state, err)
	}
	if got := query(t, mount, `SELECT count(*) FROM Track`); got[0][0] != "5" {
		t.Fatalf("tracks after playlist-only reexport=%s", got[0][0])
	}
}
