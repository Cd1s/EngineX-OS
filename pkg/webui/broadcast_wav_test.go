package webui

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/broadcast"
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

// An older client that still posts format and bitrate is not rejected: the format follows the
// protocol (WAV for Icecast) and the bitrate is the Shoutcast AAC one, the default when the
// client's MP3 choice is not offered. (Until 5.3.0.6 the API had no bitrate at all.)
func TestBroadcastAPIAcceptsOldClients(t *testing.T) {
	s, _ := newTestServer(t)
	for _, c := range []struct {
		posted, want int
	}{{320, 320}, {256, 192}} {
		body := []byte(`{"format":"mp3","bitrate":` + strconv.Itoa(c.posted) + `,"streamName":"Old client"}`)
		resp := doAuthRequest(t, s, "POST", "/api/broadcast", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("old client rejected with %d", resp.StatusCode)
		}
		for _, r := range []*http.Response{resp, doAuthRequest(t, s, "GET", "/api/broadcast", nil)} {
			var st map[string]any
			if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
				t.Fatal(err)
			}
			if st["bitrate"] != float64(c.want) || st["format"] != "wav" || st["streamName"] != "Old client" {
				t.Errorf("posted %d: status %v, want format wav, bitrate %d, and the posted stream name", c.posted, st, c.want)
			}
		}
	}
}

// An Icecast server forwards the WAV header only to the listeners who join at the
// start of the push; a later one gets bare PCM and the player guesses the wrong codec.
// The deck serves the header, with the same streaming sizes as /live.wav, so it can be
// set as the mount's <intro> (checked against Icecast 2.4.4). It needs no login and
// no Live Stream.
func TestLiveHeaderIsTheStreamingWAVHeader(t *testing.T) {
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/live-header.wav", nil))
	sum := sha256.Sum256(rec.Body.Bytes())
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "audio/wav" || rec.Body.Len() != 44 ||
		hex.EncodeToString(sum[:]) != "980639bb629bc7f075641e7e1be2584784bba82ac062c73dc2f7387818db00f1" {
		t.Fatalf("/live-header.wav: %d %q, %d bytes, sha256 %x; want the 44-byte PCM s16le 48 kHz stereo streaming header",
			rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len(), sum)
	}
}

// A Shoutcast target saved by an older version, with Live Stream on, used to be
// connected to, sent a WAV stream it cannot take, and retried for ever. It now gets AAC
// (pkg/broadcast tests); on a deck without the AAC encoder nothing is pushed, the status
// says why, and /live.wav keeps working. (Until 5.3.0.6 no Shoutcast target was pushed to.)
func TestSavedShoutcastConfigWithoutAnEncoderIsNotPushed(t *testing.T) {
	t.Setenv("ENGINEX_AACENC", filepath.Join(t.TempDir(), "enginex-aacenc")) // not there
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	listenerPort := ln.Addr().(*net.TCPAddr).Port - 1 // the source port is the next one up
	connected := make(chan struct{}, 1)
	go func() {
		if conn, err := ln.Accept(); err == nil {
			connected <- struct{}{}
			conn.Close()
		}
	}()

	s, stateDir := newTestServer(t)
	saved := `{"enabled":true,"protocol":"shoutcast","serverUrl":"127.0.0.1:` + strconv.Itoa(listenerPort) + `","password":"x","format":"mp3","bitrate":192}`
	if err := os.WriteFile(filepath.Join(stateDir, "broadcast-config.json"), []byte(saved), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.SetBroadcastEnabled(false) })
	s.broadcaster() // loads the saved settings, as the first request after a boot does

	select {
	case <-connected:
		t.Fatal("pushed to a Shoutcast server")
	case <-time.After(500 * time.Millisecond):
	}
	var st broadcast.Status
	if err := json.NewDecoder(doAuthRequest(t, s, "GET", "/api/broadcast", nil).Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || st.Streaming || st.Protocol != "shoutcast" || st.Format != "aac" || st.Bitrate != 192 || !strings.Contains(st.LastError, "AAC encoder unavailable") {
		t.Errorf("status %+v, want the saved Shoutcast target kept as AAC at 192 kbps, not streaming, and the encoder error", st)
	}

	srv := httptest.NewServer(s)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/live.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	head := make([]byte, 12)
	if _, err := io.ReadFull(resp.Body, head); err != nil || resp.StatusCode != 200 || string(head[:4]) != "RIFF" {
		t.Errorf("/live.wav: %d %q %v, want the WAV stream", resp.StatusCode, head, err)
	}
}

