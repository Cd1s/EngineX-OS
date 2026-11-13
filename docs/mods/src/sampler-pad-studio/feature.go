// Package sampler is the Sampler Pad Studio helper: an independent eight-file
// bank, not an undocumented native sampler binding.
package sampler

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
	"github.com/Cd1s/EngineX-OS/internal/audioheader"
	"github.com/hajimehoshi/go-mp3"
	"github.com/jfreymuth/oggvorbis"
	"github.com/mewkiz/flac"
)

type slot struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	Duration float64 `json:"duration"`
}

type feature struct {
	state string
	slots []slot
}

// New returns the Sampler Pad Studio feature.
func New(env modkit.Env) (modkit.Feature, error) {
	return newFeature(env.State)
}

func newFeature(state string) (*feature, error) {
	f := &feature{state: state, slots: make([]slot, 8)}
	for i := range f.slots {
		f.slots[i] = slot{Name: fmt.Sprintf("Pad %d", i+1)}
	}
	bank := filepath.Join(state, "bank.json")
	if _, err := os.Stat(bank); err == nil {
		var saved []slot
		if err := modkit.LoadJSON(bank, &saved); err != nil || len(saved) != 8 {
			return nil, modkit.Bad("bank must have exactly eight slots")
		}
		f.slots = saved
	}
	return f, nil
}

func (f *feature) Status() (any, error) {
	return map[string]any{"slots": f.slots, "nativeBinding": false,
		"note": "Files are ready for loading via the native browser; pad numbers here identify this Mod's bank."}, nil
}

func (f *feature) Action(req map[string]any) (any, error) {
	padv, err := modkit.Field(req, "pad")
	if err != nil {
		return nil, err
	}
	pad, ok := modkit.Int(padv)
	if !ok || pad < 1 || pad > 8 {
		return nil, modkit.Bad("pad must be 1..8")
	}
	index := int(pad - 1)
	action, err := modkit.Field(req, "action")
	if err != nil {
		return nil, err
	}
	previous := f.slots[index]
	switch action {
	case "rename":
		v, err := modkit.Field(req, "name")
		if err != nil {
			return nil, err
		}
		s, ok := v.(string)
		if !ok {
			return nil, modkit.Bad("name must be 1..80 visible characters")
		}
		name := strings.TrimSpace(s)
		bad := name == "" || utf8.RuneCountInString(name) > 80
		for _, r := range name {
			bad = bad || r < 32
		}
		if bad {
			return nil, modkit.Bad("name must be 1..80 visible characters")
		}
		f.slots[index].Name = name
	case "clear":
		// Never delete media files. Previous Mod exports remain available in history.
		f.slots[index] = slot{Name: fmt.Sprintf("Pad %d", pad)}
	case "import", "slice":
		if err := f.load(req, action.(string), pad, index); err != nil {
			return nil, err
		}
	default:
		return nil, modkit.Bad("unknown action")
	}
	if err := modkit.SaveJSON(filepath.Join(f.state, "bank.json"), f.slots); err != nil {
		f.slots[index] = previous
		return nil, err
	}
	return f.Status()
}

