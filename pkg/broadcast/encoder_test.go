package broadcast

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The tests never depend on lame/ffmpeg on the host: the test binary itself stands in
// for the MP3 encoder (TestMain), emitting one frame-synced 48 kHz frame per PCM frame read.
func init() {
	mp3EncoderFn = func(ctx context.Context, _ int) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0])
		cmd.Env = append(os.Environ(), "ENGINEX_FAKE_MP3_ENCODER=1")
		return groupKill(cmd)
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("ENGINEX_FAKE_MP3_ENCODER") == "1" {
		frame := make([]byte, 144*192000/SampleRate)
		copy(frame, []byte{0xFF, 0xFB, 0xB4, 0x00}) // MPEG-1 Layer III, 192 kbit/s, 48 kHz
		pcm := make([]byte, 1152*4)
		for {
			if _, err := io.ReadFull(os.Stdin, pcm); err != nil {
				os.Exit(0)
			}
			if _, err := os.Stdout.Write(frame); err != nil {
				os.Exit(0)
			}
		}
	}
	os.Exit(m.Run())
}

// BH-GF4-8: no deck image ships an MP3 encoder. Without one the push must not connect
// and send hash-filled pseudo frames (the old fallback); it reports what works instead.
func TestPushWithoutEncoderSendsNothing(t *testing.T) {
	old := mp3EncoderFn
	defer func() { mp3EncoderFn = old }()
	mp3EncoderFn = func(context.Context, int) *exec.Cmd { return nil }
	for _, format := range []string{"mp3", "ogg"} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		connected := make(chan struct{}, 1)
		go func() {
			if conn, err := ln.Accept(); err == nil {
				connected <- struct{}{}
				conn.Close()
			}
		}()
		b := New(newTestGuard())
		if err := b.Configure(Config{Enabled: true, ServerURL: "http://" + ln.Addr().String(), Password: "x", Format: format}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for b.Status().LastError == "" && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		st := b.Status()
		if !strings.Contains(st.LastError, "wav") || !strings.Contains(st.LastError, "/live.wav") || st.Streaming {
			t.Errorf("%s: status %+v, want a clear error pointing to wav", format, st)
		}
		select {
		case <-connected:
			t.Errorf("%s: connected to the server without an encoder", format)
		case <-time.After(200 * time.Millisecond):
		}
		b.Close()
		ln.Close()
	}
}

// With an encoder available, the server must receive the encoder's bytes, not placeholder frames.
// "cat" stands in for lame/ffmpeg: PCM in, same bytes out.
func TestStreamUsesExternalMP3Encoder(t *testing.T) {
	catPath, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("no cat")
	}
	old := mp3EncoderFn
	defer func() { mp3EncoderFn = old }()
	mp3EncoderFn = func(ctx context.Context, _ int) *exec.Cmd {
		return groupKill(exec.CommandContext(ctx, catPath))
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if strings.TrimSpace(line) == "" {
				break
			}
		}
		_, _ = io.WriteString(conn, "HTTP/1.0 200 OK\r\n\r\n")
		buf := make([]byte, 4608)
		if _, err := io.ReadFull(r, buf); err == nil {
			got <- buf
		}
	}()

	b := New(newTestGuard())
	defer b.Close()
	if err := b.Configure(Config{Enabled: true, ServerURL: "http://" + ln.Addr().String(), Password: "x", Format: "mp3"}); err != nil {
		t.Fatal(err)
	}
	select {
	case buf := <-got:
		if bytes.Equal(buf[:2], []byte{0xFF, 0xFB}) {
			t.Fatal("received placeholder MP3 frame instead of encoder output")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for encoder output")
	}
	if err := b.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
}

// The server hangs up right after the handshake: streamOnce must return promptly
// instead of blocking on the encoder's full stdin pipe.
func TestStreamEncodedMP3ServerCloseDoesNotHang(t *testing.T) {
	catPath, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("no cat")
	}
	old := mp3EncoderFn
	defer func() { mp3EncoderFn = old }()
	mp3EncoderFn = func(ctx context.Context, _ int) *exec.Cmd {
		return groupKill(exec.CommandContext(ctx, catPath))
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			if err != nil || strings.TrimSpace(line) == "" {
				break
			}
		}
		_, _ = io.WriteString(conn, "HTTP/1.0 200 OK\r\n\r\n")
		conn.Close()
	}()

	b := New(newTestGuard())
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	feed := make(chan []byte)
	go func() { // keep feeding PCM so the encoder has output to write into the dead socket
		pcm := make([]byte, 4608)
		for {
			select {
			case feed <- pcm:
			case <-ctx.Done():
				return
			}
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := b.streamOnce(ctx, Config{ServerURL: "http://" + ln.Addr().String(), Password: "x", Format: "mp3"}, feed)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error after the server closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("streamOnce hung after the server closed the connection")
	}
}
