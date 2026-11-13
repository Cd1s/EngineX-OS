package broadcast

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// BH-GF4-7: a Shoutcast v1 server accepts the password with "OK", not "OK2"; the deck refused it as
// an authentication failure. A refusal must still be one.
func TestShoutcastHandshakeAcceptsV1AndV2(t *testing.T) {
	for _, c := range []struct {
		reply string
		ok    bool
	}{
		{"OK\r\n", true},
		{"OK2\r\nicy-caps:11\r\n\r\n", true},
		{"invalid password\r\n", false},
		{"Invalid Password\r\nOK2\r\n", false},
		{"OKAY\r\n", false},
	} {
		t.Run(strings.TrimSpace(c.reply), func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			headers := make(chan string, 1)
			go func() {
				defer server.Close()
				r := bufio.NewReader(server)
				if _, err := r.ReadString('\n'); err != nil { // the password line
					headers <- ""
					return
				}
				io.WriteString(server, c.reply)
				server.SetReadDeadline(time.Now().Add(time.Second))
				var b strings.Builder
				for {
					line, err := r.ReadString('\n')
					b.WriteString(line)
					if err != nil || line == "\r\n" {
						break
					}
				}
				headers <- b.String()
			}()
			b := New(newTestGuard())
			defer b.Close()
			err := b.handshakeShoutcast(client, Config{Protocol: "shoutcast", Password: "pw", StreamName: "deck", Bitrate: 128, Format: "mp3"})
			if c.ok {
				if err != nil {
					t.Fatalf("handshake refused %q: %v", c.reply, err)
				}
				if h := <-headers; !strings.Contains(h, "icy-name:deck\r\n") {
					t.Fatalf("icy headers not sent after %q: %q", c.reply, h)
				}
			} else if err == nil || !strings.Contains(err.Error(), "authentication failed") {
				t.Fatalf("reply %q: err = %v, want an authentication failure", c.reply, err)
			}
		})
	}
}
