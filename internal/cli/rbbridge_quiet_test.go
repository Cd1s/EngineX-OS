package cli

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The simulated quiet window is shortened, but every case checks the real remount
// gate and uevents. No synthetic remove is allowed before the condition clears.
func TestIssue301WaitsThenRemountsAutomatically(t *testing.T) {
	for _, scenario := range []string{"peer_holds", "silent_peer", "serve_in_progress", "unconfirmed_serve"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFakeStick(t)
			f.engine("5", "", "")
			status := filepath.Join(t.TempDir(), "status")
			rep := newUSBLoadingReporter(status, "usb")
			var holds atomic.Bool
			holds.Store(scenario == "peer_holds")
			enginePeers = func() []net.IP { return []net.IP{net.IPv4(169, 254, 1, 3)} }
			peerDecks = func(net.IP) ([]string, bool) {
				if holds.Load() {
					return []string{"net://{owner}/REKORDBOX (USB 1)/Contents/song.mp3"}, true
				}
				return nil, scenario != "silent_peer"
			}
			ownDecks = func() ([]string, bool) { return nil, scenario != "unconfirmed_serve" }
			track := filepath.Join(f.mount, "Contents/song.mp3")
			if scenario == "serve_in_progress" || scenario == "unconfirmed_serve" {
				f.engine("9", track, "0100000")
				noteServed(f.mount)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			done := make(chan error, 1)
			go func() { done <- remount(ctx, f.mount, rep) }()
			defer func() { cancel(); <-done }()
			// An active read stays blocked longer than one quiet window. Others must
			// still wait before their initial quiet window expires.
			delay := serveQuiet / 2
			if scenario == "serve_in_progress" || scenario == "unconfirmed_serve" || scenario == "peer_holds" {
				delay = 2 * serveQuiet
			}
			time.Sleep(delay)
			if got := f.seen(); len(got) != 0 {
				t.Fatalf("premature remount: %v", got)
			}
			var st usbLoadingStatus
			data, err := os.ReadFile(status)
			if err != nil || json.Unmarshal(data, &st) != nil || st.State != "waiting" {
				t.Fatalf("waiting status: %s %v", data, err)
			}
			holds.Store(false)
			stopped := time.Now()
			if scenario == "serve_in_progress" || scenario == "unconfirmed_serve" {
				f.engine("9", "", "")
			}
			select {
			case err := <-done:
				done <- err // the cleanup always joins, including failed assertions above
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("did not remount automatically after release/quiet")
			}
			if (scenario == "serve_in_progress" || scenario == "unconfirmed_serve") && time.Since(stopped) < serveQuiet-remountWait {
				t.Fatalf("serve close did not restart quiet: %s", time.Since(stopped))
			}
			if got := strings.Join(f.seen(), " "); got != "remove: add:" {
				t.Fatalf("automatic publication: %q", got)
			}
		})
	}
}

// History accumulated during import counts: an already-quiet stick needs no new window.
func TestIssue301UsesObservedQuietHistory(t *testing.T) {
	if serveQuiet != 30*time.Second {
		t.Fatalf("documented quiet window: %s", serveQuiet)
	}
	f := newFakeStick(t)
	f.engine("5", "", "")
	serveQuiet = 2 * time.Second
	mark := servedMark(f.mount)
	if err := os.WriteFile(mark, nil, 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(mark, old, old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := remount(ctx, f.mount, newUSBLoadingReporter(filepath.Join(t.TempDir(), "status"), "usb")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.seen(), " "); got != "remove: add:" {
		t.Fatalf("ignored already-quiet history: %q", got)
	}
}
