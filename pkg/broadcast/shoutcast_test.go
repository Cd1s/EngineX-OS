package broadcast

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as a fake enginex-aacenc (ENGINEX_FAKE_AACENC=<mode>): it frames its
// input as ADTS at the asked bitrate, so the push path is tested without the deck's libavcodec.
// With ENGINEX_AACENC set (the build host, with the stock libraries), the real encoder runs too.
func TestMain(m *testing.M) {
	if mode := os.Getenv("ENGINEX_FAKE_AACENC"); mode != "" {
		os.Exit(fakeAACEnc(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeAACEnc behaves like enginex-aacenc: "ok" writes one frame per 1024 input samples, "slow"
// takes 300 ms per frame (14x slower than real time), "flood" writes 500 frames per input frame
// (a server that cannot keep up), "missing" fails its check like a deck without libavcodec.
func fakeAACEnc(mode string, args []string) int {
	if len(args) == 1 && args[0] == "--check" {
		if mode == "missing" {
			fmt.Fprintln(os.Stderr, "enginex-aacenc: no stock AAC encoder: libavcodec.so.58: cannot open shared object file: No such file or directory")
			return 3
		}
		return 0
	}
	kbps, err := strconv.Atoi(args[0])
	if err != nil {
		return 2
	}
	if dir := os.Getenv("ENGINEX_FAKE_AACENC_DIR"); dir != "" { // lets the test see which encoders live
		_ = os.WriteFile(filepath.Join(dir, strconv.Itoa(os.Getpid())), nil, 0o644)
	}
	payload := kbps*1000/8*1024/SampleRate - 7
	frame := make([]byte, 7+payload)
	adtsHeader(frame, len(frame))
	in := make([]byte, 1024*4)
	for {
		if _, err := io.ReadFull(os.Stdin, in); err != nil {
			return 0
		}
		n := 1
		switch mode {
		case "slow":
			time.Sleep(300 * time.Millisecond)
		case "flood":
			n = 500
		}
		for i := 0; i < n; i++ {
			if _, err := os.Stdout.Write(frame); err != nil {
				return 1
			}
		}
	}
}

// adtsHeader writes the header enginex-aacenc writes: MPEG-4 AAC LC, 48 kHz, stereo, no CRC.
func adtsHeader(h []byte, n int) {
	h[0], h[1] = 0xff, 0xf1
	h[2] = 1<<6 | 3<<2
	h[3] = 2<<6 | byte(n>>11)&3
	h[4] = byte(n >> 3)
	h[5] = byte(n&7)<<5 | 0x1f
	h[6] = 0xfc
}

// encoderStarts sees the fake encoders a test started: each one leaves its PID in dir.
type encoderStarts struct{ dir string }

func newEncoderStarts(t *testing.T) *encoderStarts { return &encoderStarts{dir: t.TempDir()} }

func (e *encoderStarts) command(mode string) encoderCommand {
	return func(args ...string) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], args...)
		cmd.Env = append(os.Environ(), "ENGINEX_FAKE_AACENC="+mode, "ENGINEX_FAKE_AACENC_DIR="+e.dir)
		return cmd, nil
	}
}

// running counts the encoders started (checks excluded) and those not yet reaped: stop waits
// for its encoder, so a reaped PID no longer answers signal 0.
func (e *encoderStarts) running() (started, running int) {
	entries, _ := os.ReadDir(e.dir)
	for _, ent := range entries {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		started++
		if syscall.Kill(pid, 0) == nil {
			running++
		}
	}
	return started, running
}

// fakeShoutcast is a SHOUTcast v1 source port: it answers the login and collects the stream.
type fakeShoutcast struct {
	ln        net.Listener
	serverURL string // the listener port the console is given: the source port minus one
	sessions  chan *scSession
}

type scSession struct {
	login   string
	headers string
	r       *bufio.Reader
	conn    net.Conn
}

func newFakeShoutcast(t *testing.T, reply string) *fakeShoutcast {
	t.Helper()
	var ln net.Listener
	var err error
	for i := 0; i < 20; i++ { // the listener port (source port - 1) must be a valid port
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		if ln.Addr().(*net.TCPAddr).Port > 1 {
			break
		}
		ln.Close()
	}
	f := &fakeShoutcast{ln: ln, sessions: make(chan *scSession, 4),
		serverURL: "http://127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port-1)}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s := &scSession{conn: conn, r: bufio.NewReader(conn)}
			if s.login, err = s.r.ReadString('\n'); err != nil {
				conn.Close()
				continue
			}
			_, _ = io.WriteString(conn, reply)
			if !strings.HasPrefix(reply, "OK") {
				conn.Close()
				continue
			}
			for {
				line, err := s.r.ReadString('\n')
				s.headers += line
				if err != nil || line == "\r\n" {
					break
				}
			}
			f.sessions <- s
		}
	}()
	t.Cleanup(func() {
		for {
			select {
			case s := <-f.sessions:
				s.conn.Close()
			default:
				return
			}
		}
	})
	return f
}

