package webui

// Write rekordbox's analysis before returning a computer's track list to Engine.
// The native hook fills Engine's rows on its own connection as Engine reads them.

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/Cd1s/EngineX-OS/internal/smbclient"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// Use a new cache directory so an upgrade does not reuse the old grids and cues.
var rbxAnalysisDir = "/run/enginex-rbx-analysis-v3"

// rbxAnalysing are the tracks whose analysis is being read; rbxAnalysisJobs waits for them.
var (
	rbxAnalysing    sync.Map
	rbxAnalysisJobs sync.WaitGroup
)

// rbxAnalysisMagic heads the file: then trackData, overviewWaveFormData, beatData,
// quickCues and loops, each a little-endian uint32 length and its bytes. The
// last byte is EncodePerformance's analysed flag, absent in older files.
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
	defer rbxAnalysisJobs.Done()
	if rbxAnalysisReady(id) {
		return
	}
	if _, running := rbxAnalysing.LoadOrStore(id, true); running {
		return
	}
	defer rbxAnalysing.Delete(id)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rbxSession.Lock()
	link, err := g.s.rbxDial(ctx, ip)
	if err != nil {
		rbxSession.Unlock()
		log.Printf("stream-sync: load track=%d provider=rekordbox step=analysis result=failed error=%v", id, err)
		return
	}
	t, a := rbxReadAnalysis(link, tid, rbPath)
	link.Close()
	rbxSession.Unlock()
	rbxHeader(file, &t)
	_, slot := rbxMedia(ip)
	rbxStoreAnalysis(id, t, a, slot == 0)
}

func rbxAnalysisPath(id int64) string {
	return filepath.Join(rbxAnalysisDir, strconv.FormatInt(id, 10)+".bin")
}

func rbxAnalysisReady(id int64) bool {
	_, err := os.Stat(rbxAnalysisPath(id))
	return err == nil
}

func rbxReadAnalysis(link rbxLink, tid uint32, rbPath string) (rekordbox.Track, *rekordbox.Analysis) {
	var meta []prolink.RekordboxItem
	if m, ok := link.(interface {
		Metadata(uint32) ([]prolink.RekordboxItem, error)
	}); ok {
		meta, _ = m.Metadata(tid)
	}
	return rbxTrackFacts(meta, tid, rbPath), rbxAnalysis(link, tid)
}

func rbxStoreAnalysis(id int64, t rekordbox.Track, a *rekordbox.Analysis, replay bool) {
	if rbxAnalysisReady(id) || t.SampleRate <= 0 || t.SampleCount == 0 {
		return
	}
	if len(a.Beats) > 0 && a.Beats[0].BPM >= 499.5 {
		a.Beats = nil
	}
	if t.BPM == 0 && len(a.Beats) > 0 {
		t.BPM = a.Beats[0].BPM
	}
	p, err := enginedb.EncodePerformance(t, a)
	if err != nil || len(p.Beats) == 0 {
		log.Printf("stream-sync: load track=%d provider=rekordbox step=analysis result=none error=%v", id, err)
		return
	}
	var b bytes.Buffer
	b.WriteString(rbxAnalysisMagic)
	for _, blob := range [][]byte{p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops} {
		_ = binary.Write(&b, binary.LittleEndian, uint32(len(blob)))
		b.Write(blob)
	}
	if p.Complete {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	if err := os.MkdirAll(rbxAnalysisDir, 0o755); err != nil {
		return
	}
	out := rbxAnalysisPath(id)
	// Engine can ask OfflineAnalyzer to analyse a newly downloaded stream
	// even after it has read the same track's PerformanceData from stm.
	if replay && p.Complete {
		if perf, err := perfFromBlobs(t.BPM, p.TrackData, p.Beats, p.Overview); err == nil {
			savePerf(out, perf)
		}
	}
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o644); err != nil || os.Rename(tmp, out) != nil {
		os.Remove(tmp)
		return
	}
	hot := 0
	for _, cue := range a.Cues {
		if cue.HotCue > 0 {
			hot++
		}
	}
	log.Printf("stream-sync: load track=%d provider=rekordbox step=analysis result=ready rate=%d grid=%d cues=%d hot=%d overview=%d", id, t.SampleRate, len(p.Beats), len(a.Cues), hot, len(p.Overview))
}

