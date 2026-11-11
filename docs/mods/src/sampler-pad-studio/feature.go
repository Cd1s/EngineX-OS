// Package sampler is the Sampler Pad Studio helper: an independent eight-file
// bank, not an undocumented native sampler binding.
package sampler

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
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
		return fmt.Errorf("ffmpeg failed: %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

func (f *feature) load(req map[string]any, action string, pad int64, index int) (err error) {
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
		if err := ffmpeg("-nostdin", "-v", "error", "-i", source, "-vn", "-c:a", "pcm_s16le", target); err != nil {
			return err
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
		if wav, werr := ReadWAV(source); werr == nil {
			if int64(wav.Rate) != rate || end > wav.Frames() {
				return modkit.Bad("sample rate mismatch or slice outside WAV bounds")
			}
			frame := int64(wav.FrameSize())
			out := wav.WithData(wav.Data[start*frame : end*frame])
			if err := out.Write(target); err != nil {
				return err
			}
		} else {
			r := fmt.Sprint(rate)
			if err := ffmpeg("-nostdin", "-v", "error", "-i", source, "-vn", "-ar", r,
				"-af", fmt.Sprintf("aresample=%s,atrim=start_sample=%d:end_sample=%d,asetpts=PTS-STARTPTS", r, start, end),
				"-c:a", "pcm_s16le", target); err != nil {
				return err
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
			if format != 1 || w.Channels < 1 || w.SampleWidth < 1 {
				return WAV{}, ErrNotPCM
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return WAV{}, ErrNotPCM
			}
			data, err := io.ReadAll(io.LimitReader(f, size))
			if err != nil {
				return WAV{}, err
			}
			w.Data = data[:len(data)-len(data)%w.FrameSize()]
			return w, nil
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