func (f *fakeShoutcast) next(t *testing.T, b *Broadcaster) *scSession {
	t.Helper()
	select {
	case s := <-f.sessions:
		return s
	case <-time.After(5 * time.Second):
		t.Fatalf("no SHOUTcast source connected, status %+v", b.Status())
		return nil
	}
}

// adtsFrames reads n ADTS frames and checks each is AAC LC, 48 kHz, stereo.
func adtsFrames(t *testing.T, r *bufio.Reader, n int) (bytes int) {
	t.Helper()
	for i := 0; i < n; i++ {
		frame, err := readADTS(r)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		profile, rate, ch := frame[2]>>6, frame[2]>>2&15, (frame[2]&1)<<2|frame[3]>>6
		if profile != 1 || rate != 3 || ch != 2 {
			t.Fatalf("frame %d: profile %d rate index %d channels %d, want AAC LC (1), 48 kHz (3), stereo (2)", i, profile, rate, ch)
		}
		bytes += len(frame)
	}
	return bytes
}

// #138: a saved SHOUTcast target with Live Stream on gets AAC (never WAV) at the chosen bitrate,
// with a v1 login (password) or a v2 one (password:#stream ID).
func TestShoutcastPushesAACAtTheChosenBitrate(t *testing.T) {
	for _, c := range []struct {
		user, login string
		kbps        int
	}{
		{"source", "pw\r\n", 128},
		{"", "pw\r\n", 192},
		{"2", "pw:#2\r\n", 320},
		{"#1", "pw:#1\r\n", 0}, // no choice: the default, 192
	} {
		t.Run(fmt.Sprintf("%s-%d", c.user, c.kbps), func(t *testing.T) {
			f := newFakeShoutcast(t, "OK2\r\nicy-caps:11\r\n\r\n")
			enc := newEncoderStarts(t)
			b := New(newTestGuard())
			b.encoderCmd = enc.command("ok")
			defer b.Close()
			if err := b.Configure(Config{Enabled: true, Protocol: "shoutcast", ServerURL: f.serverURL, Username: c.user,
				Password: "pw", StreamName: "Club", Bitrate: c.kbps}); err != nil {
				t.Fatal(err)
			}
			want := c.kbps
			if want == 0 {
				want = DefaultBitrate
			}
			s := f.next(t, b)
			defer s.conn.Close()
			if s.login != c.login {
				t.Errorf("login line %q, want %q", s.login, c.login)
			}
			for _, h := range []string{"icy-name:Club\r\n", "content-type:audio/aacp\r\n", fmt.Sprintf("icy-br:%d\r\n", want)} {
				if !strings.Contains(s.headers, h) {
					t.Errorf("headers %q lack %q", s.headers, h)
				}
			}
			if head, err := s.r.Peek(4); err != nil || string(head) == "RIFF" {
				t.Fatalf("stream opens with %q (%v), want ADTS", head, err)
			}
			const frames = 47 // about one second
			got := adtsFrames(t, s.r, frames)
			kbps := float64(got*8) / (frames * 1024.0 / SampleRate) / 1000
			if kbps < float64(want)*0.9 || kbps > float64(want)*1.1 {
				t.Errorf("stream is %.1f kbps, want %d", kbps, want)
			}
			st := b.Status()
			if st.Protocol != "shoutcast" || st.Format != "aac" || st.Bitrate != want || !st.Streaming || st.LastError != "" {
				t.Errorf("status %+v, want shoutcast, aac at %d kbps, streaming, no error", st, want)
			}
		})
	}
}

