package webui

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// analysedRbxLink is the computer's library with track facts and analysis over dbserver.
type analysedRbxLink struct{ fakeRbxLink }

func (analysedRbxLink) Metadata(id uint32) ([]prolink.RekordboxItem, error) {
	return []prolink.RekordboxItem{{Kind: 4, Label: "First"}, {Kind: 7, Label: "Artist One"}, {Kind: 0xb, ID: 3}, {Kind: 0xd, ID: 12000}}, nil
}

// Analysis answers as rekordbox 7 does (live probe, rbx-probe mac-run-4): no PQTZ over 0x2c04
// (status 50), and each section behind its little-endian length, padded to 4 bytes.
func (analysedRbxLink) Analysis(id uint32, tag, file string) ([]byte, error) {
	if tag+"."+file != "PCOB.DAT" {
		return nil, nil
	}
	// hot cue A at 2 s
	b := make([]byte, 24+56)
	copy(b, "PCOB")
	binary.BigEndian.PutUint32(b[4:], 24)
	binary.BigEndian.PutUint32(b[8:], uint32(len(b)))
	binary.BigEndian.PutUint32(b[12:], 1)
	binary.BigEndian.PutUint16(b[18:], 1)
	p := b[24:]
	copy(p, "PCPT")
	binary.BigEndian.PutUint32(p[4:], 28)
	binary.BigEndian.PutUint32(p[8:], 56)
	binary.BigEndian.PutUint32(p[12:], 1)
	p[28] = 1
	binary.BigEndian.PutUint32(p[32:], 2000)
	binary.BigEndian.PutUint32(p[36:], 0xffffffff)
	out := binary.LittleEndian.AppendUint32(nil, uint32(len(b)+4))
	return append(append(out, b...), 0, 0, 0, 0), nil
}

// BeatGrid is 0x2204's grid: beats at 0.5 s and 1 s, 120 BPM.
func (analysedRbxLink) BeatGrid(id uint32) ([]byte, error) {
	b := make([]byte, 20+2*16)
	for i, ms := range []uint32{500, 1000} {
		p := b[20+16*i:]
		binary.LittleEndian.PutUint16(p, uint16(i+1))
		binary.LittleEndian.PutUint16(p[2:], 12000)
		binary.LittleEndian.PutUint32(p[4:], ms)
	}
	return b, nil
}

// flac48k is a FLAC file at 48 kHz, 3 s, padded to size: only its header is real.
func flac48k(size int) []byte {
	b := make([]byte, size)
	copy(b, "fLaC")
	b[4], b[7] = 0x80, 34 // last block, STREAMINFO
	si := uint64(48000)<<44 | uint64(1)<<41 | uint64(15)<<36 | 3*48000
	binary.BigEndian.PutUint32(b[8:], 4096<<16|4096)
	binary.BigEndian.PutUint64(b[8+10:], si)
	return b
}

type rbxPrefetchLink struct {
	analysedRbxLink
	rows  []prolink.RekordboxTrack
	path  func(uint32)
	close func()
}

func (l rbxPrefetchLink) Tracks() ([]prolink.RekordboxTrack, error) { return l.rows, nil }
func (l rbxPrefetchLink) Path(id uint32) (string, error) {
	l.path(id)
	return l.analysedRbxLink.Path(id)
}
func (l rbxPrefetchLink) Close() { l.close() }