// rbxPrefetch runs before publishing the page: Engine snapshots the analysed
// state while it ingests the list and may never read the row again at load.
// Bounded work on one dbserver session keeps slow or large libraries browseable.
func (g *tidalGateway) rbxPrefetch(ctx context.Context, ip string, rows []prolink.RekordboxTrack, items []any, offset int) (ready bool) {
	if _, slot := rbxMedia(ip); slot != 0 || len(items) == 0 {
		return false // keep the CDJ media load path as before
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var link rbxLink
	defer func() {
		if link != nil {
			link.Close()
			rbxSession.Unlock()
		}
	}()
	for i, item := range items {
		if ctx.Err() != nil || i == 48 {
			break
		}
		id := item.(map[string]any)["id"].(int64)
		if rbxAnalysisReady(id) {
			ready = true
			continue
		}
		if link == nil {
			if !rbxSession.TryLock() {
				break
			}
			var err error
			link, err = g.s.rbxDial(ctx, ip)
			if err != nil {
				rbxSession.Unlock()
				break
			}
			if _, ok := link.(interface {
				Analysis(uint32, string, string) ([]byte, error)
			}); !ok {
				break
			}
		}
		tid := rows[i+offset].ID
		rbPath, err := link.Path(tid)
		if err != nil {
			continue
		}
		if _, ok := localCodecs[strings.ToLower(path.Ext(rbxPath(rbPath)))]; !ok {
			continue
		}
		info, err := g.rbxRemoteHeader(ctx, ip, rbPath)
		if err != nil || info.SampleRate <= 0 || info.Samples == 0 {
			continue
		}
		t, a := rbxReadAnalysis(link, tid, rbPath)
		rbxApplyHeader(&t, info)
		if ctx.Err() == nil {
			if _, running := rbxAnalysing.LoadOrStore(id, true); !running {
				rbxStoreAnalysis(id, t, a, true)
				rbxAnalysing.Delete(id)
				ready = rbxAnalysisReady(id) || ready
			}
		}
	}
	return ready
}

func (g *tidalGateway) rbxRemoteHeader(ctx context.Context, ip, rbPath string) (compat.AudioInfo, error) {
	p := rbxPath(rbPath)
	if file, _ := g.s.rbxShareFile(rbPath); file != "" {
		return compat.AudioHeader(file)
	}
	// Try the computer's Pro DJ Link export without starting a staged copy.
	c := rbxNFS(net.ParseIP(ip))
	nfsCtx, cancel := context.WithTimeout(ctx, 350*time.Millisecond)
	defer cancel()
	for want := uint32(32 << 10); want <= 2<<20; want *= 4 {
		b, file, err := c.FetchPrefix(nfsCtx, rbxExport, p, want)
		if err != nil {
			break
		}
		info, err := rbxParsePrefix(p, b, int64(file.Size))
		// FLAC's STREAMINFO already gives both values even if the prefix
		// ends before a later metadata block after the cover art.
		if want < file.Size && info.Format == "flac" && info.SampleRate > 0 && info.Samples > 0 {
			return info, nil
		}
		if err == nil || want >= file.Size {
			return info, err
		}
	}
	remote := g.rbxFindSMB(ctx, rbPath)
	if remote == nil {
		return compat.AudioInfo{}, nil
	}
	smb, err := smbclient.Dial(ctx, remote.mount.smbURL(), remote.mount.Username, remote.mount.Password, nil)
	if err != nil {
		return compat.AudioInfo{}, err
	}
	defer smb.Close()
	f, err := smb.Share.Open(remote.path)
	if err != nil {
		return compat.AudioInfo{}, err
	}
	defer f.Close()
	var limit int64 = 32 << 10
	if compat.IsMP4AudioPath(p) {
		limit = 512 << 10
	}
	b, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return compat.AudioInfo{}, err
	}
	return rbxParsePrefix(p, b, remote.info.Size())
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
// Cover art before the audio (a FLAC PICTURE block, an MP3's ID3 tag) can be megabytes, so
// a header that does not read whole is read again from four times as many bytes.
func rbxHeader(file string, t *rekordbox.Track) {
	for want := int64(rbxHeadBytes); ; want *= 4 {
		info, size, err := rbxHead(file, want)
		if err == nil && info.SampleRate > 0 {
			rbxApplyHeader(t, info)
			return
		}
		if size <= 0 || want >= size {
			return
		}
	}
}

func rbxApplyHeader(t *rekordbox.Track, info compat.AudioInfo) {
	t.SampleRate, t.StartOffsetSamples = info.SampleRate, info.StartOffsetSamples
	if info.Duration > 0 {
		t.Duration = info.Duration
	}
	if info.Samples > 0 {
		t.SampleCount = info.Samples
	}
}

// rbxHead reads the header of file from its first want bytes.
func rbxHead(file string, want int64) (compat.AudioInfo, int64, error) {
	size := int64(0)
	src := file
	if cp := rbxRunning(file); cp != nil {
		size, src = cp.size, cp.tmp
		want = min(size, want)
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
			return compat.AudioInfo{}, size, err
		}
	}
	head := make([]byte, min(size, want))
	n, _ := f.ReadAt(head, 0)
	f.Close()
	info, err := rbxParsePrefix(file, head[:n], size)
	return info, size, err
}

