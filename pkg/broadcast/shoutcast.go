package broadcast

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The SHOUTcast mode (#138, #345). SHOUTcast servers take MP3 or AAC, never WAV, so a saved
// SHOUTcast target gets AAC-LC in ADTS frames from enginex-aacenc. That small program carries no
// codec: it opens the AAC encoder of the FFmpeg 4.4 libavcodec that every stock base image already
// ships for Engine (tools/aacenc). The /live.wav stream and the Icecast WAV push do not change.
//
// Where the cost goes and why nothing waits on it:
//   - The encoder runs only while a SHOUTcast push session is up, as a child of stream-sync: it
//     inherits the background CPU, SCHED_IDLE, nice 19 and idle I/O (applied again by the guard).
//   - Capture hands PCM to the session through the existing 80-chunk channel and drops a chunk
//     when it is full (workerLoop), so a slow encoder never holds up the feed FIFO or /live.wav.
//   - Encoded frames wait for the network in a queue of aacQueueFrames; when a slow server lets
//     it fill, new frames are dropped, so the encoder never waits on the network and memory stays
//     bounded.

// DefaultBitrate is the SHOUTcast mode's AAC bitrate in kbps when none is chosen.
const DefaultBitrate = 192

// Bitrates are the AAC bitrates the console offers for the SHOUTcast mode.
var Bitrates = []int{128, 192, 320}

func validBitrate(kbps int) bool {
	for _, k := range Bitrates {
		if k == kbps {
			return true
		}
	}
	return false
}

// aacQueueFrames bounds the encoded audio waiting for a slow server: 96 AAC frames of 1024
// samples = 2.05 s, at most about 90 KB at 320 kbps.
const aacQueueFrames = 96

// aacEncoderName is the encoder program. Gen-1 images install it in aacEncoderDir; Gen-2 bundles
// put it next to enginex-suite, where stream-sync runs from.
const (
	aacEncoderName = "enginex-aacenc"
	aacEncoderDir  = "/usr/local/lib/enginex"
)

// findAACEncoder returns the encoder program: $ENGINEX_AACENC (development), next to this
// executable, then the Gen-1 install path.
func findAACEncoder() (string, error) {
	if p := strings.TrimSpace(os.Getenv("ENGINEX_AACENC")); p != "" {
		return p, nil
	}
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, dir := range append(dirs, aacEncoderDir) {
		p := filepath.Join(dir, aacEncoderName)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", errors.New("the AAC encoder (" + aacEncoderName + ") is not installed on this deck")
}

// encoderCommand builds the encoder's command line; tests replace it with a fake encoder.
type encoderCommand func(args ...string) (*exec.Cmd, error)

func defaultEncoderCommand(args ...string) (*exec.Cmd, error) {
	path, err := findAACEncoder()
	if err != nil {
		return nil, err
	}
	return exec.Command(path, args...), nil
}

// shoutcastLogin returns the DNAS source login line: a numeric Username is a
// DNAS v2 stream ID ("password:#id"); anything else (incl. "source") is v1, password only.
func shoutcastLogin(cfg Config) string {
	if id := shoutcastStreamID(cfg); id > 0 {
		return fmt.Sprintf("%s:#%d", cfg.Password, id)
	}
	return cfg.Password
}

// shoutcastStreamID returns the DNAS v2 stream ID from Username ("1" or "#1"), or 0 for v1.
func shoutcastStreamID(cfg Config) int {
	id, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(cfg.Username), "#"))
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// shoutcastSourceAddr is where a SHOUTcast source connects: the server's port plus one, the
// legacy source port of DNAS v1 and v2 (and of Icecast's shoutcast-compat listener). The console
// asks for the server address listeners use, as other DJ software (libshout, BUTT) does.
func shoutcastSourceAddr(scheme, hostPort string) (string, error) {
	if scheme == "https" {
		return "", errors.New("SHOUTcast sources connect without TLS: enter the server as http://host:port or host:port")
	}
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65534 {
		return "", fmt.Errorf("invalid SHOUTcast port %q", port)
	}
	return net.JoinHostPort(host, strconv.Itoa(n+1)), nil
}

// maxShoutcastReply bounds what is read of the server's answer to the login.
const maxShoutcastReply = 512

// readShoutcastReply returns the server's first non-empty answer line, trimmed. It reads up to the
// line end, as TCP may deliver the line in pieces ("O", then "K2\r\n"), and at most
// maxShoutcastReply bytes. A server that closes after its answer (often "invalid password")
// ends the line too.
func readShoutcastReply(conn net.Conn) (string, error) {
	lr := &io.LimitedReader{R: conn, N: maxShoutcastReply}
	r := bufio.NewReaderSize(lr, 64)
	for {
		line, err := r.ReadString('\n')
		text := strings.TrimSpace(line)
		switch {
		case err == nil && text == "":
			continue // a blank line before the answer
		case err == nil:
			return text, nil
		case errors.Is(err, io.EOF) && lr.N <= 0:
			return "", fmt.Errorf("shoutcast handshake: no answer line within %d bytes", maxShoutcastReply)
		case errors.Is(err, io.EOF) && text != "":
			return text, nil
		default:
			return "", fmt.Errorf("shoutcast reading handshake failed: %w", err)
		}
	}
}

// handshakeShoutcast logs in with the SHOUTcast v1 source protocol (DNAS v2 takes it with a
// stream ID) and announces an AAC stream.
func (b *Broadcaster) handshakeShoutcast(conn net.Conn, cfg Config) error {
	if _, err := fmt.Fprintf(conn, "%s\r\n", shoutcastLogin(cfg)); err != nil {
		return fmt.Errorf("shoutcast sending password failed: %w", err)
	}

	reply, err := readShoutcastReply(conn)
	if err != nil {
		return err
	}
	// A Shoutcast v2 (and Icecast's v1 compatibility) answers "OK2", a Shoutcast v1 server "OK"
	// (BH-GF4-7); any other line, e.g. "invalid password" or "OK2BAD", is a refusal.
	if reply != "OK" && reply != "OK2" {
		return fmt.Errorf("shoutcast authentication failed: %s", reply)
	}

	icyHeaders := fmt.Sprintf(
		"icy-name:%s\r\n"+
			"icy-pub:0\r\n"+
			"icy-br:%d\r\n"+
			"content-type:audio/aacp\r\n"+
			"\r\n",
		cfg.StreamName, cfg.Bitrate,
	)
	if _, err := io.WriteString(conn, icyHeaders); err != nil {
		return fmt.Errorf("shoutcast sending icy headers failed: %w", err)
	}
	return nil
}

// tailBuffer keeps the last bytes written to it: the encoder's stderr, for the error shown.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 512 {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-512:]...)
	}
	return len(p), nil
}

