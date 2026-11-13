package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// stallingDeck accepts SSH logins and exec requests but never answers a command (BH-G2-9). With
// silent it also leaves keepalives unanswered, like a deck that lost power. execs gets each command.
func stallingDeck(t *testing.T, port int, silent bool) <-chan string {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(key)
	cfg := &ssh.ServerConfig{PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if string(pw) == "denon" {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(signer)
	l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	execs := make(chan string, 16)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				if silent {
					go func() {
						for range reqs { // never replied
						}
					}()
				} else {
					go ssh.DiscardRequests(reqs)
				}
				for nc := range chans {
					ch, rq, err := nc.Accept()
					if err != nil {
						continue
					}
					_ = ch // kept open, never written
					go func() {
						for r := range rq {
							var p struct{ Cmd string }
							ssh.Unmarshal(r.Payload, &p)
							r.Reply(r.Type == "exec", nil)
							if r.Type == "exec" {
								execs <- p.Cmd
							}
						}
					}()
				}
			}()
		}
	}()
	return execs
}

// Ctrl+C while the deck sits on a command: modeA returns instead of waiting for ever.
func TestModeAFollowsCancelWhileTheDeckStalls(t *testing.T) {
	o := testOpts(t)
	o.defaults()
	execs := stallingDeck(t, o.SSHPort, false)
	ctx, cancel := context.WithCancel(ctxT(t))
	defer cancel()
	go func() { <-execs; time.Sleep(100 * time.Millisecond); cancel() }()
	done := make(chan error, 1)
	go func() { done <- modeA(ctx, o, deck{IP: "127.0.0.1"}) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("modeA still waits on the stalled deck 10 s after Ctrl+C")
	}
}
