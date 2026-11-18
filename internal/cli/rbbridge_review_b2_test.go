package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
)

// A serve observed during import postpones publication, then expires after a quiet window.
func TestIssue301RemountKeepsALibraryAPeerLoadedFrom(t *testing.T) {
	f := newFakeStick(t)
	f.engine("5", "", "")
	ownDecks = func() ([]string, bool) { return nil, true }
	f.engine("9", filepath.Join(f.mount, "Contents/song.mp3"), "0100000")
	watch, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); watchServed(watch, f.mount) }()
	err := pollUntil(watch, time.Second, func() bool { _, err := os.Stat(servedMark(f.mount)); return err == nil })
	stop()
	<-done
	if err != nil {
		t.Fatal("import watcher missed the outgoing read")
	}
	f.engine("9", "", "")
	if err := remount(context.Background(), f.mount, newUSBLoadingReporter(filepath.Join(t.TempDir(), "status"), "usb")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.seen(), " "); got != "remove: add:" {
		t.Fatalf("quiet after serving: uevent writes %q", got)
	}
	// A confirmed own-deck read must not be recorded as a new outgoing serve.
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(servedMark(f.mount), old, old); err != nil {
		t.Fatal(err)
	}
	ownDecks = func() ([]string, bool) { return []string{"net://{1}/REKORDBOX/Contents/song.mp3"}, true }
	f.engine("9", filepath.Join(f.mount, "Contents/song.mp3"), "0100000")
	noteServed(f.mount)
	if st, err := os.Stat(servedMark(f.mount)); err != nil || !st.ModTime().Equal(old) {
		t.Fatalf("own-deck read restarted peer quiet: %v %v", st, err)
	}
	f.engine("9", "", "")
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
	stamp := fmt.Sprintf("v3 PIONEER/rekordbox/export.pdb %d %d", st.Size(), st.ModTime().UnixNano())
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
	// Stick B's import starts and writes its progress; A's job (publishing) must leave it.
	b := newUSBLoadingReporter(status, "sd")
	b.state = "importing"
	b.Start()
	b.Report(4, 10, true)
	a := newDriveReporter(root)
	a.Publishing()
	a.Refresh()
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

