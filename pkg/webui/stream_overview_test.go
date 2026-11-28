package webui

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/analyzer"
	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

func writeTestWAV(t *testing.T, path string, secs int) {
	t.Helper()
	sr := 44100
	n := sr * secs
	dataBytes := n * 4
	buf := make([]byte, 44+dataBytes)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataBytes))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1)
	binary.LittleEndian.PutUint16(buf[22:24], 2)
	binary.LittleEndian.PutUint32(buf[24:28], uint32(sr))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(sr*4))
	binary.LittleEndian.PutUint16(buf[32:34], 4)
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataBytes))
	for i := 0; i < n; i++ {
		s := int16(18000 * math.Sin(2*math.Pi*200*float64(i)/float64(sr)))
		binary.LittleEndian.PutUint16(buf[44+i*4:], uint16(s))
		binary.LittleEndian.PutUint16(buf[44+i*4+2:], uint16(s))
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStreamOverviewGatewayScope(t *testing.T) {
	tmp := t.TempDir()
	oldDir := streamOverviewDir
	streamOverviewDir = filepath.Join(tmp, "ov")
	defer func() { streamOverviewDir = oldDir }()

	wav := filepath.Join(tmp, "deezer.wav")
	writeTestWAV(t, wav, 5)
	mp4 := filepath.Join(tmp, "am.mp4")
	if err := os.WriteFile(mp4, []byte("\x00\x00\x00\x18ftypM4A \x00\x00\x00\x00M4A isom"), 0o644); err != nil {
		t.Fatal(err)
	}

	jobs := NewJobs(Pipeline{
		Guard:   resourceguard.New(resourceguard.Config{Disabled: true}),
		Catalog: &providers.Deezer{},
		Deemix:  &deemixclient.Client{DownloadDir: tmp},
	})
	defer jobs.Close()
	s := New(Config{StateDir: filepath.Join(tmp, "state")}, jobs)
	defer s.Close()

	// 1. Streamed non-rekordbox track produces a valid EXOV1 sidecar.
	deezerTrack := providers.Track{ID: "dz-1", Provider: "deezer", Title: "Song"}
	jobs.mu.Lock()
	jobs.files[trackKey(deezerTrack)] = fetched{matched: deezerTrack, dl: &deemixclient.Download{Path: wav}}
	jobs.mu.Unlock()
	id := s.tidal.addTrack(trackRef{track: deezerTrack})

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/stream/%d.wav", id), nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	s.tidal.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream GET status %d: %s", rec.Code, rec.Body.String())
	}

	fp, err := analyzer.OverviewFileFingerprint(wav)
	if err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(streamOverviewDir, fp+".ov")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sidecar); err == nil {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	b, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("expected sidecar at %s for id=%d: %v", sidecar, id, err)
	}
	if _, _, err := analyzer.ParseOverviewSidecar(b); err != nil {
		t.Fatalf("invalid sidecar written by gateway: %v", err)
	}

	// 2. Drive track (ref.file != "") and rekordbox track MUST NOT write any sidecar.
	driveWav := filepath.Join(tmp, "drive.wav")
	writeTestWAV(t, driveWav, 4)
	s.enqueueStreamOverview(2, localProvider, driveWav)
	s.enqueueStreamOverview(3, rekordboxScope, driveWav)
	s.enqueueStreamOverview(4, "applemusic", mp4)
	s.stopStreamOverview()

	driveFP, _ := analyzer.OverviewFileFingerprint(driveWav)
	if _, err := os.Stat(filepath.Join(streamOverviewDir, driveFP+".ov")); !os.IsNotExist(err) {
		t.Fatalf("unexpected sidecar for local/rekordbox track")
	}
	mp4FP, _ := analyzer.OverviewFileFingerprint(mp4)
	if _, err := os.Stat(filepath.Join(streamOverviewDir, mp4FP+".ov")); !os.IsNotExist(err) {
		t.Fatalf("unexpected sidecar for mp4 track")
	}

	_ = context.Background()
}
