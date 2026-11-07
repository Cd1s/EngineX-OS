package webui

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// createSyntheticWav creates a valid 16-bit stereo PCM WAV file with deterministic sample data.
func createSyntheticWav(t *testing.T, filePath string, sampleRate int, numFrames int) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(filePath), 0o755)

	numChannels := 2
	bitsPerSample := 16
	bytesPerFrame := numChannels * (bitsPerSample / 8)
	dataSize := numFrames * bytesPerFrame

	buf := make([]byte, 44+dataSize)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataSize))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:24], uint16(numChannels))
	binary.LittleEndian.PutUint32(buf[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(sampleRate*numChannels*2))
	binary.LittleEndian.PutUint16(buf[32:34], uint16(bytesPerFrame))
	binary.LittleEndian.PutUint16(buf[34:36], uint16(bitsPerSample))
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataSize))

	for i := 0; i < numFrames; i++ {
		offset := 44 + i*bytesPerFrame
		left := int16(i & 0x7fff)
		right := int16((i * 2) & 0x7fff)
		binary.LittleEndian.PutUint16(buf[offset:offset+2], uint16(left))
		binary.LittleEndian.PutUint16(buf[offset+2:offset+4], uint16(right))
	}

	if err := os.WriteFile(filePath, buf, 0o644); err != nil {
		t.Fatalf("failed to write test WAV: %v", err)
	}
}

type mockSamplerRunner struct {
	mu       sync.Mutex
	commands [][]string
}

func (m *mockSamplerRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cmd := append([]string{name}, args...)
	m.commands = append(m.commands, cmd)

	// If output file is requested, create a dummy WAV file so downstream stat/checks succeed
	if len(args) > 0 {
		outPath := args[len(args)-1]
		if strings.HasSuffix(outPath, ".wav") {
			_ = os.MkdirAll(filepath.Dir(outPath), 0o755)
			hdr := make([]byte, 44)
			copy(hdr[0:4], "RIFF")
			binary.LittleEndian.PutUint32(hdr[4:8], 36)
			copy(hdr[8:12], "WAVE")
			copy(hdr[12:16], "fmt ")
			binary.LittleEndian.PutUint32(hdr[16:20], 16)
			binary.LittleEndian.PutUint16(hdr[20:22], 1)
			binary.LittleEndian.PutUint16(hdr[22:24], 2)
			binary.LittleEndian.PutUint32(hdr[24:28], 44100)
			binary.LittleEndian.PutUint32(hdr[28:32], 44100*4)
			binary.LittleEndian.PutUint16(hdr[32:34], 4)
			binary.LittleEndian.PutUint16(hdr[34:36], 16)
			copy(hdr[36:40], "data")
			_ = os.WriteFile(outPath, hdr, 0o644)
		}
	}
	return []byte("ffmpeg conversion ok\n"), nil
}

func TestLoopToSamplerPadValidation(t *testing.T) {
	stateDir := t.TempDir()
	uiDir := t.TempDir()
	s := New(Config{StateDir: stateDir, UIDir: uiDir}, nil)

	// 1. Deck < 1
	req := httptest.NewRequest(http.MethodPost, "/api/sampler/loop-to-pad", bytes.NewReader([]byte(`{"deck": 0, "pad": 1, "trackPath": "/tmp/test.wav", "inSample": 0, "outSample": 44100}`)))
	w := httptest.NewRecorder()
	s.handleSamplerLoopToPad(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for deck 0, got %d", w.Code)
	}

	// 2. Pad < 1
	req = httptest.NewRequest(http.MethodPost, "/api/sampler/loop-to-pad", bytes.NewReader([]byte(`{"deck": 1, "pad": 0, "trackPath": "/tmp/test.wav", "inSample": 0, "outSample": 44100}`)))
	w = httptest.NewRecorder()
	s.handleSamplerLoopToPad(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for pad 0, got %d", w.Code)
	}

	// 3. Track not found
	req = httptest.NewRequest(http.MethodPost, "/api/sampler/loop-to-pad", bytes.NewReader([]byte(`{"deck": 1, "pad": 1, "trackPath": "/tmp/nonexistent_track.wav", "inSample": 0, "outSample": 44100}`)))
	w = httptest.NewRecorder()
	s.handleSamplerLoopToPad(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing track, got %d", w.Code)
	}

	// 4. InSample < 0 or OutSample <= InSample
	existingFile := filepath.Join(t.TempDir(), "dummy.wav")
	createSyntheticWav(t, existingFile, 44100, 1000)

	req = httptest.NewRequest(http.MethodPost, "/api/sampler/loop-to-pad", bytes.NewReader([]byte(`{"deck": 1, "pad": 1, "trackPath": "`+existingFile+`", "inSample": 500, "outSample": 500}`)))
	w = httptest.NewRecorder()
	s.handleSamplerLoopToPad(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for outSample <= inSample, got %d", w.Code)
	}
}