func TestRekordboxPrefetchContinuesPastPageAfterResponse(t *testing.T) {
	const tracks = 52
	paths := make(map[uint32]string)
	rows := make([]prolink.RekordboxTrack, tracks)
	for i := range rows {
		id := uint32(i + 1)
		paths[id] = "/Users/dj/Music/same.flac"
		rows[i] = prolink.RekordboxTrack{ID: id, Title: strconv.Itoa(i + 1)}
	}
	e := newRbxEnv(t, paths)
	if err := os.WriteFile(filepath.Join(e.share, "same.flac"), flac48k(4096), 0o644); err != nil {
		t.Fatal(err)
	}
	old := rbxAnalysisDir
	rbxAnalysisDir = t.TempDir()
	t.Cleanup(func() { rbxAnalysisDir = old })
	started, release := make(chan struct{}), make(chan struct{})
	var startOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(rbxAnalysisJobs.Wait)
	t.Cleanup(unblock)
	var active atomic.Int32
	dialRekordbox = func(ctx context.Context, _ net.IP, _ byte) (rbxLink, error) {
		if active.Add(1) != 1 {
			t.Error("more than one dbserver session")
		}
		return rbxPrefetchLink{
			analysedRbxLink: analysedRbxLink{fakeRbxLink{paths}}, rows: rows,
			path: func(id uint32) {
				if id != 49 {
					return
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 2*time.Second {
					t.Error("background dbserver read has no two-second budget")
				}
				startOnce.Do(func() { close(started) })
				select {
				case <-release:
				case <-ctx.Done():
				}
			},
			close: func() { active.Add(-1) },
		}, nil
	}
	ref := playlistRef{}
	ref.list.ID = rbxListID("127.0.0.1", "all")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest("GET", "/?limit=50", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		e.s.tidal.rbxTracks(response, request, ref)
		cancel() // net/http cancels when the handler returns.
	}()
	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("background read blocked the list response")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("prefetch stopped at 48 tracks")
	}
	var page tidalList
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || len(page.Items) != 50 {
		t.Fatalf("response: %d tracks, %v", len(page.Items), err)
	}
	unblock()
	rbxAnalysisJobs.Wait()
	files, err := filepath.Glob(filepath.Join(rbxAnalysisDir, "*.bin"))
	if err != nil || len(files) != tracks {
		t.Fatalf("cached %d of %d tracks: %v", len(files), tracks, err)
	}
}

func TestRekordboxPrefetchCancelsOldList(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(*rbxEnv)
	}{
		{"newer list", func(e *rbxEnv) {
			ref := playlistRef{}
			ref.list.ID = rbxListID("127.0.0.1", "9")
			e.s.tidal.rbxTracks(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), ref)
		}},
		{"source removed", func(e *rbxEnv) {
			os.Remove(e.s.ProlinkStatePath())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ENGINEX_PROLINK_STATE", filepath.Join(t.TempDir(), "none.json"))
			paths := map[uint32]string{1: "/Users/dj/Music/x.flac", 2: "/Users/dj/Music/x.flac", 3: "/Users/dj/Music/x.flac"}
			e := newRbxEnv(t, paths)
			if err := os.WriteFile(filepath.Join(e.share, "x.flac"), flac48k(4096), 0o644); err != nil {
				t.Fatal(err)
			}
			old := rbxAnalysisDir
			rbxAnalysisDir = t.TempDir()
			t.Cleanup(func() { rbxAnalysisDir = old })
			t.Cleanup(rbxAnalysisJobs.Wait)
			started, unstick := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var cancelled, reachedThird atomic.Bool
			dialRekordbox = func(ctx context.Context, _ net.IP, _ byte) (rbxLink, error) {
				return rbxPrefetchLink{
					analysedRbxLink: analysedRbxLink{fakeRbxLink{paths}},
					rows:            []prolink.RekordboxTrack{{ID: 1, Title: "A"}, {ID: 2, Title: "B"}, {ID: 3, Title: "C"}},
					path: func(id uint32) {
						if id == 3 {
							reachedThird.Store(true)
						}
						if id != 2 {
							return
						}
						once.Do(func() {
							close(started)
							select {
							case <-unstick:
							case <-ctx.Done():
								cancelled.Store(true)
							}
						})
					},
					close: func() {},
				}, nil
			}
			ref := playlistRef{}
			ref.list.ID = rbxListID("127.0.0.1", "all")
			e.s.tidal.rbxTracks(httptest.NewRecorder(), httptest.NewRequest("GET", "/?limit=1", nil), ref)
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("background read never started")
			}
			tc.stop(e)
			if tc.name == "source removed" {
				time.Sleep(550 * time.Millisecond) // pass the presence refresh interval
			}
			close(unstick)
			rbxAnalysisJobs.Wait()
			if reachedThird.Load() || (tc.name == "newer list" && !cancelled.Load()) {
				t.Fatalf("old list was not stopped: cancelled=%t third=%t", cancelled.Load(), reachedThird.Load())
			}
		})
	}
}

type rbxEditedLink struct {
	analysedRbxLink
	ms uint32
}

func (l rbxEditedLink) BeatGrid(id uint32) ([]byte, error) {
	b, err := l.analysedRbxLink.BeatGrid(id)
	binary.LittleEndian.PutUint32(b[24:], l.ms)
	return b, err
}