func ffmpeg(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg failed: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

func (f *feature) load(req map[string]any, action string, pad int64, index int) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	srcv, err := modkit.Field(req, "source")
	if err != nil {
		return err
	}
	source, err := modkit.MediaFile(srcv, f.state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(f.state, 0o755); err != nil {
		return err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	target := filepath.Join(f.state, fmt.Sprintf("Pad%d-%s.wav", pad, hex.EncodeToString(id)))
	defer func() {
		if err != nil {
			os.Remove(target)
		}
	}()
	var start, end int64
	if action == "import" {
		if wav, werr := ReadWAVRange(ctx, source, 0, -1, 0); werr == nil {
			if err := wav.Write(target); err != nil {
				return err
			}
		} else if !errors.Is(werr, ErrNotPCM) {
			return werr
		} else if err := ffmpeg("-nostdin", "-v", "error", "-i", source, "-vn", "-c:a", "pcm_s16le", target); err != nil {
			if !errors.Is(err, exec.ErrNotFound) {
				return err
			}
			wav, err := ReadAudio(ctx, source, 0, -1, 0)
			if err != nil {
				return err
			}
			if err := wav.Write(target); err != nil {
				return err
			}
		}
	} else {
		sv, err := modkit.Field(req, "inSample")
		if err != nil {
			return err
		}
		ev, err := modkit.Field(req, "outSample")
		if err != nil {
			return err
		}
		var rv any = int64(44100)
		if v, ok := req["sampleRate"]; ok {
			rv = v
		}
		var ok1, ok2, ok3 bool
		var rate int64
		start, ok1 = modkit.Int(sv)
		end, ok2 = modkit.Int(ev)
		rate, ok3 = modkit.Int(rv)
		if !ok1 || !ok2 || !ok3 || start < 0 || start >= end || rate < 8000 || rate > 384000 {
			return modkit.Bad("expected integer sample positions: 0 <= inSample < outSample and valid sampleRate")
		}
		if end-start > rate*600 {
			return modkit.Bad("a loop slice is limited to ten minutes")
		}
		if wav, werr := ReadWAVRange(ctx, source, start, end, int(rate)); werr == nil {
			if err := wav.Write(target); err != nil {
				return err
			}
		} else if !errors.Is(werr, ErrNotPCM) {
			return werr
		} else {
			r := fmt.Sprint(rate)
			if err := ffmpeg("-nostdin", "-v", "error", "-i", source, "-vn", "-ar", r,
				"-af", fmt.Sprintf("aresample=%s,atrim=start_sample=%d:end_sample=%d,asetpts=PTS-STARTPTS", r, start, end),
				"-c:a", "pcm_s16le", target); err != nil {
				if !errors.Is(err, exec.ErrNotFound) {
					return err
				}
				wav, err := ReadAudio(ctx, source, start, end, int(rate))
				if err != nil {
					return err
				}
				if err := wav.Write(target); err != nil {
					return err
				}
			}
		}
	}
	out, err := ReadWAV(target)
	if err != nil {
		return err
	}
	count := out.Frames()
	if count == 0 || (action == "slice" && count != end-start) {
		return modkit.Bad("decoded audio is empty or the requested slice exceeds source bounds")
	}
	stem := strings.TrimSuffix(filepath.Base(source), filepath.Ext(filepath.Base(source)))
	if r := []rune(stem); len(r) > 80 {
		stem = string(r[:80])
	}
	f.slots[index] = slot{Name: stem, Path: target, Duration: float64(count) / float64(out.Rate)}
	return nil
}

func (f *feature) Close() {}

// WAV is an uncompressed PCM WAV file, as Python's wave module reads and writes it.
type WAV struct {
	Channels, SampleWidth, Rate int
	Data                        []byte
}

// FrameSize is bytes per sample frame.
func (w WAV) FrameSize() int { return w.Channels * w.SampleWidth }

// Frames is the frame count.
func (w WAV) Frames() int64 { return int64(len(w.Data) / w.FrameSize()) }

// WithData returns the same format with other frames.
func (w WAV) WithData(data []byte) WAV { w.Data = data; return w }

// ErrNotPCM is returned for anything but a readable PCM WAV (ffmpeg handles those).
var ErrNotPCM = errors.New("not a PCM WAV file")

// ReadWAV parses RIFF/WAVE with a PCM fmt chunk (format 1, or extensible PCM).
func ReadWAV(path string) (WAV, error) {
	return ReadWAVRange(context.Background(), path, 0, -1, 0)
}

// ReadWAVRange reads only [start,end) PCM frames; end=-1 reads all frames.
// A nonzero rate must match the source sample rate.
func ReadWAVRange(ctx context.Context, path string, start, end int64, rate int) (WAV, error) {
	if err := ctx.Err(); err != nil {
		return WAV{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return WAV{}, err
	}
	defer f.Close()
	var head [12]byte
	if _, err := io.ReadFull(f, head[:]); err != nil || string(head[:4]) != "RIFF" || string(head[8:]) != "WAVE" {
		return WAV{}, ErrNotPCM
	}
	var w WAV
	haveFmt := false
	for {
		var ch [8]byte
		if _, err := io.ReadFull(f, ch[:]); err != nil {
			return WAV{}, ErrNotPCM
		}
		size := int64(binary.LittleEndian.Uint32(ch[4:]))
		id := string(ch[:4])
		switch id {
		case "fmt ":
			if size < 16 || size > 1<<16 {
				return WAV{}, ErrNotPCM
			}
			b := make([]byte, size)
			if _, err := io.ReadFull(f, b); err != nil {
				return WAV{}, ErrNotPCM
			}
			format := binary.LittleEndian.Uint16(b)
			if format == 0xFFFE && size >= 40 {
				format = binary.LittleEndian.Uint16(b[24:]) // extensible: sub-format GUID starts with the format tag
			}
			w.Channels = int(binary.LittleEndian.Uint16(b[2:]))
			w.Rate = int(binary.LittleEndian.Uint32(b[4:]))
			bits := int(binary.LittleEndian.Uint16(b[14:]))
			w.SampleWidth = (bits + 7) / 8
			if format != 1 || w.Channels < 1 || w.Channels > 8 || (bits != 8 && bits != 16 && bits != 24 && bits != 32) || w.Rate < 8000 || w.Rate > 384000 {
				return WAV{}, ErrNotPCM
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return WAV{}, ErrNotPCM
			}
			at, err := f.Seek(0, io.SeekCurrent)
			if err != nil {
				return WAV{}, err
			}
			st, err := f.Stat()
			if err != nil {
				return WAV{}, err
			}
			frame := int64(w.FrameSize())
			if size > st.Size()-at || size%frame != 0 {
				return WAV{}, modkit.Bad("truncated WAV data")
			}
			frames := size / frame
			if end == -1 {
				end = frames
			}
			if start < 0 || end <= start || end > frames || (rate != 0 && rate != w.Rate) {
				return WAV{}, modkit.Bad("sample rate mismatch or slice outside WAV bounds")
			}
			n := (end - start) * frame
			if n > 128<<20 {
				return WAV{}, modkit.Bad("decoded PCM memory limit exceeded")
			}
			w.Data = make([]byte, int(n))
			_, err = io.ReadFull(contextReader{ctx, io.NewSectionReader(f, at+start*frame, n)}, w.Data)
			return w, err
		default:
			if _, err := f.Seek(size+size%2, io.SeekCurrent); err != nil {
				return WAV{}, ErrNotPCM
			}
		}
		if id == "fmt " && size%2 == 1 {
			f.Seek(1, io.SeekCurrent)
		}
	}
}

// Write stores a canonical 44-byte-header PCM WAV.
func (w WAV) Write(path string) error {
	var b bytes.Buffer
	n := uint32(len(w.Data))
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, 36+n+n%2)
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(w.Channels), uint32(w.Rate),
		uint32(w.Rate * w.FrameSize()), uint16(w.FrameSize()), uint16(w.SampleWidth * 8)} {
		binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, n)
	b.Write(w.Data)
	if n%2 == 1 {
		b.WriteByte(0)
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// ReadAudio decodes the formats supported by the in-process codecs without ffmpeg.
// Channels are preserved; sample offsets refer to the source's native rate.
func ReadAudio(ctx context.Context, path string, start, end int64, rate int) (w WAV, err error) {
	defer func() {
		if p := recover(); p != nil {
			w, err = WAV{}, fmt.Errorf("malformed audio: %v", p)
		}
	}()
	w, err = ReadWAVRange(ctx, path, start, end, rate)
	if !errors.Is(err, ErrNotPCM) {
		return w, err
	}
	f, err := os.Open(path)
	if err != nil {
		return WAV{}, err
	}
	defer f.Close()
	br := bufio.NewReader(contextReader{ctx, f})
	if err = audioheader.SkipID3(br); err != nil {
		return WAV{}, err
	}
	head, _ := br.Peek(4)
	w.SampleWidth = 2
	var next func() ([]byte, error)
	var expectedFrames uint64
	switch {
	case len(head) >= 2 && head[0] == 0xff && head[1]&0xe0 == 0xe0:
		trim := int64(audioheader.PeekMP3StartOffset(br))
		dec, e := mp3.NewDecoder(br)
		if e != nil {
			return WAV{}, e
		}
		w.Channels, w.Rate = 2, dec.SampleRate()
		if start > math.MaxInt64-trim || end > math.MaxInt64-trim {
			return WAV{}, modkit.Bad("sample range overflow")
		}
		start += trim
		if end >= 0 {
			end += trim
		}
		buf := make([]byte, 4096*4)
		next = func() ([]byte, error) { n, e := dec.Read(buf); return buf[:n], e }
	case string(head) == "fLaC":
		dec, e := flac.New(br)
		if e != nil {
			return WAV{}, e
		}
		w.Channels, w.Rate = int(dec.Info.NChannels), int(dec.Info.SampleRate)
		bits := int(dec.Info.BitsPerSample)
		expectedFrames = dec.Info.NSamples
		if bits < 8 || bits > 32 || w.Channels < 1 || w.Channels > 8 {
			return WAV{}, modkit.Bad("invalid FLAC format")
		}
		next = func() ([]byte, error) {
			frame, e := dec.Next()
			if e != nil {
				return nil, e
			}
			if frame.SampleRate == 0 {
				frame.SampleRate = dec.Info.SampleRate
			}
			if frame.BitsPerSample == 0 {
				frame.BitsPerSample = dec.Info.BitsPerSample
			}
			if frame.Channels.Count() != w.Channels || int(frame.SampleRate) != w.Rate || int(frame.BitsPerSample) != bits {
				return nil, modkit.Bad("inconsistent FLAC format")
			}
			if err := frame.Parse(); err != nil {
				return nil, err
			}
			n := int(frame.BlockSize)
			b := make([]byte, n*w.Channels*2)
			for c, sub := range frame.Subframes {
				if len(sub.Samples) != n {
					return nil, modkit.Bad("truncated FLAC frame")
				}
				for i, v := range sub.Samples {
					if bits > 16 {
						v >>= uint(bits - 16)
					} else {
						v <<= uint(16 - bits)
					}
					binary.LittleEndian.PutUint16(b[(i*w.Channels+c)*2:], uint16(int16(v)))
				}
			}
			return b, nil
		}
	case string(head) == "OggS":
		dec, e := oggvorbis.NewReader(br)
		if e != nil {
			return WAV{}, e
		}
		w.Channels, w.Rate = dec.Channels(), dec.SampleRate()
		if w.Channels < 1 || w.Channels > 8 {
			return WAV{}, modkit.Bad("invalid Vorbis channels")
		}
		buf := make([]float32, 4096*w.Channels)
		next = func() ([]byte, error) {
			n, e := dec.Read(buf)
			b := make([]byte, n*2)
			for i, v := range buf[:n] {
				if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
					return nil, modkit.Bad("non-finite audio sample")
				}
				sample := int16(max(-32768, min(32767, math.Round(float64(v)*32768))))
				binary.LittleEndian.PutUint16(b[i*2:], uint16(sample))
			}
			return b, e
		}
	default:
		return WAV{}, fmt.Errorf("unsupported in-process audio format")
	}
	if w.Rate < 8000 || w.Rate > 384000 || (rate != 0 && rate != w.Rate) {
		return WAV{}, modkit.Bad("sample rate mismatch or invalid source rate")
	}
	if start < 0 || (end != -1 && end <= start) {
		return WAV{}, modkit.Bad("invalid sample range")
	}
	var position int64
	frame := int64(w.FrameSize())
	for {
		if err = ctx.Err(); err != nil {
			return WAV{}, err
		}
		b, e := next()
		if int64(len(b))%frame != 0 {
			return WAV{}, modkit.Bad("truncated audio frame")
		}
		frames := int64(len(b)) / frame
		from, to := max(start-position, 0), frames
		if end >= 0 {
			to = min(to, end-position)
		}
		if from < to {
			chunk := b[from*frame : to*frame]
			if len(w.Data)+len(chunk) > 128<<20 {
				return WAV{}, modkit.Bad("decoded PCM memory limit exceeded")
			}
			w.Data = append(w.Data, chunk...)
		}
		position += frames
		if e != nil && e != io.EOF {
			return WAV{}, e
		}
		if end >= 0 && position >= end {
			return w, nil
		}
		if e == io.EOF {
			if expectedFrames != 0 && uint64(position) != expectedFrames {
				return WAV{}, modkit.Bad("truncated FLAC samples")
			}
			if len(w.Data) == 0 || end >= 0 {
				return WAV{}, modkit.Bad("empty audio or slice outside source bounds")
			}
			return w, nil
		}
	}
}
