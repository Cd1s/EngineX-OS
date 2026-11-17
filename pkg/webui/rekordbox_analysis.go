package webui

// rekordbox's analysis for Engine's streaming load. Engine analyses a streamed track
// unless the track's row in its streaming database (stm) already holds PerformanceData:
// a track loaded a second time is not analysed again. When a deck asks for a rekordbox
// track, this writes rekordbox's beat grid, cues, loops and waveform, as the blobs
// rb-bridge writes into a drive's m.db, to rbxAnalysisDir/<track>.bin; Engine's native
// hook puts them into that row through Engine's own connection before Engine reads it.

import (
	"bytes"
	"context"
	"encoding/binary"
	"log"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// rbxAnalysisDir is where the hook reads them (tmpfs). A variable for the tests.
var rbxAnalysisDir = "/run/enginex-rbx-analysis"

// rbxAnalysisMagic heads the file: then trackData, overviewWaveFormData, beatData,
// quickCues and loops, each a little-endian uint32 length and its bytes.
const rbxAnalysisMagic = "RBXA1"

// rbxMarkLoading opens the hook for a while: only a deck's request for a Pro DJ Link track
// does, so a drive's load stays as stock (the hook does not even read then).
func rbxMarkLoading() {
	mark := filepath.Join(rbxAnalysisDir, ".loading")
	if err := os.MkdirAll(rbxAnalysisDir, 0o755); err != nil {
		return
	}
	now := time.Now()
	if os.Chtimes(mark, now, now) != nil {
		_ = os.WriteFile(mark, nil, 0o644)
	}
}

// rbxWriteAnalysis writes track id's analysis (the gateway's id, the one in Engine's uri)
// for rekordbox track tid of the library ip (a computer, or a player's medium), whose audio is file.
func (g *tidalGateway) rbxWriteAnalysis(id int64, ip string, tid uint32, rbPath, file string) {
	out := filepath.Join(rbxAnalysisDir, strconv.FormatInt(id, 10)+".bin")
	if _, err := os.Stat(out); err == nil {
		return
	}
	var meta []prolink.RekordboxItem
	a := &rekordbox.Analysis{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rbxSession.Lock()
	link, err := g.s.rbxDial(ctx, ip)
	if err == nil {
		if m, ok := link.(interface {
			Metadata(uint32) ([]prolink.RekordboxItem, error)
		}); ok {
			meta, _ = m.Metadata(tid)
		}
		a = rbxAnalysis(link, tid)
		link.Close()
	}
	rbxSession.Unlock()
	if err != nil {
		log.Printf("stream-sync: load track=%d provider=rekordbox step=analysis result=failed error=%v", id, err)
		return
	}
	t := rbxTrackFacts(meta, tid, rbPath)
	rbxHeader(file, &t)
	p, err := enginedb.EncodePerformance(t, a)
	if err != nil || len(p.Beats) == 0 && len(p.Overview) == 0 {
		log.Printf("stream-sync: load track=%d provider=rekordbox step=analysis result=none error=%v", id, err)
		return
	}
	var b bytes.Buffer
	b.WriteString(rbxAnalysisMagic)
	for _, blob := range [][]byte{p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops} {
		_ = binary.Write(&b, binary.LittleEndian, uint32(len(blob)))
		b.Write(blob)
	}
	if err := os.MkdirAll(rbxAnalysisDir, 0o755); err != nil {
		return
	}
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o644); err != nil || os.Rename(tmp, out) != nil {
		os.Remove(tmp)
		return
	}
	log.Printf("stream-sync: load track=%d provider=rekordbox step=analysis result=ready rate=%d grid=%d cues=%d overview=%d", id, t.SampleRate, len(p.Beats), len(a.Cues), len(p.Overview))
}

// rbxTrackFacts reads a track's metadata rows (kind 4 title, 7 artist, 2 album, 0xb
// length in seconds, 0xd tempo times 100, 0xf key).
func rbxTrackFacts(rows []prolink.RekordboxItem, id uint32, rbPath string) rekordbox.Track {
	t := rekordbox.Track{ID: id, Path: rbPath, Title: path.Base(rbxPath(rbPath))}
	for _, it := range rows {
		switch it.Kind & 0xff {
		case 4:
			t.Title = it.Label
		case 7:
			t.Artist = it.Label
		case 2:
			t.Album = it.Label
		case 0xb:
			t.Duration = float64(it.ID)
		case 0xd:
			t.BPM = float64(it.ID) / 100
		case 0xf:
			t.Key = it.Label
		}
	}
	return t
}

const rbxHeadBytes = 512 << 10

// rbxHeader takes the sample rate and length from the file's header: its first bytes
// (waited for while a copy runs), in a file of the real size so the header reads whole.
func rbxHeader(file string, t *rekordbox.Track) {
	size := int64(0)
	src := file
	if cp := rbxRunning(file); cp != nil {
		size, src = cp.size, cp.tmp
		want := min(size, rbxHeadBytes)
		deadline := time.Now().Add(rbxProbeTime * 5)
		cp.mu.Lock()
		for cp.got < want && !cp.done && time.Now().Before(deadline) {
			cp.mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			cp.mu.Lock()
		}
		cp.mu.Unlock()
	} else if st, err := os.Stat(file); err == nil {
		size = st.Size()
	}
	f, err := os.Open(src)
	if err != nil {
		if f, err = os.Open(file); err != nil {
			return
		}
	}
	head := make([]byte, min(size, rbxHeadBytes))
	n, _ := f.ReadAt(head, 0)
	f.Close()
	tmp, err := os.CreateTemp("", "rbx-head-*"+filepath.Ext(file))
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(head[:n])
	if err == nil {
		err = tmp.Truncate(size)
	}
	tmp.Close()
	if err != nil {
		return
	}
	info, err := compat.AudioHeader(tmp.Name())
	if err != nil || info.SampleRate <= 0 {
		return
	}
	t.SampleRate, t.StartOffsetSamples = info.SampleRate, info.StartOffsetSamples
	if info.Duration > 0 {
		t.Duration = info.Duration
	}
	if info.Samples > 0 {
		t.SampleCount = info.Samples
	}
}

// rbxAnalysis is the track's rekordbox analysis over dbserver: each ANLZ tag rekordbox
// hands out alone, read as a one-section analysis file and merged.
func rbxAnalysis(link rbxLink, id uint32) *rekordbox.Analysis {
	if m, ok := link.(interface {
		AnalysisOf(uint32) *rekordbox.Analysis
	}); ok {
		return m.AnalysisOf(id) // a player's medium: its ANLZ files
	}
	a := &rekordbox.Analysis{}
	l, ok := link.(interface {
		Analysis(uint32, string, string) ([]byte, error)
	})
	if !ok {
		return a
	}
	for _, tf := range [][2]string{{"PQTZ", "DAT"}, {"PCO2", "EXT"}, {"PCOB", "DAT"}, {"PWV7", "2EX"}, {"PWV5", "EXT"}, {"PWAV", "DAT"}} {
		b, err := l.Analysis(id, tf[0], tf[1])
		if err != nil || len(b) < 12 {
			continue
		}
		file := make([]byte, 28, 28+len(b))
		copy(file, "PMAI")
		binary.BigEndian.PutUint32(file[4:], 28)
		binary.BigEndian.PutUint32(file[8:], uint32(28+len(b)))
		if s, err := rekordbox.ParseAnalysis(append(file, b...)); err == nil {
			a.Merge(s)
		}
	}
	return a
}
