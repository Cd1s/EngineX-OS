package bridge

import (
	"testing"
	"time"
)

func TestCueWatcherForgetWaitsForCancelledPass(t *testing.T) {
	done := make(chan struct{})
	cancelled := make(chan struct{})
	w := &CueWatcher{sticks: map[string]*cueStick{
		"/mount": {running: done, cancel: func() { close(cancelled) }},
	}}
	returned := make(chan struct{})
	go func() {
		w.forget(nil)
		close(returned)
	}()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("forget did not cancel the running pass")
	}
	select {
	case <-returned:
		t.Fatal("forget cleared the session before the cancelled pass ended")
	case <-time.After(20 * time.Millisecond):
	}
	close(done)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("forget did not finish after the running pass ended")
	}
}