// lastLine is the last non-empty line written.
func (t *tailBuffer) lastLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(t.buf)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// aacEncoder is one running enginex-aacenc.
type aacEncoder struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *tailBuffer
	once   sync.Once
}

// startEncoder checks that the stock encoder opens, then starts it at kbps.
func (b *Broadcaster) startEncoder(kbps int) (*aacEncoder, error) {
	command := b.encoderCmd
	if command == nil {
		command = defaultEncoderCommand
	}
	// The check exits within milliseconds; it keeps a deck without the encoder from logging in to
	// the server and dropping the connection again on every retry.
	check, err := command("--check")
	if err != nil {
		return nil, err
	}
	if out, err := runWithTimeout(check, 10*time.Second); err != nil {
		msg := strings.TrimSpace(string(out))
		if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
			msg = msg[i+1:]
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New("AAC encoder unavailable: " + strings.TrimPrefix(msg, aacEncoderName+": "))
	}

	cmd, err := command(strconv.Itoa(kbps))
	if err != nil {
		return nil, err
	}
	enc := &aacEncoder{cmd: cmd, stderr: &tailBuffer{}}
	cmd.Stderr = enc.stderr
	if enc.stdin, err = cmd.StdinPipe(); err != nil {
		return nil, err
	}
	if enc.stdout, err = cmd.StdoutPipe(); err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("AAC encoder did not start: %w", err)
	}
	// A child already inherits stream-sync's CPU, SCHED_IDLE, nice and I/O class; applying the
	// guard again also covers a stream-sync started without them. If stream-sync dies, the
	// encoder reads end of input and exits: nothing keeps encoding.
	_, _ = b.guard.ApplyToProcess(cmd.Process.Pid)
	return enc, nil
}

