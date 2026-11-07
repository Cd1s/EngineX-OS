package webui

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// SamplerRunner executes external audio tools (such as ffmpeg) or test mocks.
type SamplerRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

var defaultSamplerRunner SamplerRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

var (
	samplerRunnerMu sync.RWMutex
	samplerRunner   = defaultSamplerRunner
)

// SetSamplerRunner allows tests or callers to mock command execution.
func SetSamplerRunner(r SamplerRunner) {
	samplerRunnerMu.Lock()
	defer samplerRunnerMu.Unlock()
	if r == nil {
		samplerRunner = defaultSamplerRunner
	} else {
		samplerRunner = r
	}
}

func getSamplerRunner() SamplerRunner {
	samplerRunnerMu.RLock()
	defer samplerRunnerMu.RUnlock()
	return samplerRunner
}

type LoopToPadRequest struct {
	Deck       int    `json:"deck"`
	Pad        int    `json:"pad"`
	TrackPath  string `json:"trackPath"`
	InSample   int64  `json:"inSample"`
	OutSample  int64  `json:"outSample"`
	SampleRate int    `json:"sampleRate"`
}

type LoopToPadResponse struct {
	OK          bool    `json:"ok"`
	Deck        int     `json:"deck"`
	Pad         int     `json:"pad"`
	Path        string  `json:"path"`
	Samples     int64   `json:"samples"`
	SampleRate  int     `json:"sampleRate"`
	DurationSec float64 `json:"durationSec"`
}

func (s *Server) samplesDir() string {
	if s.cfg.StateDir != "" {
		return filepath.Join(s.cfg.StateDir, "samples")
	}
	return filepath.Join(s.uiDir(), "samples")
}

// determineSampleOutputPath determines where the looped sample WAV should be written.
// 1. If on an external volume under MediaRoot, tries "<Volume>/Samples from loops/Loop_Deck<D>_Pad<P>.wav".
// 2. Otherwise tries "/data/enginex/samples/Loop_Deck<D>_Pad<P>.wav".
// 3. Fallback to state/samples or ui/samples if /data is not writable (e.g. tests/sandbox).
func (s *Server) determineSampleOutputPath(trackPath string, deck, pad int) string {
	filename := fmt.Sprintf("Loop_Deck%d_Pad%d.wav", deck, pad)
	cleanTrack := filepath.Clean(trackPath)

	mediaRoot := filepath.Clean(s.mediaRoot())
	if rel, err := filepath.Rel(mediaRoot, cleanTrack); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) > 0 && parts[0] != "" {
			volRoot := filepath.Join(mediaRoot, parts[0])
			candDir := filepath.Join(volRoot, "Samples from loops")
			if err := os.MkdirAll(candDir, 0o755); err == nil {
				testFile := filepath.Join(candDir, ".writable_test")
				if err := os.WriteFile(testFile, []byte("ok"), 0o644); err == nil {
					_ = os.Remove(testFile)
					return filepath.Join(candDir, filename)
				}
			}
		}
	}

	// Candidate 2: standard EngineX samples directory
	enginexDir := "/data/enginex/samples"
	if err := os.MkdirAll(enginexDir, 0o755); err == nil {
		testFile := filepath.Join(enginexDir, ".writable_test")
		if err := os.WriteFile(testFile, []byte("ok"), 0o644); err == nil {
			_ = os.Remove(testFile)
			return filepath.Join(enginexDir, filename)
		}
	}

	// Candidate 3: state samples directory
	fallbackDir := s.samplesDir()
	_ = os.MkdirAll(fallbackDir, 0o755)
	return filepath.Join(fallbackDir, filename)
}