// The encoder runs only while a SHOUTcast push is up: not for /live.wav alone, not for an Icecast
// push, and it is gone once Live Stream is switched off.
func TestEncoderRunsOnlyWhileShoutcastPushes(t *testing.T) {
	f := newFakeShoutcast(t, "OK2\r\n")
	enc := newEncoderStarts(t)
	b := New(newTestGuard())
	b.encoderCmd = enc.command("ok")
	defer b.Close()

	ice, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ice.Close()
	go func() {
		for {
			conn, err := ice.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.WriteString(conn, "HTTP/1.0 200 OK\r\n\r\n")
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()
	for _, cfg := range []Config{{Enabled: true}, {Enabled: true, ServerURL: ice.Addr().String(), Password: "x"}} {
		if err := b.Configure(cfg); err != nil {
			t.Fatal(err)
		}
		pcm, cancel := b.Subscribe() // after Configure: a restart closes the listeners
		select {
		case _, ok := <-pcm:
			if !ok {
				t.Fatal("the local stream was closed")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the local stream does not flow")
		}
		cancel()
		time.Sleep(300 * time.Millisecond)
		if started, _ := enc.running(); started != 0 {
			t.Fatalf("an encoder started for %+v", cfg)
		}
	}

	if err := b.Configure(Config{Enabled: true, Protocol: "shoutcast", ServerURL: f.serverURL, Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	s := f.next(t, b)
	adtsFrames(t, s.r, 5)
	if started, running := enc.running(); started != 1 || running != 1 {
		t.Fatalf("encoders started %d, running %d; want one running", started, running)
	}
	if err := b.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if _, running := enc.running(); running != 0 {
		t.Fatalf("%d encoder(s) still running after Live Stream was switched off", running)
	}
}

// A dropped server connection ends that session's encoder and the push logs in again.
func TestShoutcastReconnectsAfterTheServerDrops(t *testing.T) {
	f := newFakeShoutcast(t, "OK2\r\n")
	enc := newEncoderStarts(t)
	b := New(newTestGuard())
	b.encoderCmd = enc.command("ok")
	defer b.Close()
	if err := b.Configure(Config{Enabled: true, Protocol: "shoutcast", ServerURL: f.serverURL, Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	first := f.next(t, b)
	adtsFrames(t, first.r, 10)
	first.conn.Close() // the server goes away

	second := f.next(t, b)
	defer second.conn.Close()
	adtsFrames(t, second.r, 10)
	if started, running := enc.running(); started != 2 || running != 1 {
		t.Errorf("encoders started %d, running %d; want a new one for the new session and the old one gone", started, running)
	}
	if st := b.Status(); !st.Streaming {
		t.Errorf("status %+v, want streaming again", st)
	}
}

// A server that reads far slower than the stream: encoded frames are dropped once the queue
// is full, memory stays flat, and the encoder is never made to wait on the network.
func TestSlowShoutcastServerDoesNotGrowMemory(t *testing.T) {
	f := newFakeShoutcast(t, "OK2\r\n")
	enc := newEncoderStarts(t)
	b := New(newTestGuard())
	b.encoderCmd = enc.command("flood") // 50x real time: fills the socket and the queue at once
	defer b.Close()
	if err := b.Configure(Config{Enabled: true, Protocol: "shoutcast", ServerURL: f.serverURL, Password: "pw", Bitrate: 320}); err != nil {
		t.Fatal(err)
	}
	s := f.next(t, b)
	defer s.conn.Close()
	stopRead := make(chan struct{})
	defer close(stopRead)
	go func() { // 1 KB every 50 ms: 160 kbps for a 320 kbps x 50 stream
		buf := make([]byte, 1024)
		for {
			select {
			case <-stopRead:
				return
			case <-time.After(50 * time.Millisecond):
			}
			if _, err := s.r.Read(buf); err != nil {
				return
			}
		}
	}()
	heap := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	time.Sleep(time.Second)
	before := heap()
	time.Sleep(2 * time.Second)
	after := heap()
	if after > before+4<<20 {
		t.Errorf("heap grew from %d to %d bytes while the server was slow", before, after)
	}
	if st := b.Status(); st.Dropped == 0 || !st.Streaming {
		t.Errorf("status %+v, want frames dropped and the session still up", st)
	}
}

// An encoder slower than real time (a deck too busy) never holds up capture: /live.wav keeps its
// pace and the audio meant for the encoder is dropped instead.
func TestLaggingEncoderDropsInsteadOfBlocking(t *testing.T) {
	f := newFakeShoutcast(t, "OK2\r\n")
	enc := newEncoderStarts(t)
	b := New(newTestGuard())
	b.encoderCmd = enc.command("slow")
	defer b.Close()
	if err := b.Configure(Config{Enabled: true, Protocol: "shoutcast", ServerURL: f.serverURL, Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	pcm, cancel := b.Subscribe() // after Configure: a restart closes the listeners
	defer cancel()
	s := f.next(t, b)
	defer s.conn.Close()
	go func() { _, _ = io.Copy(io.Discard, s.r) }()

	for drained := false; !drained; { // what queued for the listener while connecting
		select {
		case <-pcm:
		default:
			drained = true
		}
	}
	start, bytes := time.Now(), 0
	for time.Since(start) < 4*time.Second {
		select {
		case p, ok := <-pcm:
			if !ok {
				t.Fatal("the local stream was closed")
			}
			bytes += len(p)
		case <-time.After(500 * time.Millisecond):
			t.Fatal("the local stream stalled behind the encoder")
		}
	}
	if secs := float64(bytes) / (SampleRate * 4); secs < 3.5 {
		t.Errorf("/live.wav got %.2f s of audio in 4 s while the encoder lagged", secs)
	}
	if st := b.Status(); st.Dropped == 0 {
		t.Errorf("status %+v, want the encoder's audio dropped", st)
	}
}

// A deck without the stock encoder never logs in to the SHOUTcast server (it must never get
// WAV), says why, and /live.wav keeps working. (Before 5.3.0.6 every SHOUTcast target was
// treated like this: TestShoutcastTargetIsNotPushed.)
func TestShoutcastTargetIsNotPushedWithoutAnEncoder(t *testing.T) {
	f := newFakeShoutcast(t, "OK2\r\n")
	enc := newEncoderStarts(t)
	b := New(newTestGuard())
	b.encoderCmd = enc.command("missing")
	defer b.Close()
	if err := b.Configure(Config{Enabled: true, Protocol: "shoutcast", ServerURL: f.serverURL, Password: "shoutpwd"}); err != nil {
		t.Fatal(err)
	}
	pcm, cancel := b.Subscribe()
	defer cancel()
	select {
	case _, ok := <-pcm:
		if !ok {
			t.Fatal("the local stream was closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the local stream does not flow")
	}
	select {
	case <-f.sessions:
		t.Fatal("logged in to the SHOUTcast server without an encoder")
	case <-time.After(500 * time.Millisecond):
	}
	if st := b.Status(); st.Streaming || !strings.Contains(st.LastError, "AAC encoder unavailable") || !strings.Contains(st.LastError, "libavcodec") {
		t.Errorf("status %+v, want not streaming and the encoder's reason", st)
	}
	if started, _ := enc.running(); started != 0 {
		t.Errorf("%d encoder(s) started after a failed check", started)
	}
}

// BH-GF4-7: a Shoutcast v1 server accepts the password with "OK", not "OK2"; the deck refused it as
// an authentication failure. A refusal must still be one. (Restored with the SHOUTcast mode.)
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
				_, _ = io.WriteString(server, c.reply)
				_ = server.SetReadDeadline(time.Now().Add(time.Second))
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
			err := b.handshakeShoutcast(client, Config{Protocol: "shoutcast", Password: "pw", StreamName: "deck", Bitrate: 192})
			if c.ok {
				if err != nil {
					t.Fatalf("handshake refused %q: %v", c.reply, err)
				}
				if h := <-headers; !strings.Contains(h, "icy-name:deck\r\n") || !strings.Contains(h, "icy-br:192\r\n") {
					t.Fatalf("icy headers not sent after %q: %q", c.reply, h)
				}
			} else if err == nil || !strings.Contains(err.Error(), "authentication failed") {
				t.Fatalf("reply %q: err = %v, want an authentication failure", c.reply, err)
			}
		})
	}
}

func TestShoutcastLoginUsesStreamID(t *testing.T) {
	for _, c := range []struct{ user, want string }{
		{"source", "pw"}, {"", "pw"}, {"admin", "pw"}, {"2", "pw:#2"}, {"#1", "pw:#1"}, {" #3 ", "pw:#3"},
	} {
		if got := shoutcastLogin(Config{Username: c.user, Password: "pw"}); got != c.want {
			t.Errorf("user %q: got %q want %q", c.user, got, c.want)
		}
	}
}

// BH2-A3-7 for the SHOUTcast handshake: pasted line breaks do not split its lines.
func TestShoutcastSettingsStayOnTheirHandshakeLine(t *testing.T) {
	handshake := func(cfg Config) string {
		t.Helper()
		cfg.Protocol = "shoutcast"
		if err := normalizeConfig(&cfg); err != nil {
			t.Fatal(err)
		}
		deck, server := net.Pipe()
		defer deck.Close()
		got := make(chan string, 1)
		go func() {
			defer server.Close()
			r := bufio.NewReader(server)
			all, _ := r.ReadString('\n')
			_, _ = io.WriteString(server, "OK2\r\n")
			for {
				line, err := r.ReadString('\n')
				all += line
				if err != nil || line == "\r\n" {
					break
				}
			}
			got <- all
		}()
		b := New(newTestGuard())
		defer b.Close()
		_ = deck.SetDeadline(time.Now().Add(3 * time.Second))
		if err := b.handshakeShoutcast(deck, cfg); err != nil {
			t.Fatalf("handshake: %v", err)
		}
		return <-got
	}
	plain := Config{Username: "2", Password: "pw", StreamName: "Friday Night Mix"}
	pasted := Config{Username: "2\r\n", Password: "pw\r\n", StreamName: "Friday\r\n Night\x00 Mix\n"}
	if want, got := handshake(plain), handshake(pasted); got != want {
		t.Errorf("settings pasted with line breaks changed the handshake:\n got %q\nwant %q", got, want)
	}
}

// The source goes to the listener port + 1; TLS is refused for it (DNAS has none for sources).
func TestShoutcastSourceAddr(t *testing.T) {
	for _, c := range []struct{ raw, want string }{
		{"radio.example", "radio.example:8001"},
		{"http://radio.example:8000", "radio.example:8001"},
		{"[2001:db8::1]:9000", "[2001:db8::1]:9001"},
	} {
		scheme, hp, err := parseServerTarget(c.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := shoutcastSourceAddr(scheme, hp); err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.raw, got, err, c.want)
		}
	}
	scheme, hp, _ := parseServerTarget("https://radio.example:8000")
	if _, err := shoutcastSourceAddr(scheme, hp); err == nil {
		t.Error("an https SHOUTcast source was accepted")
	}
	if _, err := shoutcastSourceAddr("http", "radio.example:65535"); err == nil {
		t.Error("port 65535 + 1 was accepted")
	}
}

// Saved settings from older versions: a bitrate the console no longer offers becomes 192, and
// the format follows the protocol whatever was saved.
func TestShoutcastBitrateAndFormatNormalize(t *testing.T) {
	for _, c := range []struct {
		in      Config
		format  string
		bitrate int
	}{
		{Config{Protocol: "shoutcast", Format: "mp3", Bitrate: 256}, "aac", 192},
		{Config{Protocol: "shoutcast", Bitrate: 128}, "aac", 128},
		{Config{Protocol: "icecast", Format: "aac", Bitrate: 320}, "wav", 320},
		{Config{}, "wav", 192},
	} {
		cfg := c.in
		if err := normalizeConfig(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Format != c.format || cfg.Bitrate != c.bitrate {
			t.Errorf("%+v -> format %q bitrate %d, want %q %d", c.in, cfg.Format, cfg.Bitrate, c.format, c.bitrate)
		}
	}
}

// Track titles go to the DNAS admin.cgi with the source password (and the stream ID for v2).
// (Restored with the SHOUTcast mode; TestUpdateMetadataSkipsAShoutcastTarget covered its absence.)
func TestUpdateMetadataShoutcast(t *testing.T) {
	hits := make(chan *http.Request, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- r
		fmt.Fprintln(w, "OK")
	}))
	defer server.Close()
	for _, c := range []struct{ user, sid string }{{"source", ""}, {"2", "2"}} {
		b := New(newTestGuard())
		b.cfg = Config{Protocol: "shoutcast", ServerURL: server.URL, Username: c.user, Password: "mypass"}
		if err := b.UpdateMetadata("Kraftwerk", "Autobahn"); err != nil {
			t.Fatalf("UpdateMetadata: %v", err)
		}
		r := <-hits
		q := r.URL.Query()
		if r.URL.Path != "/admin.cgi" || q.Get("mode") != "updinfo" || q.Get("pass") != "mypass" || q.Get("song") != "Kraftwerk - Autobahn" || q.Get("sid") != c.sid {
			t.Errorf("user %q: request %s", c.user, r.URL.RequestURI())
		}
		if _, _, ok := r.BasicAuth(); ok {
			t.Errorf("user %q: basic auth sent to admin.cgi", c.user)
		}
		b.Close()
	}
}

// The admin.cgi URL carries the password: an error that quotes it shows *** instead.
func TestShoutcastMetadataErrorHidesThePassword(t *testing.T) {
	const secret = "superSecretPassword123!@#"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintln(w, "denied "+r.URL.RequestURI())
	}))
	defer server.Close()
	b := New(newTestGuard())
	defer b.Close()
	b.cfg = Config{Protocol: "shoutcast", ServerURL: server.URL, Password: secret}
	if err := b.UpdateMetadata("Artist", "Track"); err == nil {
		t.Fatal("want an error from a 403")
	}
	got := b.Status().LastError
	if strings.Contains(got, secret) || strings.Contains(got, url.QueryEscape(secret)) || !strings.Contains(got, "***") {
		t.Errorf("LastError = %q, want the password masked", got)
	}
}

// The real encoder over the deck's stock libavcodec, when the build host provides it
// (ENGINEX_AACENC=<wrapper that runs enginex-aacenc with a stock rootfs's libraries>): the
// session's AAC decodes (ffprobe, when present) at the chosen bitrate.
func TestRealEncoderStreamDecodes(t *testing.T) {
	if os.Getenv("ENGINEX_AACENC") == "" {
		t.Skip("ENGINEX_AACENC not set: the real encoder runs on the build host with a stock rootfs")
	}
	for _, kbps := range Bitrates {
		t.Run(strconv.Itoa(kbps), func(t *testing.T) {
			f := newFakeShoutcast(t, "OK2\r\n")
			b := New(newTestGuard())
			defer b.Close()
			if err := b.Configure(Config{Enabled: true, Protocol: "shoutcast", ServerURL: f.serverURL, Password: "pw", Bitrate: kbps,
				InputPath: os.Getenv("ENGINEX_AACENC_PCM")}); err != nil {
				t.Fatal(err)
			}
			s := f.next(t, b)
			defer s.conn.Close()
			const frames = 470 // 10 s
			out, err := os.CreateTemp(t.TempDir(), "*.aac")
			if err != nil {
				t.Fatal(err)
			}
			tee := bufio.NewReader(io.TeeReader(s.r, out))
			got := adtsFrames(t, tee, frames)
			out.Close()
			// The native encoder holds the average loosely: allow +-15 % over ten seconds.
			rate := float64(got*8) / (frames * 1024.0 / SampleRate) / 1000
			if rate < float64(kbps)*0.85 || rate > float64(kbps)*1.15 {
				t.Errorf("stream is %.1f kbps, want about %d", rate, kbps)
			}
			if _, err := exec.LookPath("ffprobe"); err != nil {
				t.Log("no ffprobe: ADTS framing checked only")
				return
			}
			cmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name,profile,sample_rate,channels", "-of", "csv=p=0", out.Name())
			msg, err := cmd.CombinedOutput()
			if err != nil || !strings.HasPrefix(strings.TrimSpace(string(msg)), "aac,LC,48000,2") {
				t.Errorf("ffprobe: %q %v", msg, err)
			}
			if msg, err := exec.Command("ffmpeg", "-v", "error", "-i", out.Name(), "-f", "null", "-").CombinedOutput(); err != nil || len(strings.TrimSpace(string(msg))) > 0 {
				if !errors.Is(err, exec.ErrNotFound) {
					t.Errorf("decode errors: %q %v", msg, err)
				}
			}
		})
	}
}
