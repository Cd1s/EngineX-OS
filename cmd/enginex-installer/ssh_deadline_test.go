package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// BH-G2-9: a read-only command the deck never answers ends at its deadline.
func TestSSHReadOnlyCommandHasADeadline(t *testing.T) {
	o := testOpts(t)
	o.defaults()
	stallingDeck(t, o.SSHPort, false)
	saved := sshReadTimeout
	sshReadTimeout = 300 * time.Millisecond
	defer func() { sshReadTimeout = saved }()
	start := time.Now()
	err := modeA(ctxT(t), o, deck{IP: "127.0.0.1"})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v after %v, want the read deadline", err, time.Since(start))
	}
}

// BH-G2-9: a deck that stops answering altogether (power lost) is dropped even during a command
// that Ctrl+C does not cut off (the first-generation firmware write).
func TestSSHDeadDeckIsDropped(t *testing.T) {
	o := testOpts(t)
	o.defaults()
	stallingDeck(t, o.SSHPort, true)
	saved := sshKeepAlive
	sshKeepAlive = 100 * time.Millisecond
	defer func() { sshKeepAlive = saved }()
	cl, err := dialSSH(ctxT(t), o, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	done := make(chan error, 1)
	go func() {
		_, err := sshRun(context.WithoutCancel(ctxT(t)), cl, "/usr/sbin/az01-update --file x", nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a command on a dead deck reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still waiting on a deck that answers nothing")
	}
}
