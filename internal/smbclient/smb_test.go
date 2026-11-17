package smbclient

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestSMBRejectsInvalidPathsAndCredentialURLs(t *testing.T) {
	for _, raw := range []string{"smb://user:password@server/music", "smb://server/", "smb://server/music/../other", "smb://server/music/a%5Cb", "smb://server/music?password=secret"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := Parse(raw); err == nil {
				t.Fatal("accepted invalid URL")
			}
		})
	}
}

func TestSMBDialStalledServerHasDeadline(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := l.Accept()
		if err == nil {
			defer c.Close()
			io.Copy(io.Discard, c)
		}
	}()
	start := time.Now()
	if c, err := Dial(context.Background(), "smb://"+l.Addr().String()+"/music", "user", "password"); err == nil {
		c.Close()
		t.Fatal("stalled server accepted")
	}
	if time.Since(start) > IOTimeout+time.Second {
		t.Fatalf("deadline took %s", time.Since(start))
	}
	<-done
}