// Re-check of 2e227f88 and the PM's steer: a peer that copied a track before any look leaves
// no trace here but Engine's connection to it (the reviewer's original probe). Each Engine Link
// peer is asked what its decks hold, as this deck asks its own stream-sync: an EngineX peer that
// holds nothing from this stick does not block the remount (two linked EngineX decks, the
// maintainer's set), one that holds a track of it blocks until release; a silent peer instead waits for quiet.
func TestIssue301RemountAsksEngineLinkPeers(t *testing.T) {
	f := newFakeStick(t)
	f.engine("5", "", "")
	// Engine's socket, established to a deck on the LAN (192.168.1.224 to .138), and one to
	// stream-sync on loopback.
	f.engine("6", "socket:[4242]", "02")
	f.engine("7", "socket:[4243]", "02")
	tcp := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: E001A8C0:C35A 8A01A8C0:C36A 01 00000000:00000000 00:00000000 00000000     0        0 4242 1 0 0 10 0\n" +
		"   1: 0100007F:1F90 0100007F:D431 01 00000000:00000000 00:00000000 00000000     0        0 4243 1 0 0 10 0\n"
	if err := os.MkdirAll(filepath.Join(f.proc, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.proc, "net", "tcp"), []byte(tcp), 0o644); err != nil {
		t.Fatal(err)
	}
	enginePeers = oldEnginePeers
	if got := enginePeers(); len(got) != 1 || !got[0].Equal(net.IPv4(192, 168, 1, 138)) {
		t.Fatalf("Engine Link peers %v, want only 192.168.1.138", got)
	}
	// The peer is an EngineX deck: its stream-sync answers GET /peer/decks.
	var mu sync.Mutex
	var holds []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/peer/decks" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		decks := []map[string]any{{"deck": 1}}
		for i, n := range holds {
			decks = append(decks, map[string]any{"deck": i + 2, "net": n})
		}
		json.NewEncoder(w).Encode(map[string]any{"decks": decks})
	}))
	defer srv.Close()
	peerDecks = func(ip net.IP) ([]string, bool) {
		if !ip.Equal(net.IPv4(192, 168, 1, 138)) {
			return nil, false
		}
		return decksAt(strings.TrimPrefix(srv.URL, "http://"))
	}
	try := func() ([]string, error) {
		t.Helper()
		before := len(f.seen())
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second) // bounded safety wait
		defer cancel()
		err := remount(ctx, f.mount, newUSBLoadingReporter(filepath.Join(t.TempDir(), "status"), "usb"))
		return f.seen()[before:], err
	}
	// It holds its own stick's track and nothing of ours: remounted.
	mu.Lock()
	holds = []string{"net://{9f}/THEIRS (USB 1)/Contents/x.mp3"}
	mu.Unlock()
	if got, err := try(); err != nil || strings.Join(got, " ") != "remove: add:" {
		t.Fatalf("EngineX peer holding nothing of the stick: %v %v", got, err)
	}
	// The peer holds this stick, then releases it: wait and remount automatically.
	mu.Lock()
	holds = []string{"net://{1}/REKORDBOX (USB 1)/Contents/Track00003.mp3"}
	mu.Unlock()
	before := len(f.seen())
	released := make(chan bool, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		released <- len(f.seen()) == before
		mu.Lock()
		holds = nil
		mu.Unlock()
	}()
	got, err := try()
	if stayed := <-released; !stayed || err != nil || strings.Join(got, " ") != "remove: add:" {
		t.Fatalf("peer release: held=%v events=%v err=%v", stayed, got, err)
	}
	// A stock peer does not answer: after the quiet window it does not block forever.
	peerDecks = func(net.IP) ([]string, bool) { return nil, false }
	if got, err := try(); err != nil || strings.Join(got, " ") != "remove: add:" {
		t.Fatalf("silent peer after quiet window: %v %v", got, err)
	}
	// Engine connected to no other device: remounted.
	f.engine("6", "", "")
	if got, err := try(); err != nil || strings.Join(got, " ") != "remove: add:" {
		t.Fatalf("no peer: %v %v", got, err)
	}
}

// Re-check of 2e227f88: Engine was seen sending a track while stream-sync did not answer; that
// read restarts the quiet window even when stream-sync cannot identify its owner.
func TestIssue301UnconfirmedServeWaitsThenRemounts(t *testing.T) {
	f := newFakeStick(t)
	f.engine("5", "", "")
	ownDecks = func() ([]string, bool) { return nil, false }
	f.engine("9", filepath.Join(f.mount, "Contents/song.mp3"), "0100000")
	noteServed(f.mount)
	f.engine("9", "", "")
	start := time.Now()
	if err := remount(context.Background(), f.mount, newUSBLoadingReporter(filepath.Join(t.TempDir(), "status"), "usb")); err != nil {
		t.Fatal(err)
	}
	if events := strings.Join(f.seen(), " "); events != "remove: add:" || time.Since(start) < serveQuiet {
		t.Fatalf("must wait for quiet, then remount: %v after %s", events, time.Since(start))
	}
}

