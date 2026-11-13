package broadcast

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// Live streaming is WAV only: no deck image ships an MP3 or Ogg encoder, so a saved
// "mp3" (the old default) used to end in "no encoder" and never connect. Settings
// from older versions or clients that still say mp3 or ogg must push WAV instead.
func TestOldFormatSettingsPushWAV(t *testing.T) {
	for _, format := range []string{"mp3", "MP3", "ogg"} {
		t.Run(format, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			type pushed struct {
				headers string
				wav     []byte
			}
			got := make(chan pushed, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				r := bufio.NewReader(conn)
				var h strings.Builder
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					h.WriteString(line)
					if line == "\r\n" {
						break
					}
				}
				_, _ = io.WriteString(conn, "HTTP/1.0 200 OK\r\n\r\n")
				wav := make([]byte, 44)
				if _, err := io.ReadFull(r, wav); err == nil {
					got <- pushed{h.String(), wav}
				}
			}()

			b := New(newTestGuard())
			defer b.Close()
			if err := b.Configure(Config{Enabled: true, ServerURL: "http://" + ln.Addr().String(), Password: "x", Format: format}); err != nil {
				t.Fatal(err)
			}
			select {
			case p := <-got:
				if !strings.Contains(p.headers, "Content-Type: audio/wav\r\n") || !strings.Contains(p.headers, "Ice-Bitrate: 1536\r\n") {
					t.Errorf("handshake does not describe a 1536 kbps WAV stream:\n%s", p.headers)
				}
				if string(p.wav[:4]) != "RIFF" || string(p.wav[8:12]) != "WAVE" {
					t.Errorf("stream does not open with a WAV header: %q", p.wav[:12])
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("nothing was pushed, status %+v", b.Status())
			}
			if st := b.Status(); st.Format != "wav" || st.LastError != "" {
				t.Errorf("status %+v, want format wav and no error", st)
			}
		})
	}
}
