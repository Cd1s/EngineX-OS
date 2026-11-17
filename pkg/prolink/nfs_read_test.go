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

type gatedNFSWriter struct {
	bytes.Buffer
	once            sync.Once
	started, resume chan struct{}
}

func (w *gatedNFSWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started); <-w.resume })
	return w.Buffer.Write(p)
}

func TestNFSReadPipelineConcurrentPrivilegedLookup(t *testing.T) {
	var source int
	for source = 1023; source > 1000; source-- {
		probe, err := net.ListenUDP("udp4", &net.UDPAddr{Port: source})
		if err == nil {
			probe.Close()
			break
		}
	}
	if source == 1000 {
		t.Skip("no available privileged UDP test port")
	}
	root := t.TempDir()
	want := bytes.Repeat([]byte("audio"), 6000)
	os.WriteFile(filepath.Join(root, "a.mp3"), want, 0600)
	s := &NFSServer{Root: root}
	if err := s.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	secure, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64<<10)
		for {
			n, from, err := secure.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if from.Port >= 1024 {
				continue
			}
			secure.WriteToUDP(s.handle(buf[:n], uint32(secure.LocalAddr().(*net.UDPAddr).Port)), from)
		}
	}()
	defer func() { secure.Close(); <-done }()
	c := &NFSClient{IP: net.IPv4(127, 0, 0, 1), Portmap: secure.LocalAddr().(*net.UDPAddr).Port, SourcePort: source, Timeout: 30 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	w := &gatedNFSWriter{started: make(chan struct{}), resume: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(w.resume) }) }
	defer release()
	finished := make(chan error, 1)
	go func() { _, err := c.FetchTo(ctx, "/C/", "/a.mp3", w, 1<<20); finished <- err }()
	select {
	case <-w.started:
	case err := <-finished:
		t.Fatalf("first copy never started: %v", err)
	case <-ctx.Done():
		t.Fatal("first copy stalled")
	}
	_, info, lookupErr := c.Lookup(ctx, "/C/", "/a.mp3")
	release()
	if err := <-finished; err != nil {
		t.Fatalf("first copy: %v", err)
	}
	if lookupErr != nil || info.Size != uint32(len(want)) {
		t.Fatalf("concurrent privileged lookup: %+v %v", info, lookupErr)
	}
	if !bytes.Equal(w.Bytes(), want) {
		t.Fatal("copy bytes changed")
	}
}