// runWithTimeout runs cmd and returns its combined output, killing it after d.
func runWithTimeout(cmd *exec.Cmd, d time.Duration) ([]byte, error) {
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.Bytes(), err
	case <-time.After(d):
		_ = cmd.Process.Kill()
		<-done
		return out.Bytes(), errors.New("timed out")
	}
}

// stop ends the encoder and waits for it.
func (e *aacEncoder) stop() {
	e.once.Do(func() {
		_ = e.stdin.Close()
		_ = e.cmd.Process.Kill()
		_ = e.cmd.Wait()
	})
}

// failure explains why the encoder side of a session ended.
func (e *aacEncoder) failure(err error) error {
	if msg := e.stderr.lastLine(); msg != "" {
		return errors.New("AAC encoder: " + strings.TrimPrefix(msg, aacEncoderName+": "))
	}
	return fmt.Errorf("AAC encoder stopped: %w", err)
}

// readADTS returns the next ADTS frame (header included) from the encoder's output.
func readADTS(r *bufio.Reader) ([]byte, error) {
	h, err := r.Peek(7)
	if err != nil {
		return nil, err
	}
	if h[0] != 0xff || h[1]&0xf6 != 0xf0 {
		return nil, errors.New("lost ADTS sync")
	}
	n := int(h[3]&3)<<11 | int(h[4])<<3 | int(h[5])>>5
	if n < 7 {
		return nil, errors.New("bad ADTS frame length")
	}
	frame := make([]byte, n)
	if _, err := io.ReadFull(r, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

// streamAAC runs one SHOUTcast session's audio: PCM from capture into the encoder, its frames to
// the server, until ctx ends or a side fails.
func (b *Broadcaster) streamAAC(ctx context.Context, conn net.Conn, feed <-chan []byte, enc *aacEncoder) error {
	// Drop PCM queued while we were connecting so the stream starts live.
	for drained := false; !drained; {
		select {
		case <-feed:
		default:
			drained = true
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		enc.stop() // unblocks both goroutines: their pipes close
		wg.Wait()
	}()
	errc := make(chan error, 2)
	frames := make(chan []byte, aacQueueFrames)

	wg.Add(2)
	go func() { // capture -> encoder. Waits only on the encoder; while it lags, capture drops.
		defer wg.Done()
		for {
			var pcm []byte
			select {
			case <-ctx.Done():
				return
			case pcm = <-feed:
			}
			if _, err := enc.stdin.Write(pcm); err != nil {
				errc <- enc.failure(err)
				return
			}
		}
	}()
	go func() { // encoder -> queue. Never waits on the network: a full queue drops the frame.
		defer wg.Done()
		r := bufio.NewReaderSize(enc.stdout, 16<<10)
		for {
			frame, err := readADTS(r)
			if err != nil {
				errc <- enc.failure(err)
				return
			}
			select {
			case frames <- frame:
			default:
				b.dropped.Add(1)
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errc:
			return err
		case frame := <-frames:
			if err := writeConn(conn, frame); err != nil {
				return err
			}
			b.bytesSent.Add(int64(len(frame)))
		}
	}
}