func rbxParsePrefix(file string, head []byte, size int64) (compat.AudioInfo, error) {
	if compat.IsMP4AudioPath(file) {
		return compat.MP4AudioHeaderPrefix(bytes.NewReader(head), size)
	}
	tmp, err := os.CreateTemp("", "rbx-head-*"+filepath.Ext(file))
	if err != nil {
		return compat.AudioInfo{}, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(head)
	if err == nil {
		err = tmp.Truncate(size)
	}
	tmp.Close()
	if err != nil {
		return compat.AudioInfo{}, err
	}
	return compat.AudioHeader(tmp.Name())
}

// rbxAnalysis is the track's rekordbox analysis over dbserver. rekordbox 7 answers 0x2c04
// with the tag's sections, each behind its little-endian length (padded; the section's own
// length cuts it), read as one analysis file; it has no PQTZ there (status 50), so the grid
// comes from 0x2204 (live probe, rbx-probe mac-run-4).
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
		if err != nil {
			continue
		}
		file := make([]byte, 28, 28+len(b))
		copy(file, "PMAI")
		binary.BigEndian.PutUint32(file[4:], 28)
		for len(b) >= 4+12 {
			n := uint64(binary.LittleEndian.Uint32(b))
			s := b[4:]
			if n > uint64(len(s)) {
				break
			}
			if own := uint64(binary.BigEndian.Uint32(s[8:])); own <= n {
				file = append(file, s[:own]...)
			}
			b = s[n:]
		}
		if len(file) == 28 {
			continue
		}
		binary.BigEndian.PutUint32(file[8:], uint32(len(file)))
		if s, err := rekordbox.ParseAnalysis(file); err == nil {
			a.Merge(s)
		}
	}
	if g, ok := link.(interface{ BeatGrid(uint32) ([]byte, error) }); ok && len(a.Beats) == 0 {
		if b, err := g.BeatGrid(id); err == nil {
			a.Beats = rbxBeatGrid(b)
		}
	}
	// rekordbox 7 sends its PCO2 sections empty: the cues come only as the cue list.
	if c, ok := link.(interface{ CuesExtended(uint32) ([]byte, error) }); ok && len(a.Cues) == 0 {
		if b, err := c.CuesExtended(id); err == nil {
			a.Merge(&rekordbox.Analysis{Cues: rbxCueList(b)})
		}
	}
	return a
}

// rbxCueList reads 0x2b04's cues as the PCO2 entries of an export give them: per entry
// its little-endian size, the hot cue (0 a memory cue) at 4, 1 cue / 2 loop at 6, start
// and end in ms at 12 and 16, colour ID at 0x22, then the comment's size at 0x48 and its
// UTF-16LE text, the colour code and RGB after it (beat-link's CueList).
func rbxCueList(b []byte) []rekordbox.Cue {
	var cues []rekordbox.Cue
	for len(b) >= 0x24 {
		n := int(binary.LittleEndian.Uint32(b))
		if n < 0x24 || n > len(b) {
			break
		}
		e := b[:n]
		b = b[n:]
		if e[6] == 0 && e[4] == 0 {
			continue
		}
		c := rekordbox.Cue{HotCue: uint32(e[4]), Loop: e[6] == 2, TimeMS: binary.LittleEndian.Uint32(e[12:]), EndMS: binary.LittleEndian.Uint32(e[16:]), ColorID: e[0x22], Extended: true}
		if c.TimeMS == 0xffffffff || c.Loop && c.EndMS == 0xffffffff {
			continue
		}
		if n > 0x49 {
			cs := int(binary.LittleEndian.Uint16(e[0x48:]))
			if cs >= 2 && 0x4a+cs <= n {
				u := make([]uint16, (cs-2)/2)
				for i := range u {
					u[i] = binary.LittleEndian.Uint16(e[0x4a+2*i:])
				}
				c.Name = string(utf16.Decode(u))
			}
			if q := 0x4e + cs; q+4 <= n {
				c.ColorCode = e[q]
				c.Color = [4]byte{255, e[q+1], e[q+2], e[q+3]}
			}
		}
		cues = append(cues, c)
	}
	return cues
}

// rbxBeatGrid reads 0x2204's grid: 20 bytes, then per beat 16 (little-endian beat in the
// bar, tempo times 100, time in ms).
func rbxBeatGrid(b []byte) []rekordbox.Beat {
	var beats []rekordbox.Beat
	for p := 20; p+16 <= len(b); p += 16 {
		beats = append(beats, rekordbox.Beat{Number: binary.LittleEndian.Uint16(b[p:]), BPM: float64(binary.LittleEndian.Uint16(b[p+2:])) / 100, TimeMS: binary.LittleEndian.Uint32(b[p+4:])})
	}
	return beats
}