// The console card offers the two modes: Icecast with the fixed WAV format, and Shoutcast with
// AAC at 128, 192 (default) or 320 kbps. MP3 and Ogg stay gone. Only the Save button sets the
// protocol and bitrate: Start and Stop leave them alone. The card title ("Shoutcast / Icecast")
// is true again, and status and errors of the Shoutcast mode are in all four languages.
// (Until 5.3.0.6 the card was WAV only: TestConsoleBroadcastCardIsWAVOnly.)
func TestConsoleBroadcastCardOffersIcecastWAVAndShoutcastAAC(t *testing.T) {
	b, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	for _, gone := range []string{`id="bcFmt"`, `value="mp3"`, `value="ogg"`} {
		if strings.Contains(page, gone) {
			t.Errorf("console still offers %s", gone)
		}
	}
	for _, want := range []string{`<option value="icecast" data-i18n="bcProtoIce">`, `<option value="shoutcast" data-i18n="bcProtoSc">`,
		`<option value="128">128 kbps</option><option value="192">192 kbps</option><option value="320">320 kbps</option></select>`} {
		if !strings.Contains(page, want) {
			t.Errorf("console lacks %s", want)
		}
	}
	if strings.Count(page, "kbps</option>") != 3 {
		t.Error("the bitrate choice must be exactly 128, 192 and 320 kbps")
	}
	if !strings.Contains(page, "post('broadcast',{...bcForm(),protocol:$('bcProto').value,bitrate:+$('bcBr').value})") {
		t.Error("Save should send the chosen protocol and bitrate")
	}
	toggle := page[strings.Index(page, "async function toggleBroadcast"):]
	toggle = toggle[:strings.Index(toggle, "\n")]
	form := page[strings.Index(page, "function bcForm"):]
	form = form[:strings.Index(form, "\n")]
	if strings.Contains(toggle+form, "protocol") || strings.Contains(toggle+form, "bitrate") {
		t.Error("Start and Stop must leave the protocol and bitrate alone")
	}
	for key, want := range map[string]string{"bcTitle": "Shoutcast / Icecast", "bcFormat": "1536", "bcFormatAAC": "AAC-LC", "bcRelay": "AAC",
		"bcIntro": "live-header.wav", "bcShoutcast": "+ 1", "bcProtoSc": "AAC", "bcProtoIce": "WAV", "bcBr": "AAC",
		"bcDropped": "{0}", "bcEncMissing": "AAC", "bcEncFailed": "AAC", "bcScAuth": "Shoutcast"} {
		if !strings.Contains(page, `data-i18n="`+key+`"`) && !strings.Contains(page, "t('"+key+"'") && !strings.Contains(page, "'"+key+"'") {
			t.Errorf("%s is not shown on the card", key)
		}
		m := regexp.MustCompile(key + `:\[('(?:[^'\\]|\\.)*'),('(?:[^'\\]|\\.)*'),('(?:[^'\\]|\\.)*'),('(?:[^'\\]|\\.)*')\]`).FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("%s is not in all languages", key)
		}
		for _, text := range m[1:] {
			if !strings.Contains(text, want) {
				t.Errorf("%s: %s does not mention %s", key, text, want)
			}
		}
	}
	if strings.Contains(page, "Shoutcast is not supported") || strings.Contains(page, "Shoutcast 不受支持") {
		t.Error("the console still says Shoutcast is not supported")
	}
}
