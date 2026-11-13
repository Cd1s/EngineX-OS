package webui

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Live Stream is WAV only. A broadcast-config.json saved by an older version says
// "mp3" and a bitrate; it must load as WAV and start, not stop at "no MP3 encoder".
func TestSavedMP3ConfigLoadsAsWAVAndStarts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
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
			got <- h.String() + string(wav[:4])
		}
	}()

	s, stateDir := newTestServer(t)
	saved := `{"enabled":true,"protocol":"icecast","serverUrl":"http://` + ln.Addr().String() + `","password":"x","format":"mp3","bitrate":320}`
	if err := os.WriteFile(filepath.Join(stateDir, "broadcast-config.json"), []byte(saved), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.SetBroadcastEnabled(false) })
	s.broadcaster() // loads the saved settings, as the first request after a boot does

	select {
	case p := <-got:
		if !strings.Contains(p, "Content-Type: audio/wav\r\n") || !strings.HasSuffix(p, "RIFF") {
			t.Errorf("pushed %q, want a WAV stream", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("saved mp3 config did not start: %+v", s.broadcaster().Status())
	}
	if st := s.broadcaster().Status(); st.Format != "wav" || st.LastError != "" {
		t.Errorf("status %+v, want format wav and no error", st)
	}
}

// An older client that still posts format and bitrate is not rejected, and the API
// has no bitrate to offer any more.
func TestBroadcastAPIHasNoBitrateAndIgnoresOldClients(t *testing.T) {
	s, _ := newTestServer(t)
	body := []byte(`{"format":"mp3","bitrate":320,"streamName":"Old client"}`)
	resp := doAuthRequest(t, s, "POST", "/api/broadcast", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("old client rejected with %d", resp.StatusCode)
	}
	for _, r := range []*http.Response{resp, doAuthRequest(t, s, "GET", "/api/broadcast", nil)} {
		var st map[string]any
		if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
			t.Fatal(err)
		}
		if _, ok := st["bitrate"]; ok || st["format"] != "wav" || st["streamName"] != "Old client" {
			t.Errorf("status %v, want format wav, no bitrate, and the posted stream name", st)
		}
	}
}

// The console card shows the fixed WAV format and says how the push relates to
// /live.wav; MP3, Ogg, bitrate and Shoutcast are gone.
func TestConsoleBroadcastCardIsWAVOnly(t *testing.T) {
	b, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	for _, gone := range []string{`id="bcFmt"`, `id="bcBr"`, `id="bcProto"`, `kbps</option>`, `value="mp3"`, `value="shoutcast"`} {
		if strings.Contains(page, gone) {
			t.Errorf("console still offers %s", gone)
		}
	}
	for _, key := range []string{"bcFormat", "bcRelay"} {
		if !strings.Contains(page, `data-i18n="`+key+`"`) {
			t.Errorf("%s is not shown on the card", key)
		}
		m := regexp.MustCompile(key + `:\[('(?:[^'\\]|\\.)*'),('(?:[^'\\]|\\.)*'),('(?:[^'\\]|\\.)*')\]`).FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("%s is not in all three languages", key)
		}
		for _, text := range m[1:] {
			if key == "bcFormat" && (!strings.Contains(text, "WAV") || !strings.Contains(text, "1536")) {
				t.Errorf("%s: %s does not state the fixed WAV format", key, text)
			}
		}
	}
}