func (l rbxEditedLink) CuesExtended(uint32) ([]byte, error) {
	b := make([]byte, 0x24)
	binary.LittleEndian.PutUint32(b, uint32(len(b)))
	b[4], b[6] = 3, 1
	binary.LittleEndian.PutUint32(b[12:], l.ms)
	return b, nil
}

type rbxStampedLink struct {
	rbxEditedLink
	paths *atomic.Int32
}

func (l rbxStampedLink) Path(id uint32) (string, error) {
	if id == 1 {
		l.paths.Add(1)
	}
	return l.rbxEditedLink.Path(id)
}

func (l rbxStampedLink) Analysis(uint32, string, string) ([]byte, error) { return nil, nil }

func TestRekordboxFirstPrefetchSkipsSecondHeaderRead(t *testing.T) {
	paths := map[uint32]string{1: "/Users/dj/Music/x.flac"}
	e := newRbxEnv(t, paths)
	if err := os.WriteFile(filepath.Join(e.share, "x.flac"), flac48k(4096), 0o644); err != nil {
		t.Fatal(err)
	}
	old := rbxAnalysisDir
	rbxAnalysisDir = t.TempDir()
	t.Cleanup(func() { rbxAnalysisDir = old })
	t.Cleanup(rbxAnalysisJobs.Wait)
	var reads atomic.Int32
	l := rbxStampedLink{rbxEditedLink{analysedRbxLink{fakeRbxLink{paths}}, 500}, &reads}
	dialRekordbox = func(context.Context, net.IP, byte) (rbxLink, error) { return l, nil }
	ref := playlistRef{}
	ref.list.ID = rbxListID("127.0.0.1", "all")
	e.s.tidal.rbxTracks(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), ref)
	rbxAnalysisJobs.Wait()
	// Listing probes the first path once, then first-screen prefetch reads its
	// header. The follow-up fingerprint check must not resolve that path again.
	if n := reads.Load(); n != 2 {
		t.Fatalf("first track path read %d times, want 2", n)
	}
}

func TestRekordboxManifestRefreshesEditedGridAndCue(t *testing.T) {
	paths := map[uint32]string{1: "/Users/dj/Music/x.m4a"}
	e := newRbxEnv(t, paths)
	if err := os.WriteFile(filepath.Join(e.share, "x.m4a"), testfixture.AACWithPriming(), 0o644); err != nil {
		t.Fatal(err)
	}
	old := rbxAnalysisDir
	rbxAnalysisDir = t.TempDir()
	t.Cleanup(func() { rbxAnalysisDir = old })
	t.Cleanup(rbxAnalysisJobs.Wait)
	base := analysedRbxLink{fakeRbxLink{paths}}
	var edit atomic.Uint32
	dialRekordbox = func(context.Context, net.IP, byte) (rbxLink, error) {
		if ms := edit.Load(); ms != 0 {
			return rbxEditedLink{base, ms}, nil
		}
		return base, nil
	}
	ids, _ := e.trackIDs(t, "Tracks")
	rbxAnalysisJobs.Wait()
	id := ids["First"]
	out := rbxAnalysisPath(id)
	before, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	edit.Store(650)
	manifest := fmt.Sprintf("/v1/tracks/%d/playbackinfopostpaywall", id)
	tidalGet(t, e.base, manifest, 200, nil)
	after, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	l := rbxEditedLink{base, 650}
	grid, _ := l.BeatGrid(1)
	cues, _ := l.CuesExtended(1)
	want, err := enginedb.EncodePerformance(rekordbox.Track{SampleRate: 44100, SampleCount: 21435392, StartOffsetSamples: 2048},
		&rekordbox.Analysis{Beats: rbxBeatGrid(grid), Cues: rbxCueList(cues)})
	got, original := rbxTestBlobs(t, after), rbxTestBlobs(t, before)
	if err != nil || !bytes.Equal(got[2], want.Beats) || !bytes.Equal(got[3], want.QuickCues) ||
		!bytes.Equal(got[0], original[0]) || !bytes.Equal(got[1], original[1]) {
		t.Fatal("reload did not refresh both edited fields and preserve the waveform")
	}
	first, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	tidalGet(t, e.base, manifest, 200, nil)
	again, err := os.Stat(out)
	if err != nil || !os.SameFile(first, again) {
		t.Fatal("unchanged source responses rewrote the analysis")
	}
	edit.Store(750)
	tidalGet(t, e.base, manifest, 200, nil)
	previous, err := os.ReadFile(out + ".previous")
	if err != nil || !bytes.Equal(previous, after) {
		t.Fatalf("second edit did not retain the immediate predecessor: %v", err)
	}
}

