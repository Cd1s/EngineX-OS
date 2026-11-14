package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
	sampler "github.com/Cd1s/EngineX-OS/docs/mods/src/sampler-pad-studio"
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

// sliceLoop extracts [inSample, outSample) from trackPath and writes PCM WAV to outPath.
// errEmptyLoop means the requested range holds no audio frames.
var errEmptyLoop = errors.New("loop range contains no audio frames")

func (s *Server) sliceLoop(ctx context.Context, trackPath, outPath string, inSample, outSample int64, sampleRate int) error {
	wav, err := sampler.ReadWAVRange(ctx, trackPath, inSample, outSample, sampleRate)
	if err == nil {
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}
		return wav.Write(outPath)
	}
	if !errors.Is(err, sampler.ErrNotPCM) {
		return err
	}

	// Fallback to ffmpeg for MP3, FLAC, AIFF, or complex audio
	runner := getSamplerRunner()

	_ = os.MkdirAll(filepath.Dir(outPath), 0o755)
	args := []string{
		"-y",
		"-i", trackPath,
		"-af", fmt.Sprintf("aresample=%d,atrim=start_sample=%d:end_sample=%d,asetpts=PTS-STARTPTS", sampleRate, inSample, outSample),
		"-ar", strconv.Itoa(sampleRate),
		"-ac", "2",
		"-c:a", "pcm_s16le",
		outPath,
	}

	out, err := runner(ctx, "ffmpeg", args...)
	if err != nil {
		if !errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("sampler ffmpeg fallback failed: %w (output: %s)", err, string(out))
		}
		wav, err := sampler.ReadAudio(ctx, trackPath, inSample, outSample, sampleRate)
		if err != nil {
			return err
		}
		return wav.Write(outPath)
	}

	wav, err = sampler.ReadWAVRange(ctx, outPath, 0, -1, sampleRate)
	if err != nil {
		return err
	}
	if wav.Frames() != outSample-inSample {
		return errEmptyLoop
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
		if errors.Is(err, errEmptyLoop) || modkit.IsBad(err) {
			fail(w, http.StatusBadRequest, err)
			return
		}
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