// sliceWavDirect attempts to slice [inSample, outSample) from a 16-bit PCM WAV in pure Go.
func sliceWavDirect(data []byte, inSample, outSample int64) ([]byte, bool) {
	if len(data) < 44 {
		return nil, false
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, false
	}

	offset := 12
	var fmtChunk []byte
	var dataChunkOffset int
	var dataChunkSize int

	for offset+8 <= len(data) {
		chunkID := string(data[offset : offset+4])
		chunkSize := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		chunkDataStart := offset + 8

		if chunkID == "fmt " {
			if chunkDataStart+chunkSize <= len(data) {
				fmtChunk = data[chunkDataStart : chunkDataStart+chunkSize]
			}
		} else if chunkID == "data" {
			dataChunkOffset = chunkDataStart
			dataChunkSize = chunkSize
			break
		}

		offset = chunkDataStart + chunkSize
		if chunkSize%2 != 0 {
			offset++
		}
	}

	if fmtChunk == nil || len(fmtChunk) < 16 || dataChunkOffset == 0 {
		return nil, false
	}

	audioFormat := binary.LittleEndian.Uint16(fmtChunk[0:2])
	numChannels := binary.LittleEndian.Uint16(fmtChunk[2:4])
	sampleRate := binary.LittleEndian.Uint32(fmtChunk[4:8])
	bitsPerSample := binary.LittleEndian.Uint16(fmtChunk[14:16])

	// We handle 16-bit PCM directly
	if audioFormat != 1 || bitsPerSample != 16 || numChannels == 0 {
		return nil, false
	}

	bytesPerFrame := int64(numChannels) * 2
	maxData := len(data) - dataChunkOffset
	if dataChunkSize > maxData {
		dataChunkSize = maxData
	}

	startByte := inSample * bytesPerFrame
	endByte := outSample * bytesPerFrame

	if startByte < 0 {
		startByte = 0
	}
	if startByte > int64(dataChunkSize) {
		startByte = int64(dataChunkSize)
	}
	if endByte > int64(dataChunkSize) {
		endByte = int64(dataChunkSize)
	}
	if endByte < startByte {
		endByte = startByte
	}

	rawSlice := data[dataChunkOffset+int(startByte) : dataChunkOffset+int(endByte)]

	// Build standard 16-bit PCM WAV
	hdr := make([]byte, 44)
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(36+len(rawSlice)))
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:20], 16)
	binary.LittleEndian.PutUint16(hdr[20:22], 1)
	binary.LittleEndian.PutUint16(hdr[22:24], numChannels)
	binary.LittleEndian.PutUint32(hdr[24:28], sampleRate)
	binary.LittleEndian.PutUint32(hdr[28:32], sampleRate*uint32(numChannels)*2)
	binary.LittleEndian.PutUint16(hdr[32:34], numChannels*2)
	binary.LittleEndian.PutUint16(hdr[34:36], 16)
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:44], uint32(len(rawSlice)))

	out := make([]byte, 44+len(rawSlice))
	copy(out[0:44], hdr)
	copy(out[44:], rawSlice)
	return out, true
}

// sliceLoop extracts [inSample, outSample) from trackPath and writes 16-bit PCM WAV to outPath.
func (s *Server) sliceLoop(ctx context.Context, trackPath, outPath string, inSample, outSample int64, sampleRate int) error {
	raw, err := os.ReadFile(trackPath)
	if err == nil {
		if sliced, ok := sliceWavDirect(raw, inSample, outSample); ok {
			_ = os.MkdirAll(filepath.Dir(outPath), 0o755)
			return os.WriteFile(outPath, sliced, 0o644)
		}
	}

	// Fallback to ffmpeg for MP3, FLAC, AIFF, or complex audio
	runner := getSamplerRunner()
	startSec := float64(inSample) / float64(sampleRate)
	durationSec := float64(outSample-inSample) / float64(sampleRate)

	_ = os.MkdirAll(filepath.Dir(outPath), 0o755)
	args := []string{
		"-y",
		"-ss", fmt.Sprintf("%.6f", startSec),
		"-t", fmt.Sprintf("%.6f", durationSec),
		"-i", trackPath,
		"-ar", strconv.Itoa(sampleRate),
		"-ac", "2",
		"-c:a", "pcm_s16le",
		outPath,
	}

	out, err := runner(ctx, "ffmpeg", args...)
	if err != nil {
		return fmt.Errorf("sampler ffmpeg fallback failed: %w (output: %s)", err, string(out))
	}

	return nil
}

func (s *Server) handleSamplerLoopToPad(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}

	var in LoopToPadRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid JSON request"))
		return
	}

	if in.Deck < 1 {
		fail(w, http.StatusBadRequest, errors.New("deck must be >= 1"))
		return
	}
	if in.Pad < 1 {
		fail(w, http.StatusBadRequest, errors.New("pad must be >= 1"))
		return
	}
	if strings.TrimSpace(in.TrackPath) == "" {
		fail(w, http.StatusBadRequest, errors.New("trackPath cannot be empty"))
		return
	}
	st, err := os.Stat(in.TrackPath)
	if err != nil || !st.Mode().IsRegular() {
		fail(w, http.StatusBadRequest, errors.New("track file not found"))
		return
	}
	if in.InSample < 0 {
		fail(w, http.StatusBadRequest, errors.New("inSample must be >= 0"))
		return
	}
	if in.OutSample <= in.InSample {
		fail(w, http.StatusBadRequest, errors.New("outSample must be greater than inSample"))
		return
	}
	if in.SampleRate <= 0 {
		in.SampleRate = 44100
	}

	outPath := s.determineSampleOutputPath(in.TrackPath, in.Deck, in.Pad)

	if err := s.sliceLoop(r.Context(), in.TrackPath, outPath, in.InSample, in.OutSample, in.SampleRate); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}

	samples := in.OutSample - in.InSample
	durSec := float64(samples) / float64(in.SampleRate)

	respond(w, LoopToPadResponse{
		OK:          true,
		Deck:        in.Deck,
		Pad:         in.Pad,
		Path:        outPath,
		Samples:     samples,
		SampleRate:  in.SampleRate,
		DurationSec: durSec,
	})
}
