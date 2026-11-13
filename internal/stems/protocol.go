package stems

// Session checks the Engine child dialect as tools/stems/worker.py speaks it,
// so the deck client can tell a finished render from a cut-off one without
// relying on an HTTP trailer that edge proxies may drop. Both directions are
// header blocks of key=value lines ending in a blank line. Engine's first
// header is the init (numChannels, sampleRate, maxBlockSize); then
// action=block + numSamples frames of float32 PCM per channel, or action=done.
// The renderer answers returnFrames + frames for numReturnChannels channels;
// after done it drains and ends with returnFrames=0.

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

const maxHeader = 8192

type stream struct {
	line   []byte
	fields map[string]string
	used   int
	skip   int64        // payload bytes still to pass
	raw    []byte       // the current header as sent, kept only when keep
	keep   bool         // keep raw
	data   func([]byte) // receives payload bytes, if set
}

// feed passes p through the parser; header calls back with each complete
// header and returns the payload size that follows it.
func (s *stream) feed(p []byte, header func(map[string]string) (int64, error)) error {
	for len(p) > 0 {
		if s.skip > 0 {
			n := int64(len(p))
			if n > s.skip {
				n = s.skip
			}
			s.skip -= n
			if s.data != nil {
				s.data(p[:n])
			}
			p = p[n:]
			continue
		}
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i+1]
		}
		p = p[len(chunk):]
		s.used += len(chunk)
		if s.used > maxHeader {
			return errors.New("stems protocol: header too large")
		}
		s.line = append(s.line, chunk...)
		if s.keep {
			s.raw = append(s.raw, chunk...)
		}
		if i < 0 {
			continue
		}
		line := strings.TrimRight(string(s.line), "\r\n")
		s.line = s.line[:0]
		if s.fields == nil {
			s.fields = map[string]string{}
		}
		if line != "" {
			k, v, ok := strings.Cut(line, "=")
			if _, dup := s.fields[k]; !ok || k == "" || dup {
				return fmt.Errorf("stems protocol: bad header line %q", line)
			}
			s.fields[k] = v
			continue
		}
		n, err := header(s.fields)
		if err != nil {
			return err
		}
		s.fields, s.used, s.skip, s.raw = nil, 0, n, s.raw[:0]
	}
	return nil
}

func (s *stream) idle() bool { return s.skip == 0 && len(s.line) == 0 && s.fields == nil }

// Session is safe for one goroutine per direction.
type Session struct {
	mu                       sync.Mutex
	in, out                  stream
	err                      error
	init                     bool
	Channels, ReturnChannels int
	SampleRate               float64
	MaxBlock                 int
	FramesIn, FramesOut      int64
	Blocks, Replies          int
	done                     bool
	lastReply                int64
	// Packed replies carry three stems as int16 (see pack.go).
	Packed bool
}

func count(fields map[string]string, key string, max int64) (int64, error) {
	n, err := strconv.ParseInt(fields[key], 10, 64)
	if err != nil || n < 0 || n > max {
		return 0, fmt.Errorf("stems protocol: bad %s %q", key, fields[key])
	}
	return n, nil
}

// FeedIn observes bytes Engine sends to the renderer.
func (s *Session) FeedIn(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return
	}
	s.err = s.in.feed(p, func(f map[string]string) (int64, error) {
		if !s.init {
			c, err := count(f, "numChannels", 8)
			if err != nil || c == 0 {
				return 0, errors.New("stems protocol: bad init header")
			}
			b, err := count(f, "maxBlockSize", 1<<20)
			if err != nil {
				return 0, err
			}
			s.init, s.Channels, s.MaxBlock = true, int(c), int(b)
			s.SampleRate, _ = strconv.ParseFloat(f["sampleRate"], 64)
			return 0, nil
		}
		switch f["action"] {
		case "block":
			if s.done {
				return 0, errors.New("stems protocol: block after done")
			}
			n, err := count(f, "numSamples", int64(s.MaxBlock))
			if err != nil {
				return 0, err
			}
			s.FramesIn += n
			s.Blocks++
			return n * int64(s.Channels) * 4, nil
		case "done":
			s.done = true
			return 0, nil
		}
		return 0, fmt.Errorf("stems protocol: unknown action %q", f["action"])
	})
}

