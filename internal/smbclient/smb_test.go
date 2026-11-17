package smbclient

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testsmb"
)

func TestSMBConfiguredSharesCanUseDirectCableAddresses(t *testing.T) {
	stopped := errors.New("dial reached")
	_, err := Dial(context.Background(), "smb://169.254.80.1/music", "user", "password", func(_ context.Context, _, addr string) (net.Conn, error) {
		if addr != "169.254.80.1:445" {
			t.Fatalf("wrong SMB address: %s", addr)
		}
		return nil, stopped
	})
	if !errors.Is(err, stopped) {
		t.Fatalf("configured direct-cable share was rejected before dial: %v", err)
	}
}

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
	if c, err := Dial(context.Background(), "smb://"+l.Addr().String()+"/music", "user", "password", nil); err == nil {
		c.Close()
		t.Fatal("stalled server accepted")
	}
	if time.Since(start) > ConnectTimeout+time.Second {
		t.Fatalf("deadline took %s", time.Since(start))
	}
	<-done
}

func TestSMBIdleConnectionSurvivesShortCachePause(t *testing.T) {
	s := testsmb.Start(t)
	os.WriteFile(filepath.Join(s.Root, "a.mp3"), []byte("ID3 fixture"), 0600)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	c, err := Dial(ctx, s.URL, s.User, s.Password, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// A local drive write may pause between SMB requests; the receiver is idle.
	time.Sleep(4 * time.Second)
	f, err := c.Share.Open("a.mp3")
	if err != nil {
		t.Fatalf("short idle gap killed SMB session: %v", err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil || string(b) != "ID3 fixture" {
		t.Fatalf("read after pause: %q %v", b, err)
	}
}