// Re-check of 2e227f88: one status file for every drive; an old job of drive A neither
// deletes B's progress, nor writes its stale state over B's error, nor clears B's error.
func TestIssue301StatusIsOwnedByItsJob(t *testing.T) {
	p := filepath.Join(t.TempDir(), "status")
	a, b := newUSBLoadingReporter(p, "usb"), newUSBLoadingReporter(p, "sd")
	a.Publishing()
	b.state = "importing"
	b.Start() // a new import takes the status (re-check of fefdd5cf: only Start does)
	b.Report(4, 10, true)
	a.Done()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("A's exit deleted B's in-progress status: %v", err)
	}
	a, b = newUSBLoadingReporter(p, "usb"), newUSBLoadingReporter(p, "sd")
	a.Publishing()
	b.Fail(errors.New("B failed"))
	a.Refresh()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("A resurrected its old status after B failed: %v", err)
	}
	a.Done()
	if _, err := os.Stat(p + ".err"); err != nil {
		t.Fatalf("A's exit deleted B's error before another Start: %v", err)
	}
	// A job's own lifecycle is unchanged: it clears its own status and error.
	b.Done()
	if _, err := os.Stat(p + ".err"); !os.IsNotExist(err) {
		t.Fatalf("B's own cleanup left its error: %v", err)
	}
}

var oldEnginePeers = enginePeers // the real one, before newFakeStick stubs it

// Re-check of fefdd5cf: a deck cabled straight to this one has a link-local address
// (169.254.0.0/16, no DHCP); it is an Engine Link peer, asked like any other.
func TestIssue301LinkLocalPeerIsAsked(t *testing.T) {
	f := newFakeStick(t)
	f.engine("5", "", "")
	f.engine("6", "socket:[4242]", "02")
	if err := os.MkdirAll(filepath.Join(f.proc, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	tcp := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0201FEA9:C35A 0301FEA9:C36A 01 00000000:00000000 00:00000000 00000000     0        0 4242 1 0 0 10 0\n"
	if err := os.WriteFile(filepath.Join(f.proc, "net", "tcp"), []byte(tcp), 0o644); err != nil {
		t.Fatal(err)
	}
	enginePeers = oldEnginePeers
	var asked []string
	peerDecks = func(ip net.IP) ([]string, bool) {
		asked = append(asked, ip.String())
		return []string{"net://{1}/REKORDBOX (USB 1)/Contents/song.mp3"}, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = remount(ctx, f.mount, newUSBLoadingReporter(filepath.Join(t.TempDir(), "status"), "usb"))
	if got := f.seen(); len(got) > 0 || strings.Join(asked, ",") == "" || asked[0] != "169.254.1.3" {
		t.Fatalf("169.254.1.3 holds a track of the stick: asked %v, uevent writes %v", asked, got)
	}
}

// Re-check of fefdd5cf: a late state change of an old job (A publishing, waiting, or a publication
// failure) does not take the status over from a newer drive's import (B), nor write
// over B's error.
func TestIssue301LateTransitionKeepsNewerStatus(t *testing.T) {
	p := filepath.Join(t.TempDir(), "status")
	a, b := newUSBLoadingReporter(p, "usb"), newUSBLoadingReporter(p, "sd")
	a.state = "importing"
	a.Start()
	a.Report(10, 10, true)
	b.state = "importing"
	b.Start()
	b.Report(4, 10, true)
	a.Publishing()
	a.FailPublication(errors.New("old publication failed"))
	b.Refresh()
	var st usbLoadingStatus
	data, err := os.ReadFile(p)
	if err != nil || json.Unmarshal(data, &st) != nil || st.Owner != b.owner || st.State != "importing" || st.Done != 4 {
		t.Fatalf("A's late publishing took B's status: %s %v", data, err)
	}
	b.Fail(errors.New("B failed"))
	a.Waiting()
	a.FailPublication(errors.New("old publication failed"))
	if _, err = os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("A's late publication hid B's error: %v", err)
	}
	// The same drive's remount (another process) carries on its import job's status.
	t.Setenv("ENGINEX_USB_PREPARING_PATH", p)
	job, remounter := newDriveReporter("/media/X"), newDriveReporter("/media/X")
	job.Start()
	job.Publishing()
	remounter.Waiting()
	if data, _ = os.ReadFile(p); !strings.Contains(string(data), `"state":"waiting"`) {
		t.Fatalf("the drive's remount could not report waiting: %s", data)
	}
}