func TestLoopToSamplerPadDirectWavSlicing(t *testing.T) {
	stateDir := t.TempDir()
	uiDir := t.TempDir()
	mediaDir := t.TempDir()

	s := New(Config{StateDir: stateDir, UIDir: uiDir, MediaRoot: mediaDir}, nil)

	// Create test track on USB drive under MediaRoot: mediaDir/USB1/Music/track.wav
	usbDrive := filepath.Join(mediaDir, "USB1")
	trackPath := filepath.Join(usbDrive, "Music", "club_track.wav")
	sampleRate := 44100
	numFrames := 220500 // 5 seconds
	createSyntheticWav(t, trackPath, sampleRate, numFrames)

	// Slice [44100, 132300) -> 88200 frames = exactly 2.0 seconds
	inSample := int64(44100)
	outSample := int64(132300)
	body := map[string]any{
		"deck":       1,
		"pad":        2,
		"trackPath":  trackPath,
		"inSample":   inSample,
		"outSample":  outSample,
		"sampleRate": sampleRate,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/sampler/loop-to-pad", bytes.NewReader(bodyBytes))
	w := httptest.NewRecorder()
	s.handleSamplerLoopToPad(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp LoopToPadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if !resp.OK || resp.Deck != 1 || resp.Pad != 2 || resp.Samples != 88200 || resp.DurationSec != 2.0 {
		t.Fatalf("unexpected response: %+v", resp)
	}

	// Verify output destination: should be placed in "<usbDrive>/Samples from loops/Loop_Deck1_Pad2.wav"
	expectedDir := filepath.Join(usbDrive, "Samples from loops")
	expectedFile := filepath.Join(expectedDir, "Loop_Deck1_Pad2.wav")
	if resp.Path != expectedFile {
		t.Fatalf("expected path %s, got %s", expectedFile, resp.Path)
	}

	// Read and verify the sliced output WAV file
	outData, err := os.ReadFile(resp.Path)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}
	if len(outData) != 44+88200*4 {
		t.Fatalf("expected WAV file size 44 + 352800 = %d, got %d", 44+88200*4, len(outData))
	}

	// Verify WAV header
	if string(outData[0:4]) != "RIFF" || string(outData[8:12]) != "WAVE" {
		t.Fatalf("invalid RIFF/WAVE header")
	}
	audioFormat := binary.LittleEndian.Uint16(outData[20:22])
	channels := binary.LittleEndian.Uint16(outData[22:24])
	sr := binary.LittleEndian.Uint32(outData[24:28])
	bits := binary.LittleEndian.Uint16(outData[34:36])
	if audioFormat != 1 || channels != 2 || sr != 44100 || bits != 16 {
		t.Fatalf("invalid fmt: format=%d, ch=%d, sr=%d, bits=%d", audioFormat, channels, sr, bits)
	}

	// Verify first frame: corresponds to original frame 44100
	firstLeft := int16(binary.LittleEndian.Uint16(outData[44:46]))
	firstRight := int16(binary.LittleEndian.Uint16(outData[46:48]))
	expectedLeft := int16(44100 & 0x7fff)
	expectedRight := int16((44100 * 2) & 0x7fff)
	if firstLeft != expectedLeft || firstRight != expectedRight {
		t.Fatalf("first frame mismatch: got (%d, %d), expected (%d, %d)", firstLeft, firstRight, expectedLeft, expectedRight)
	}
}

func TestLoopToSamplerPadEmptySliceRejected(t *testing.T) {
	mediaDir := t.TempDir()
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir(), MediaRoot: mediaDir}, nil)
	trackPath := filepath.Join(mediaDir, "USB1", "Music", "short.wav")
	createSyntheticWav(t, trackPath, 44100, 1000)

	// inSample lies beyond the 1000 frames in the file
	body, _ := json.Marshal(map[string]any{"deck": 1, "pad": 3, "trackPath": trackPath,
		"inSample": 50000, "outSample": 60000, "sampleRate": 44100})
	w := httptest.NewRecorder()
	s.handleSamplerLoopToPad(w, httptest.NewRequest(http.MethodPost, "/api/sampler/loop-to-pad", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(mediaDir, "USB1", "Samples from loops", "Loop_Deck1_Pad3.wav")); err == nil {
		t.Fatalf("empty WAV must not be written")
	}
}

func TestLoopToSamplerPadFFmpegFallback(t *testing.T) {
	mockRunner := &mockSamplerRunner{}
	SetSamplerRunner(mockRunner.Run)
	defer SetSamplerRunner(nil)

	stateDir := t.TempDir()
	uiDir := t.TempDir()
	s := New(Config{StateDir: stateDir, UIDir: uiDir}, nil)

	// Create a dummy MP3 file
	mp3File := filepath.Join(t.TempDir(), "audio.mp3")
	_ = os.WriteFile(mp3File, []byte("fake mp3 audio stream content"), 0o644)

	body := map[string]any{
		"deck":       2,
		"pad":        4,
		"trackPath":  mp3File,
		"inSample":   44100,
		"outSample":  88200,
		"sampleRate": 44100,
	}
	b, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/sampler/loop-to-pad", bytes.NewReader(b))
	w := httptest.NewRecorder()
	s.handleSamplerLoopToPad(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp LoopToPadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if !resp.OK || resp.Deck != 2 || resp.Pad != 4 || resp.Samples != 44100 || resp.DurationSec != 1.0 {
		t.Fatalf("unexpected response: %+v", resp)
	}

	// Verify ffmpeg command was invoked
	mockRunner.mu.Lock()
	defer mockRunner.mu.Unlock()
	if len(mockRunner.commands) == 0 {
		t.Fatalf("expected ffmpeg runner command to be invoked")
	}
	cmd := mockRunner.commands[0]
	if cmd[0] != "ffmpeg" {
		t.Fatalf("expected ffmpeg command, got: %s", cmd[0])
	}
}
