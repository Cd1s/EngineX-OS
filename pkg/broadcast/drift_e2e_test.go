package broadcast

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// End to end through the capture loop, with the API the old code already had: a
// writer on the feed FIFO whose audio clock runs fast (4000 ppm here, so a few
// seconds show it; the deck measured about 1400) writes what that clock has
// produced, like the Engine hook does. The stream must not carry the surplus: the
// old loop published every frame it read, so the stream ran at the audio clock's
// rate and a receiver's backlog grew for as long as it listened.
func TestFastAudioClockDoesNotReachTheStream(t *testing.T) {
	if testing.Short() {
		t.Skip("runs for 10 s in real time")
	}
	path := filepath.Join(t.TempDir(), "feed.pcm")
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	b := New(newTestGuard())
	defer b.Close()
	if err := b.Configure(Config{Enabled: true, InputPath: path}); err != nil {
		t.Fatal(err)
	}
	ch, cancel := b.Subscribe()
	defer cancel()
	w, err := os.OpenFile(path, os.O_WRONLY, 0) // waits for the capture loop to open it
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	const rate, run = SampleRate * 1.004, 10 * time.Second
	received := make(chan int64, 1)
	go func() { // every published frame that is not silence
		var n int64
		for pcm := range ch {
			for i := 0; i+3 < len(pcm); i += 4 {
				if binary.LittleEndian.Uint32(pcm[i:]) != 0 {
					n++
				}
			}
		}
		received <- n
	}()
	frame := []byte{0x11, 0x22, 0x33, 0x44} // a constant, never zero
	chunk := make([]byte, 0, 4096)
	var written int64
	start := time.Now()
	for time.Since(start) < run {
		time.Sleep(10 * time.Millisecond)
		for due := int64(rate * time.Since(start).Seconds()); written < due; {
			n := min(due-written, 1024) // PIPE_BUF: whole frames per write, like the hook
			chunk = chunk[:0]
			for i := int64(0); i < n; i++ {
				chunk = append(chunk, frame...)
			}
			if _, err := w.Write(chunk); err != nil {
				t.Fatal(err)
			}
			written += n
		}
	}
	time.Sleep(300 * time.Millisecond) // let the loop read what is in the pipe
	cancel()
	got := <-received
	// 4000 ppm over 10 s is 1920 surplus frames. Locked from about 3 s on, the stream
	// leaves out roughly 1300 of them; even without a lock the pull leaves out 200.
	surplus := written - got
	t.Logf("wrote %d frames in %v, stream carried %d: %d left out (%.0f ppm)", written, run, got, surplus, float64(surplus)/float64(written)*1e6)
	if surplus < 150 {
		t.Fatalf("the stream carried %d of %d frames written by a +4000 ppm audio clock: the surplus reaches the listeners", got, written)
	}
	if surplus > int64(float64(written)*0.008) {
		t.Fatalf("the stream left out %d of %d frames: more than twice the clock offset", surplus, written)
	}
}
