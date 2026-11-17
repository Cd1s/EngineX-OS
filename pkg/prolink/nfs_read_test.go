package prolink

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// One endpoint forwards to the real server double, but holds the first window,
// loses two requests, and answers in reverse xid order (including duplicates).
func TestNFSReadPipelineLossReorderAndShortReads(t *testing.T) {
	root := t.TempDir()
	want := make([]byte, 13*nfsChunk+137)
	for i := range want {
		want[i] = byte(i * 31)
	}
	os.WriteFile(filepath.Join(root, "audio.flac"), want, 0600)
	s := &NFSServer{Root: root}
	if err := s.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	port := conn.LocalAddr().(*net.UDPAddr).Port
	done := make(chan struct{})
	var mu sync.Mutex
	seen := map[uint32]int{}
	ports := map[int]bool{}
	go func() {
		defer close(done)
		type held struct {
			packet []byte
			from   *net.UDPAddr
		}
		var batch []held
		gated := true
		buf := make([]byte, 64<<10)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			packet := append([]byte(nil), buf[:n]...)
			r := &reader{b: packet}
			r.u32()
			r.u32()
			r.u32()
			prog := r.u32()
			r.u32()
			proc := r.u32()
			r.u32()
			r.opaque(400)
			r.u32()
			r.opaque(400)
			if prog == rpcNFS && proc == 6 {
				r.fixed(fhSize)
				off := r.u32()
				mu.Lock()
				seen[off]++
				attempt := seen[off]
				ports[from.Port] = true
				mu.Unlock()
				if (off == 0 || off == 2*nfsChunk) && attempt == 1 {
					continue
				}
				if off == 0 {
					binary.BigEndian.PutUint32(packet[len(packet)-8:], 1000)
				} // legal short READ
				if gated {
					batch = append(batch, held{packet, from})
					if len(batch) < nfsReadWindow {
						continue
					}
					gated = false
					for i := len(batch) - 1; i >= 0; i-- {
						reply := s.handle(batch[i].packet, uint32(port))
						conn.WriteToUDP(reply, batch[i].from)
						conn.WriteToUDP(reply, batch[i].from)
					}
					continue
				}
			}
			conn.WriteToUDP(s.handle(packet, uint32(port)), from)
		}
	}()
	c := &NFSClient{IP: net.IPv4(127, 0, 0, 1), Portmap: port, Timeout: 40 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var got bytes.Buffer
	_, err = c.FetchTo(ctx, "/C/", "/audio.flac", &got, uint32(len(want)))
	conn.Close()
	<-done
	if err != nil || !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("pipelined bytes: %d/%d: %v", got.Len(), len(want), err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen[0] < 2 || seen[2*nfsChunk] < 2 || seen[1000] != 1 {
		t.Fatalf("lost/short READs were not recovered: %v", seen)
	}
	if len(ports) != 1 {
		t.Fatalf("READs opened %d sockets instead of one", len(ports))
	}
}

type nfsShortWriter struct{}

func (nfsShortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestNFSReadPipelineCancellationAndWriterError(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.mp3"), make([]byte, 16*nfsChunk), 0600)
	s := &NFSServer{Root: root}
	if err := s.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := &NFSClient{IP: net.IPv4(127, 0, 0, 1), Portmap: s.PortmapPort()}
	if _, err := c.FetchTo(t.Context(), "/C/", "/a.mp3", nfsShortWriter{}, 1<<20); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short writer: %v", err)
	}
	fh, info, err := c.Lookup(t.Context(), "/C/", "/a.mp3")
	if err != nil {
		t.Fatal(err)
	}
	silent, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		finished <- c.readFile(ctx, silent.LocalAddr().(*net.UDPAddr).Port, fh, info.Size, io.Discard, nfsReadWindow)
	}()
	silent.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := silent.ReadFromUDP(make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel left an NFS receiver running")
	}
	c.Timeout = 10 * time.Millisecond
	start := time.Now()
	if err := c.readFile(t.Context(), silent.LocalAddr().(*net.UDPAddr).Port, fh, info.Size, io.Discard, nfsReadWindow); err == nil || time.Since(start) > time.Second {
		t.Fatalf("silent server did not exhaust bounded retries: %v after %s", err, time.Since(start))
	}
}
