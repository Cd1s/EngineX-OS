package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
)

// Review B2: a peer deck that loaded a track from this stick over Engine Link keeps it on the
// peer, where this deck cannot see it; the remount replaced the library under it. Engine sending
// the track (an audio file of the stick read with no copy written and no deck of this player
// holding it) marks the stick, and the library then opens at the next insert. A track this
// player's own deck plays from the stick is not a peer's: the remount goes ahead after the unload.
func TestIssue301RemountKeepsALibraryAPeerLoadedFrom(t *testing.T) {
	f := newFakeStick(t)
	f.engine("5", "", "")
	track := filepath.Join(f.mount, "Contents", "Track00003.mp3")
	if err := os.MkdirAll(filepath.Dir(track), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(track, []byte("ID3"), 0o644); err != nil {
		t.Fatal(err)
	}
	ownDecks = func() ([]string, bool) { return []string{"net://{1}/OTHER/Contents/a.mp3"}, true }
	// While the import job ran: Engine sent the track to a peer, then closed it.
	watch, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { watchServed(watch, f.mount); close(done) }()
	f.engine("9", track, "0100000")
	time.Sleep(150 * time.Millisecond)
	f.engine("9", "", "")
	stop()
	<-done
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := remount(ctx, f.mount, newUSBLoadingReporter(filepath.Join(t.TempDir(), "status"), "usb"))
	if got := f.seen(); len(got) != 0 {
		t.Fatalf("remounted while a peer may hold a track from the stick: %v", got)
	}
	if err == nil || !strings.Contains(err.Error(), "insert it again") {
		t.Fatalf("no reason for the banner: %v", err)
	}
	// This player's own deck playing the same file from the stick: not a peer.
	os.Remove(servedMark(f.mount))
	ownDecks = func() ([]string, bool) { return []string{"net://{1}/REKORDBOX/Contents/Track00003.mp3"}, true }
	f.engine("9", track, "0100000")
	go func() {
		time.Sleep(150 * time.Millisecond)
		f.engine("9", "", "") // the deck unloads it
	}()
	if err = remount(context.Background(), f.mount, newUSBLoadingReporter(filepath.Join(t.TempDir(), "status"), "usb")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.seen(), " "); got != "remove: add:" {
		t.Fatalf("own deck's unload: uevent writes %q", got)
	}
}

// Review B2: a failed background import keeps its error for the browser; the deferred cleanup
// removed it at once.
func TestIssue301ImportFailureStaysVisible(t *testing.T) {
	oldMounted, oldLock := mounted, importLock
	mounted = func(string) bool { return true }
	importLock = filepath.Join(t.TempDir(), "lock")
	defer func() { mounted, importLock = oldMounted, oldLock }()
	status := filepath.Join(t.TempDir(), "status")
	t.Setenv("ENGINEX_USB_PREPARING_PATH", status)
	err := runImportJob(context.Background(), filepath.Join(t.TempDir(), "disappeared"))
	if err == nil {
		t.Fatal("expected an import error")
	}
	b, e := os.ReadFile(status + ".err")
	var st usbLoadingStatus
	if e != nil || json.Unmarshal(b, &st) != nil || st.State != "error" || st.Error == "" {
		t.Fatalf("background error %v was erased before the UI could poll: %v %s", err, e, b)
	}
}

// Review B2: a stick whose import is done no longer holds the global lock while it waits for its
// remount, so a second stick's import runs; its status is not overwritten by the first's refresh.
func TestIssue301CompletedImportReleasesGlobalLock(t *testing.T) {
	oldMounted, oldLock, oldRemount := mounted, importLock, startRemount
	mounted = func(string) bool { return true }
	importLock = filepath.Join(t.TempDir(), "lock")
	defer func() { mounted, importLock, startRemount = oldMounted, oldLock, oldRemount }()
	root := t.TempDir()
	testfixture.Export(t, root)
	st, err := os.Stat(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, "Engine Library/Database2/.rb-bridge-import")
	if err = os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := fmt.Sprintf("v2 PIONEER/rekordbox/export.pdb %d %d", st.Size(), st.ModTime().UnixNano())
	if err = os.WriteFile(filepath.Join(stage, ".rb-bridge-stamp"), []byte(stamp), 0o644); err != nil {
		t.Fatal(err)
	}
	status := filepath.Join(t.TempDir(), "status")
	t.Setenv("ENGINEX_USB_PREPARING_PATH", status)
	reached := make(chan struct{})
	startRemount = func(string) error { close(reached); return nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runImportJob(ctx, root) }()
	select {
	case <-reached:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatalf("job never reached remount: %v", <-done)
	}
	lock, err := os.OpenFile(importLock, os.O_RDWR, 0o600)
	if err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	// Stick B's import writes its progress; A's refresh must leave it.
	b := newUSBLoadingReporter(status, "sd")
	b.state = "importing"
	b.Report(3, 10, true)
	refreshed := newUSBLoadingReporter(status, "usb")
	refreshed.set("publishing")
	b.Report(4, 10, true)
	refreshed.Refresh()
	got, _ := os.ReadFile(status)
	lock.Close()
	cancel()
	if joberr := <-done; joberr != nil {
		t.Fatal(joberr)
	}
	if err != nil {
		t.Fatalf("completed stick A still prevents stick B importing: %v", err)
	}
	if !strings.Contains(string(got), `"done":4`) {
		t.Fatalf("A's refresh overwrote B's progress: %s", got)
	}
}
