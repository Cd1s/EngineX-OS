package prolink

import (
	"sync"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// Bug hunt 3: a second Observe that waited for the first one's resolve read the cache
// without the full beat list.
func TestObserveThatWaitedForAResolveGetsTheFullBeats(t *testing.T) {
	d := NewDiscovery()
	started := make(chan struct{})
	unblock := make(chan struct{})
	beats := []rekordbox.Beat{{Number: 1, BPM: 120, TimeMS: 500}, {Number: 2, BPM: 120, TimeMS: 1000}}
	d.SetResolver(func(dev Device) (*rekordbox.Analysis, string, string) {
		close(started)
		<-unblock
		return &rekordbox.Analysis{Beats: beats}, "Test Song", "Test Artist"
	})
	dev := Device{Address: "192.168.1.100", Number: 1, PacketType: 0x0a, TrackID: 42, SourcePlayer: 1, SourceSlot: 3, Beat: 2}
	var first, second Device
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		first = d.Observe(dev)
	}()
	<-started
	wg.Add(1)
	go func() {
		defer wg.Done()
		second = d.Observe(dev) // waits for the resolve that is running
	}()
	time.Sleep(20 * time.Millisecond)
	close(unblock)
	wg.Wait()
	if len(first.fullBeats) != len(beats) {
		t.Fatalf("first: %d full beats, want %d", len(first.fullBeats), len(beats))
	}
	if len(second.fullBeats) != len(beats) {
		t.Fatalf("the one that waited: %d full beats, want %d", len(second.fullBeats), len(beats))
	}
}