func rbxTestBlobs(t *testing.T, b []byte) [5][]byte {
	t.Helper()
	r := bytes.NewReader(b)
	if _, err := r.Seek(int64(len(rbxAnalysisMagic)), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var blobs [5][]byte
	for i := range blobs {
		var len uint32
		if err := binary.Read(r, binary.LittleEndian, &len); err != nil || len > uint32(r.Len()) {
			t.Fatalf("bad analysis field %d: %v", i, err)
		}
		blobs[i] = make([]byte, len)
		if _, err := io.ReadFull(r, blobs[i]); err != nil {
			t.Fatal(err)
		}
	}
	return blobs
}

// Engine analysed every rekordbox track it streamed (maintainer: grid, cues and both
// waveforms came only after 30-45 s): a load leaves rekordbox's own analysis, as the
// m.db blobs at the file's sample rate, where the native hook hands it to Engine.
func TestRekordboxLoadLeavesItsAnalysisForEngine(t *testing.T) {
	paths := map[uint32]string{1: "/Users/dj/Music/x.flac", 2: "/Users/dj/Music/y.flac"}
	e := newRbxEnv(t, paths)
	os.WriteFile(filepath.Join(e.share, "x.flac"), flac48k(4096), 0o644)
	dialRekordbox = func(context.Context, net.IP, byte) (rbxLink, error) { return analysedRbxLink{fakeRbxLink{paths}}, nil }
	old := rbxAnalysisDir
	rbxAnalysisDir = t.TempDir()
	t.Cleanup(func() { rbxAnalysisDir = old })
	t.Cleanup(rbxAnalysisJobs.Wait) // before the fakes go
	ids, _ := e.trackIDs(t, "Tracks")
	tidalGet(t, e.base, "/v1/tracks/"+strconv.FormatInt(ids["First"], 10)+"/playbackinfopostpaywall", 200, nil)
	name := filepath.Join(rbxAnalysisDir, "names", "4096", "x.flac")
	if b, err := os.ReadFile(name); err != nil || string(b) != strconv.FormatInt(ids["First"], 10)+"\n" {
		t.Fatalf("Link load did not register its name and full size: %q (%v)", b, err)
	}
	var b []byte
	for i := 0; i < 200 && len(b) == 0; i++ {
		b, _ = os.ReadFile(filepath.Join(rbxAnalysisDir, strconv.FormatInt(ids["First"], 10)+".bin"))
		time.Sleep(10 * time.Millisecond)
	}
	if !bytes.HasPrefix(b, []byte(rbxAnalysisMagic)) {
		t.Fatalf("no analysis left for the hook: %q", b)
	}
	if st, err := os.Stat(filepath.Join(rbxAnalysisDir, ".loading")); err != nil || time.Since(st.ModTime()) > time.Minute {
		t.Fatalf("the load did not open the hook: %v", err)
	}
	var blobs [][]byte
	for r := b[len(rbxAnalysisMagic):]; len(r) >= 4; {
		n := binary.LittleEndian.Uint32(r)
		blobs = append(blobs, r[4:4+n])
		r = r[4+n:]
	}
	// The morning test: rekordbox's real answers gave no grid and no cue, so Engine analysed.
	if a := rbxAnalysis(analysedRbxLink{}, 1); len(a.Beats) != 2 || a.Beats[1].TimeMS != 1000 || a.Beats[0].BPM != 120 || len(a.Cues) != 1 || a.Cues[0].TimeMS != 2000 {
		t.Fatalf("rekordbox's analysis as it answers it: %+v", a)
	}
	want, _ := enginedb.EncodePerformance(rekordbox.Track{ID: 1, Path: paths[1], SampleRate: 48000, SampleCount: 3 * 48000, Duration: 3, BPM: 120, Title: "First", Artist: "Artist One"},
		rbxAnalysis(analysedRbxLink{}, 1))
	if len(blobs) != 5 || len(want.Beats) == 0 || len(want.QuickCues) == 0 ||
		!bytes.Equal(blobs[0], want.TrackData) || !bytes.Equal(blobs[1], want.Overview) || !bytes.Equal(blobs[2], want.Beats) ||
		!bytes.Equal(blobs[3], want.QuickCues) || !bytes.Equal(blobs[4], want.Loops) {
		t.Fatalf("blobs differ from rb-bridge's at 48 kHz: %d blobs", len(blobs))
	}
}

// A CDJ-3000's USB on the link (maintainer: the feature is for CDJ links): its export shows
// in the rekordbox source, read from the player over NFS as a CDJ would, and a track loads
// with the analysis of the export's own ANLZ files, left for the hook like the computer's.
func TestCDJMediaListsAndLoadsWithItsAnalysis(t *testing.T) {
	e := newRbxEnv(t, nil)
	usb := t.TempDir()
	testfixture.Export(t, usb)
	nfs := &prolink.NFSServer{Root: usb}
	if err := nfs.Listen("127.63.0.7:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nfs.Close)
	oldNFS, oldRoom, oldDir := rbxPlayerNFS, rbxRoom, rbxAnalysisDir
	t.Cleanup(func() { rbxPlayerNFS, rbxRoom, rbxAnalysisDir = oldNFS, oldRoom, oldDir })
	rbxPlayerNFS = func(ip net.IP) *prolink.NFSClient {
		return &prolink.NFSClient{IP: ip, Portmap: nfs.PortmapPort(), Timeout: time.Second}
	}
	rbxRoom = func(*syscall.Statfs_t, int64) int64 { return 1 << 30 }
	rbxAnalysisDir = t.TempDir()
	t.Cleanup(rbxAnalysisJobs.Wait) // before the fakes go
	// The link has the CDJ and no computer.
	snap, _ := json.Marshal(prolink.Snapshot{Devices: []prolink.Device{{Name: "CDJ-3000", Number: 2, Address: "127.63.0.7", PacketType: 0x06, LastSeen: time.Now()}}})
	os.WriteFile(filepath.Join(e.s.cfg.StateDir, "prolink.json"), snap, 0o644)
	var ui struct {
		Sources       []string
		RekordboxName string
	}
	for i := 0; i < 200 && ui.RekordboxName == ""; i++ { // the player's media are checked in the background
		tidalGet(t, e.base, "/v1/enginex/ui", 200, &ui)
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(strings.Join(ui.Sources, ","), rekordboxScope) || ui.RekordboxName != "CDJ-3000" {
		t.Fatalf("source for the CDJ's media: %+v", ui)
	}
	var lists tidalList
	tidalGet(t, e.base, "/v1/users/1/playlists", 200, &lists)
	var got []string
	for _, raw := range lists.Items {
		var p tidalPlaylist
		json.Unmarshal(raw, &p)
		got = append(got, p.Title)
	}
	if !strings.Contains(strings.Join(got, "|"), "CDJ-3000 #2 USB / Tracks|CDJ-3000 #2 USB / Folder / Nested / 同名") {
		t.Fatalf("lists: %v", got)
	}
	// The fixture's CDJ analysed only its WAV; its FLAC gets the same analysis files.
	key := rbxMediaKey("127.63.0.7", 3)
	ids, names := e.trackIDs(t, "CDJ-3000 #2 USB / Tracks")
	if len(names) != 5 || ids["two.flac"] == 0 {
		t.Fatalf("tracks: %v", names)
	}
	rbxMediaLibs.Lock()
	rbxMediaLibs.m[key].tracks[2].AnalysisPath = rbxMediaLibs.m[key].tracks[1].AnalysisPath
	rbxMediaLibs.Unlock()
	id := ids["two.flac"]
	tidalGet(t, e.base, "/v1/tracks/"+strconv.FormatInt(id, 10)+"/playbackinfopostpaywall", 200, nil)
	resp, err := http.Get(e.base + "/v1/stream/" + strconv.FormatInt(id, 10) + ".flac")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(body, testfixture.FLAC()) {
		t.Fatalf("stream from the CDJ: %d, %d bytes", resp.StatusCode, len(body))
	}
	var b []byte
	for i := 0; i < 300 && len(b) == 0; i++ {
		b, _ = os.ReadFile(filepath.Join(rbxAnalysisDir, strconv.FormatInt(id, 10)+".bin"))
		time.Sleep(10 * time.Millisecond)
	}
	blobs := rbxBlobs(b)
	a := &rekordbox.Analysis{}
	for _, f := range []func() []byte{testfixture.DAT, testfixture.EXT, testfixture.EX2} {
		s, err := rekordbox.ParseAnalysis(f())
		if err != nil {
			t.Fatal(err)
		}
		a.Merge(s)
	}
	link, _ := openMedia(context.Background(), net.ParseIP("127.63.0.7"), 3)
	meta, _ := link.Metadata(2)
	want := rbxTrackFacts(meta, 2, "/Music/two.flac")
	src := filepath.Join(t.TempDir(), "two.flac")
	os.WriteFile(src, testfixture.FLAC(), 0o644)
	rbxHeader(src, &want)
	p, _ := enginedb.EncodePerformance(want, a)
	if len(blobs) != 5 || len(p.Beats) == 0 || len(p.QuickCues) == 0 || !bytes.Equal(blobs[2], p.Beats) || !bytes.Equal(blobs[3], p.QuickCues) || !bytes.Equal(blobs[1], p.Overview) {
		t.Fatalf("the CDJ's analysis is not what its ANLZ files give: %d blobs", len(blobs))
	}
}

// rbxBlobs reads the file the hook reads.
func rbxBlobs(b []byte) [][]byte {
	if !bytes.HasPrefix(b, []byte(rbxAnalysisMagic)) {
		return nil
	}
	var blobs [][]byte
	for r := b[len(rbxAnalysisMagic):]; len(r) >= 4; {
		n := binary.LittleEndian.Uint32(r)
		blobs = append(blobs, r[4:4+n])
		r = r[4+n:]
	}
	return blobs
}

// flacWithArt is a 48 kHz FLAC of 96000 samples laid out as the left deck's were: STREAMINFO,
// a cover art block, then a final PADDING block.
func flacWithArt(art int) []byte {
	b := append([]byte("fLaC"), 0, 0, 0, 34)
	si := make([]byte, 34)
	binary.BigEndian.PutUint64(si[10:], uint64(48000)<<44|uint64(1)<<41|uint64(23)<<36|96000)
	b = append(b, si...)
	b = append(b, 6, byte(art>>16), byte(art>>8), byte(art))
	b = append(b, make([]byte, art)...)
	b = append(b, 0x81, 0, 0, 16)
	return append(b, make([]byte, 16+64)...)
}

// rb7Link is rekordbox 7 on a computer answering for the export fixture's track 1 as the
// live probe on the left deck saw it (2026-11-18): no PQTZ, PCOB or PWV3 over 0x2c04
// (status 50), its two PCO2 lists empty, each section behind its little-endian length
// (padded), the grid as 0x2204 and the cues only as the 0x2b04 cue list.
type rb7Link struct {
	fakeRbxLink
	t     rekordbox.Track
	a     *rekordbox.Analysis
	files map[string][]byte
}

func (l rb7Link) Metadata(uint32) ([]prolink.RekordboxItem, error) {
	return []prolink.RekordboxItem{{Kind: 4, Label: l.t.Title}, {Kind: 7, Label: l.t.Artist}, {Kind: 0xb, ID: uint32(l.t.Duration + 0.5)},
		{Kind: 0xd, ID: uint32(l.t.BPM*100 + 0.5)}, {Kind: 0xf, Label: l.t.Key}}, nil
}

func (l rb7Link) Analysis(id uint32, tag, file string) ([]byte, error) {
	switch tag {
	case "PQTZ", "PCOB", "PWV3":
		return nil, nil
	case "PCO2":
		var out []byte
		for _, list := range []uint32{1, 0} {
			s := make([]byte, 20)
			copy(s, "PCO2")
			binary.BigEndian.PutUint32(s[4:], 20)
			binary.BigEndian.PutUint32(s[8:], 20)
			binary.BigEndian.PutUint32(s[12:], list)
			out = append(binary.LittleEndian.AppendUint32(out, 20), s...)
		}
		return out, nil
	}
	var out []byte
	b := l.files[file]
	for off := int(binary.BigEndian.Uint32(b[4:])); off+12 <= len(b); {
		n := int(binary.BigEndian.Uint32(b[off+8:]))
		if string(b[off:off+4]) == tag {
			out = append(binary.LittleEndian.AppendUint32(out, uint32(n+2)), b[off:off+n]...)
			out = append(out, 0, 0)
		}
		off += n
	}
	return out, nil
}

func (l rb7Link) BeatGrid(uint32) ([]byte, error) {
	b := make([]byte, 20, 20+16*len(l.a.Beats))
	for _, beat := range l.a.Beats {
		e := make([]byte, 16)
		binary.LittleEndian.PutUint16(e, beat.Number)
		binary.LittleEndian.PutUint16(e[2:], uint16(beat.BPM*100+0.5))
		binary.LittleEndian.PutUint32(e[4:], beat.TimeMS)
		b = append(b, e...)
	}
	return b, nil
}

func (l rb7Link) CuesExtended(uint32) ([]byte, error) {
	var b []byte
	for _, c := range l.a.Cues {
		name := utf16.Encode([]rune(c.Name + "\x00"))
		e := make([]byte, 0x4e+2*len(name)+4+0x20)
		binary.LittleEndian.PutUint32(e, uint32(len(e)))
		e[4], e[6] = byte(c.HotCue), 1
		if c.Loop {
			e[6] = 2
		}
		binary.LittleEndian.PutUint32(e[12:], c.TimeMS)
		binary.LittleEndian.PutUint32(e[16:], c.EndMS)
		e[0x22] = c.ColorID
		binary.LittleEndian.PutUint16(e[0x48:], uint16(2*len(name)))
		for i, v := range name {
			binary.LittleEndian.PutUint16(e[0x4a+2*i:], v)
		}
		q := 0x4e + 2*len(name)
		e[q] = c.ColorCode
		copy(e[q+1:], c.Color[1:])
		b = append(b, e...)
	}
	return b, nil
}

// The maintainer's test 2026-11-18: a Link load had a misplaced grid, no hot cues, and Engine
// analysed it again, where the same track from a rekordbox USB stick (rb-bridge) loads with
// its grid and cues at once. A Link load of a track leaves for Engine the bytes rb-bridge
// writes for that track from its export, and the same Track flag.
func TestRekordboxLinkAnalysisIsRbBridgesForTheSameTrack(t *testing.T) {
	for _, format := range []string{"mp3", "aac"} {
		t.Run(format, func(t *testing.T) { assertLinkMatchesBridge(t, format) })
	}
}

func TestRbxApplyHeaderUsesInfoLinkTimeline(t *testing.T) {
	var tr rekordbox.Track
	rbxApplyHeader(&tr, compat.AudioInfo{
		Format:                 "mp3",
		SampleRate:             44100,
		StartOffsetSamples:     2257,
		LinkStartOffsetSamples: 1105,
	})
	if tr.StartOffsetSamples != 1105 {
		t.Fatalf("Link MP3 offset = %d, want 1105", tr.StartOffsetSamples)
	}
}

func assertLinkMatchesBridge(t *testing.T, format string) {
	t.Helper()
	root := t.TempDir()
	var file []byte
	rel := testfixture.LargeTrackPath(1)
	if format == "aac" {
		rel, file = testfixture.AACExport(t, root), testfixture.AACWithPriming()
	} else {
		testfixture.LargeExport(t, root, 1)
		// Make the file larger than the NFS prefix; keep Xing's counts valid.
		file = testfixture.MP3WithLAMETag()
		file = append(file, bytes.Repeat(file[417:834], 100)...)
		binary.BigEndian.PutUint32(file[44:], 102)
		binary.BigEndian.PutUint32(file[48:], uint32(len(file)))
		testfixture.Write(t, root, rel, file)
	}
	if _, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	want := make([][]byte, 5)
	var analysed int
	var locked sql.NullBool
	err = db.QueryRow(`SELECT p.trackData, p.overviewWaveFormData, p.beatData, p.quickCues, p.loops, t.isAnalyzed, t.isBeatGridLocked FROM Track t JOIN PerformanceData p ON p.trackId = t.id WHERE t.pdbImportKey = 1`).
		Scan(&want[0], &want[1], &want[2], &want[3], &want[4], &analysed, &locked)
	db.Close()
	if err != nil || analysed != 1 || locked.Bool || len(want[2]) == 0 || len(want[3]) == 0 {
		t.Fatalf("rb-bridge's row: %v analysed=%d locked=%v", err, analysed, locked)
	}
	lib, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	a := &rekordbox.Analysis{}
	for _, f := range [][]byte{testfixture.DAT(), testfixture.EXT(), testfixture.EX2()} {
		s, err := rekordbox.ParseAnalysis(f)
		if err != nil {
			t.Fatal(err)
		}
		a.Merge(s)
	}
	var one rekordbox.Track
	for _, tr := range lib.Tracks {
		if tr.ID == 1 {
			one = tr
		}
	}
	paths := map[uint32]string{1: "/" + rel}
	e := newRbxEnv(t, paths)
	nfs := &prolink.NFSServer{Root: root}
	if err := nfs.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nfs.Close)
	oldExport := rbxExport
	t.Cleanup(func() { rbxExport = oldExport })
	rbxExport = "/C/" // the simulated NFS server exports its root as a Pioneer slot
	rbxNFS = func(ip net.IP) *prolink.NFSClient {
		return &prolink.NFSClient{IP: ip, Portmap: nfs.PortmapPort(), Timeout: time.Second}
	}
	prefix, audio, err := rbxNFS(net.IPv4(127, 0, 0, 1)).FetchPrefix(t.Context(), rbxExport, paths[1], 32<<10)
	if err != nil || audio.Size != uint32(len(file)) || !bytes.Equal(prefix, file[:32<<10]) {
		t.Fatalf("NFS prefix: %d of %d bytes: %v", len(prefix), audio.Size, err)
	}
	link := rb7Link{fakeRbxLink{paths}, one, a, map[string][]byte{"DAT": testfixture.DAT(), "EXT": testfixture.EXT(), "2EX": testfixture.EX2()}}
	dialRekordbox = func(context.Context, net.IP, byte) (rbxLink, error) { return link, nil }
	old := rbxAnalysisDir
	rbxAnalysisDir = t.TempDir()
	t.Cleanup(func() { rbxAnalysisDir = old })
	t.Cleanup(rbxAnalysisJobs.Wait)
	ids, _ := e.trackIDs(t, "Tracks")
	b, err := os.ReadFile(rbxAnalysisPath(ids["First"]))
	if err != nil {
		t.Fatalf("analysis not ready when Engine receives the list: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rbxAnalysisDir, ".loading")); err != nil {
		t.Fatalf("hook was not enabled before list response: %v", err)
	}
	if b[len(b)-1] != byte(analysed) {
		t.Errorf("Link analysed flag %d differs from rb-bridge's %d", b[len(b)-1], analysed)
	}
	got := rbxBlobs(b)
	if len(got) != 5 {
		t.Fatalf("no analysis left for the hook: %d blobs", len(got))
	}
	for i, name := range []string{"trackData", "overviewWaveFormData", "beatData", "quickCues", "loops"} {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("%s differs from rb-bridge's: %d bytes, rb-bridge %d", name, len(got[i]), len(want[i]))
		}
	}
	replay, err := os.ReadFile(rbxAnalysisPath(ids["First"]) + ".perf")
	if err != nil {
		t.Fatal(err)
	}
	expectedReplay, err := perfFromBlobs(one.BPM, want[0], want[2], want[1])
	if err != nil || !bytes.Equal(replay, expectedReplay) {
		t.Fatalf("download analysis replay differs from USB performance: %v", err)
	}
}

// The maintainer's test 2026-11-18: three FLACs with 1.3-2 MB of cover art before the audio
// got no sample rate from a copy's first 512 KiB, so the grid was laid at 44.1 kHz on
// 48 kHz tracks. The header is read from as much of the running copy as it takes.
func TestRekordboxHeaderPastCoverArt(t *testing.T) {
	flac := flacWithArt(2 << 20)
	dir := t.TempDir()
	file, tmp := filepath.Join(dir, "two.flac"), filepath.Join(dir, ".fetch")
	if err := os.WriteFile(tmp, flac[:rbxHeadBytes], 0o644); err != nil {
		t.Fatal(err)
	}
	cp := &rbxCopy{file: file, tmp: tmp, size: int64(len(flac)), got: rbxHeadBytes}
	cp.more = sync.NewCond(&cp.mu)
	rbxCopies.Lock()
	rbxCopies.m[file] = cp
	rbxCopies.Unlock()
	t.Cleanup(func() {
		rbxCopies.Lock()
		delete(rbxCopies.m, file)
		rbxCopies.Unlock()
	})
	go func() { // the rest arrives
		time.Sleep(100 * time.Millisecond)
		os.WriteFile(tmp, flac, 0o644)
		cp.mu.Lock()
		cp.got, cp.done = cp.size, true
		cp.mu.Unlock()
	}()
	var tr rekordbox.Track
	rbxHeader(file, &tr)
	if tr.SampleRate != 48000 || tr.SampleCount != 96000 {
		t.Fatalf("header past the cover art: %d Hz, %d samples", tr.SampleRate, tr.SampleCount)
	}
}