// FeedOut observes bytes the renderer sends to Engine.
func (s *Session) FeedOut(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return
	}
	s.err = s.out.feed(p, func(f map[string]string) (int64, error) {
		if v, ok := f["numReturnChannels"]; ok {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 || n > 64 || n%4 != 0 {
				return 0, errors.New("stems protocol: bad numReturnChannels")
			}
			s.ReturnChannels = n
		}
		if _, ok := f["returnFrames"]; !ok {
			return 0, nil
		}
		if s.ReturnChannels == 0 {
			return 0, errors.New("stems protocol: returnFrames before numReturnChannels")
		}
		n, err := count(f, "returnFrames", 1<<20)
		if err != nil {
			return 0, err
		}
		s.FramesOut += n
		s.Replies++
		s.lastReply = n
		if s.Packed {
			return n * int64(s.ReturnChannels) * 3 / 4 * 2, nil
		}
		return n * int64(s.ReturnChannels) * 4, nil
	})
}

// Complete reports whether Engine sent done and the renderer returned every
// frame for four stems of each input channel and closed with returnFrames=0,
// both streams ending on a message boundary.
func (s *Session) Complete() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.err != nil:
		return s.err
	case !s.init && s.in.idle():
		return nil // greeting only, as in a presence check: no audio, nothing for Engine to write
	case !s.done:
		return errors.New("stems protocol: Engine never sent done")
	case s.ReturnChannels != 4*s.Channels:
		return fmt.Errorf("stems protocol: %d return channels for %d input channels", s.ReturnChannels, s.Channels)
	case s.FramesOut != s.FramesIn:
		return fmt.Errorf("stems protocol: returned %d of %d frames", s.FramesOut, s.FramesIn)
	case s.Replies == 0 || s.lastReply != 0 || !s.in.idle() || !s.out.idle():
		return errors.New("stems protocol: stream ended mid-message")
	}
	return nil
}

func (s *Session) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("%d frames at %.0f Hz in %d blocks (max %d)", s.FramesIn, s.SampleRate, s.Blocks, s.MaxBlock)
}

// Err reports the first protocol violation seen so far.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// UploadTracker observes an Engine upload stream (unpacked float32 or
// X-Stems-Framing=2 packed int16) and reports when the terminal action=done
// header has arrived, allowing HTTP servers to disarm upload idle deadlines
// while a slow render finishes with stdin still held open.
type UploadTracker struct {
	Packed   bool
	s        stream
	init     bool
	channels int
	done     bool
	bad      bool
}

// Feed advances the tracker with upload wire bytes and returns true once
// action=done has been observed.
func (u *UploadTracker) Feed(p []byte) bool {
	if u.done || u.bad {
		return u.done
	}
	err := u.s.feed(p, func(f map[string]string) (int64, error) {
		if !u.init {
			if len(f) == 0 {
				return 0, nil
			}
			c, err := count(f, "numChannels", 8)
			if err != nil || c == 0 {
				return 0, errors.New("bad init")
			}
			u.init, u.channels = true, int(c)
			return 0, nil
		}
		switch f["action"] {
		case "block":
			n, err := count(f, "numSamples", 1<<20)
			if err != nil {
				return 0, err
			}
			bps := int64(4)
			if u.Packed {
				bps = 2
			}
			return n * int64(u.channels) * bps, nil
		case "done":
			u.done = true
			return 0, nil
		}
		return 0, errors.New("bad action")
	})
	if err != nil {
		u.bad = true
	}
	return u.done
}
