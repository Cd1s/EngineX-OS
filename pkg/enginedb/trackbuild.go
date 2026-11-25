package enginedb

// One track of an export, built from the volume into what the background job stages (design
// §1.5): the audio header's facts, Engine's performance blobs, the .rgb body and the cover. Rules:
//   - offsets: rule 4, one rekordbox offset for USB and Pro DJ Link (AudioInfo.RekordboxOffset);
//   - AAC: an implicit SBR stream that advertises only its core rate keeps the export's timing;
//   - CF049: 499.5 BPM or more is rekordbox's clamp of a misparsed file, not a tempo;
//   - the file header's sample rate wins over the export's;
//   - a .2EX with PWV7 (and PWV6) spares the waveform bodies of .EXT and .DAT (#253);
//   - #53: an unusable header, analysis or cover gives a record without performance data.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// ReadOptions paces what BuildTrack reads while a deck may play from the same stick: reads of at
// most Chunk bytes, each followed by Pause, whose error stops the build. Zero: full speed.
type ReadOptions struct {
	Chunk int
	Pause func(n int, took time.Duration) error
}

// Seams for slow or failing media in tests.
var (
	trackStat        = os.Stat
	trackAudioHeader = compat.AudioHeader
)

const maxCoverFile = 32 << 20

// TrackSources is a track's files as a stat sees them.
type TrackSources struct {
	Rel     string           // the audio file's spelling on the volume; the export's, cleaned, when Missing
	Missing bool             // the volume has no file for the track
	Sig     rbstate.TrackSig // Row is the caller's to set
	Broken  error            // why the analysis or the cover cannot be used (#53)
}

// sigOf stats a resolved path: a regular file's signature.
func sigOf(res *compat.Resolver, rel string) (rbstate.FileSig, error) {
	st, err := trackStat(res.FullPath(rel))
	if err == nil && !st.Mode().IsRegular() {
		err = fmt.Errorf("not a regular file: %s", rel)
	}
	if err != nil {
		return rbstate.FileSig{}, err
	}
	return rbstate.FileSig{Rel: rel, Size: st.Size(), MTimeNS: st.ModTime().UnixNano()}, nil
}

// StatTrack stats the audio file, the .DAT, .EXT and .2EX and the cover, and reads nothing else:
// unchanged files give the same Sig. An error (a path the volume refuses, a file that cannot be
// stat'ed now) leaves the track unstaged until the next job.
func StatTrack(ctx context.Context, res *compat.Resolver, t rekordbox.Track) (TrackSources, error) {
	src := TrackSources{Sig: rbstate.TrackSig{Rules: rbstate.Rules}}
	guess, err := compat.CleanRelative(t.Path)
	if err != nil {
		return src, fmt.Errorf("track %d: %w", t.ID, err)
	}
	rel, err := res.ResolveContext(ctx, t.Path)
	if errors.Is(err, os.ErrNotExist) && !stopped(err) {
		src.Rel, src.Missing = guess, true
		return src, nil
	}
	if err != nil {
		return src, fmt.Errorf("track %d: %w", t.ID, err)
	}
	src.Rel = rel
	if src.Sig.Audio, err = sigOf(res, rel); errors.Is(err, os.ErrNotExist) && !stopped(err) {
		src.Rel, src.Missing = guess, true
		return src, nil
	}
	if err != nil {
		return src, fmt.Errorf("track %d: audio: %w", t.ID, err)
	}
	src.Sig.ANLZ, src.Broken = statAnalysis(ctx, res, t.AnalysisPath)
	if stopped(src.Broken) {
		return src, src.Broken
	}
	if t.ArtworkPath != "" {
		cover, err := statCover(ctx, res, t.ArtworkPath)
		if stopped(err) {
			return src, err
		}
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("rb-bridge: artwork for track %d (%s) unavailable: %v", t.ID, t.ArtworkPath, err)
			cover = rbstate.FileSig{}
		} else if err != nil && src.Broken == nil {
			src.Broken = fmt.Errorf("artwork: %w", err)
		}
		src.Sig.Artwork = cover
	}
	return src, nil
}

// statAnalysis stats the analysis files beside the export's .DAT path that exist.
func statAnalysis(ctx context.Context, res *compat.Resolver, analysisPath string) ([]rbstate.FileSig, error) {
	if analysisPath == "" {
		return nil, nil
	}
	base := strings.TrimSuffix(analysisPath, path.Ext(analysisPath))
	var files []rbstate.FileSig
	for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
		rel, err := res.ResolveContext(ctx, base+ext)
		if errors.Is(err, os.ErrNotExist) && !stopped(err) {
			continue
		}
		if err != nil {
			return files, err
		}
		sig, err := sigOf(res, rel)
		if err != nil {
			return files, fmt.Errorf("analysis: %w", err)
		}
		files = append(files, sig)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no analysis files for %s", analysisPath)
	}
	return files, nil
}

// statCover stats the export's cover. Anything but a regular file within maxCoverFile has a zero
// signature, so the track is built again when a usable file appears.
func statCover(ctx context.Context, res *compat.Resolver, artworkPath string) (rbstate.FileSig, error) {
	rel, err := res.ResolveContext(ctx, artworkPath)
	if err != nil {
		return rbstate.FileSig{}, err
	}
	sig, err := sigOf(res, rel)
	if err == nil && sig.Size > maxCoverFile {
		sig, err = rbstate.FileSig{}, fmt.Errorf("larger than 32 MiB: %s", rel)
	}
	return sig, err
}

// stopped reports a cancellation or a Pause's stop, also when a file error wraps it.
func stopped(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, new(buildStopped))
}

type buildStopped struct{ err error }

func (e buildStopped) Error() string { return "enginedb: track build stopped: " + e.err.Error() }
func (e buildStopped) Unwrap() error { return e.err }

// BuildTrack builds a track from its StatTrack. Sig.Row is the caller's to set. A missing audio
// file gives Missing; an unusable header, analysis or cover gives Degraded and no performance data
// (#53). An error leaves the track unstaged: a file that cannot be read now, or a stop from
// read.Pause (wrapped: errors.Is finds the Pause's error).
func BuildTrack(res *compat.Resolver, t rekordbox.Track, src TrackSources, read ReadOptions) (*rbstate.Staged, error) {
	var halted error
	if read.Pause != nil {
		pause := read.Pause
		read.Pause = func(n int, took time.Duration) error {
			if err := pause(n, took); err != nil {
				halted = buildStopped{err}
				return halted
			}
			return nil
		}
	}
	key := rbstate.FoldPath(src.Rel)
	if clean, err := compat.CleanRelative(t.Path); err == nil {
		key = rbstate.FoldPath(clean)
	}
	out := &rbstate.Staged{Key: key, Sig: src.Sig, Rel: src.Rel, Missing: src.Missing}
	if src.Missing {
		return out, nil
	}
	out.Facts.FileBytes = src.Sig.Audio.Size
	// CF049: rekordbox's analyser can clamp a misparsed file to 500 BPM.
	if t.BPM >= 499.5 {
		t.BPM = 0
	}
	degrade := func(err error) (*rbstate.Staged, error) {
		log.Printf("rb-bridge: track %d (%s) staged without cues: %v", t.ID, src.Rel, err)
		out.Degraded = err.Error()
		return out, nil
	}
	if src.Broken != nil {
		return degrade(src.Broken)
	}
	full := res.FullPath(src.Rel)
	if !src.Sig.Artwork.IsZero() {
		// Read here, at the job's pace, so that no window reads the stick for it.
		art, err := readCover(res.FullPath(src.Sig.Artwork.Rel), read)
		if halted != nil {
			return nil, halted
		}
		if err == nil {
			out.Art = art
		}
	}
	a, err := readTrackAnalysis(res, src.Sig.ANLZ, read)
	if halted != nil {
		return nil, halted
	}
	if stopped(err) {
		return nil, err
	}
	if err != nil {
		return degrade(err)
	}
	start := time.Now()
	audio, err := trackAudioHeader(full)
	if stopped(err) {
		return nil, err
	}
	var file *os.PathError
	if errors.As(err, &file) {
		// The file is there but cannot be read now: the next job tries again (#252).
		return nil, fmt.Errorf("track %d: audio header: %w", t.ID, err)
	}
	if err != nil {
		return degrade(fmt.Errorf("audio header: %w", err))
	}
	if read.Pause != nil {
		if err = read.Pause(1, time.Since(start)); err != nil {
			return nil, err
		}
	}
	if len(t.Cues) > 0 {
		a.Merge(&rekordbox.Analysis{Cues: t.Cues})
	}
	if audio.Format == "aac" && t.SampleRate > 0 && t.SampleRate != audio.SampleRate {
		// An implicit SBR stream can advertise only its core sample rate. Keep the export's
		// timing; the priming tag is in decoded samples.
		t.StartOffsetSamples = audio.RekordboxOffset()
	} else if audio.SampleRate > 0 {
		if t.SampleRate > 0 && t.SampleRate != audio.SampleRate {
			log.Printf("rb-bridge: track %d (%s): export sample rate %d Hz differs from file header %d Hz; using file header", t.ID, src.Rel, t.SampleRate, audio.SampleRate)
		}
		t.SampleRate = audio.SampleRate
		if audio.Samples > 0 {
			t.SampleCount = audio.Samples
		}
		if audio.Duration > 0 {
			t.Duration = audio.Duration
		}
		if audio.BitDepth > 0 {
			t.BitDepth = audio.BitDepth
		}
		if audio.Bitrate > 0 && (audio.Format != "mp3" || t.Bitrate == 0) {
			t.Bitrate = audio.Bitrate
		}
		t.StartOffsetSamples = audio.RekordboxOffset()
	}
	if len(a.Beats) > 0 && a.Beats[0].BPM >= 499.5 {
		a.Beats = nil
	}
	gridBPM := 0.0
	if t.BPM == 0 && len(a.Beats) > 0 {
		t.BPM = a.Beats[0].BPM
		gridBPM = t.BPM
	}
	out.Facts = rbstate.Facts{
		Duration:           t.Duration,
		SampleRate:         t.SampleRate,
		SampleCount:        t.SampleCount,
		BitDepth:           t.BitDepth,
		Bitrate:            t.Bitrate,
		StartOffsetSamples: t.StartOffsetSamples,
		BPM:                gridBPM,
		FileBytes:          src.Sig.Audio.Size,
	}
	perf, err := EncodePerformance(t, a)
	if err != nil {
		return degrade(fmt.Errorf("performance: %w", err))
	}
	out.Perf = rbstate.Perf{TrackData: perf.TrackData, Overview: perf.Overview, Beats: perf.Beats,
		QuickCues: perf.QuickCues, Loops: perf.Loops, ActiveLoops: perf.ActiveLoops, Complete: perf.Complete}
	out.Digest = WrittenDigest(perf.Beats, perf.QuickCues, perf.Loops)
	if perf.Overview != nil {
		if rgb, err := EncodeRGBOverview(t, a, perf.Overview); err == nil {
			out.RGB = rgb
		}
	}
	if t.ArtworkPath == "" {
		// The export names no cover: the audio file's own one, as the import took it.
		art, err := embeddedCover(full, read)
		if halted != nil {
			return nil, halted
		}
		if err == nil {
			out.Art = art
		}
	}
	return out, nil
}

// readTrackAnalysis reads a track's analysis files as the import did (#253): every .2EX whole;
// when one has a PWV7 and an RGB overview the waveform bodies of the others are skipped, when it
// has a PWV7 only their PWV4 is read; their beats, cues and song structure are always read.
func readTrackAnalysis(res *compat.Resolver, files []rbstate.FileSig, read ReadOptions) (*rekordbox.Analysis, error) {
	parsed := make([]*rekordbox.Analysis, len(files))
	hasPWV7, hasRGB := false, false
	for i, f := range files {
		if !strings.EqualFold(path.Ext(f.Rel), ".2EX") {
			continue
		}
		a, err := readTrackANLZ(res.FullPath(f.Rel), nil, read)
		if err != nil {
			return nil, err
		}
		if w := displayAnalysisWaveform(a); w != nil && w.Tag == "PWV7" {
			hasPWV7 = true
		}
		hasRGB = hasRGB || hasColorOverviewWaveform(a)
		parsed[i] = a
	}
	skip := func(string) bool { return false }
	if hasPWV7 && hasRGB {
		skip = func(string) bool { return true }
	} else if hasPWV7 {
		skip = func(tag string) bool { return tag != "PWV4" }
	}
	out := &rekordbox.Analysis{}
	for i, f := range files {
		a := parsed[i]
		if a == nil {
			var err error
			if a, err = readTrackANLZ(res.FullPath(f.Rel), skip, read); err != nil {
				return nil, err
			}
		}
		out.Merge(a)
	}
	return out, nil
}

// readTrackANLZ reads one analysis file; a PWV7 is always read.
func readTrackANLZ(full string, skip func(tag string) bool, read ReadOptions) (*rekordbox.Analysis, error) {
	f, err := openTrackFile(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if read.Pause != nil {
		noReadahead(f)
	}
	var keep func(string) bool
	if skip != nil {
		keep = func(tag string) bool { return tag != "PWV7" && skip(tag) }
	}
	a, _, err := rekordbox.ReadAnalysisFilePaced(f, keep, read.Chunk, read.Pause)
	return a, err
}

// openTrackFile opens a file of the volume for reading, without following a symlink and,
// where allowed, without access-time updates: on FAT a read otherwise writes the file's
// directory entry back.
func openTrackFile(full string) (*os.File, error) {
	f, err := os.OpenFile(full, os.O_RDONLY|syscall.O_NOFOLLOW|openNoAtime, 0)
	if errors.Is(err, os.ErrPermission) {
		f, err = os.OpenFile(full, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	}
	return f, err
}

// DetailOf is the scrolling detail waveform of a track for the shadow display: the first of files
// (volume-relative, DetailFiles' order) that resolves under root to a regular file within 16 MiB
// and holds a waveform the display draws. Nil when none does.
func DetailOf(root string, files []string) *rekordbox.Analysis {
	if len(files) == 0 {
		return nil
	}
	r, err := compat.NewResolver(root)
	if err != nil {
		return nil
	}
	a := &rekordbox.Analysis{}
	for _, rel := range files {
		if data, err := readDetailFile(r, rel); err == nil {
			if one, err := rekordbox.ParseAnalysis(data); err == nil {
				a.Waveforms = append(a.Waveforms, one.Waveforms...)
			}
		}
		if w := displayAnalysisWaveform(a); w != nil { // a PWV7 found first leaves the .EXT unread
			return &rekordbox.Analysis{Waveforms: []rekordbox.Waveform{*w}}
		}
	}
	return nil
}

func readDetailFile(r *compat.Resolver, rel string) ([]byte, error) {
	const maxDetailFile = 16 << 20 // a PWV7 of 2<<20 entries and the rest
	rel, err := r.Resolve(rel)
	if err != nil {
		return nil, err
	}
	f, err := openTrackFile(r.FullPath(rel))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || st.Size() > maxDetailFile {
		return nil, fmt.Errorf("not a regular file within %d bytes: %s", maxDetailFile, rel)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxDetailFile+1))
	if err == nil && len(b) > maxDetailFile {
		err = fmt.Errorf("grew past %d bytes: %s", maxDetailFile, rel)
	}
	return b, err
}

// DetailFiles are the ANLZ files of a built track (Track.Built.ANLZ) that serve the detail
// waveform, in the order DetailOf reads them.
func DetailFiles(files []rbstate.FileSig) []string {
	var out []string
	for _, ext := range []string{".2EX", ".EXT"} {
		for _, f := range files {
			if strings.EqualFold(path.Ext(f.Rel), ext) {
				out = append(out, f.Rel)
			}
		}
	}
	return out
}

// readCover reads a cover file through read's pacing.
func readCover(full string, read ReadOptions) ([]byte, error) {
	f, err := openTrackFile(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := &pacedFile{f: f, chunk: read.Chunk, pause: read.Pause}
	image, err := io.ReadAll(io.LimitReader(r, maxCoverFile+1))
	if err != nil {
		return nil, err
	}
	if len(image) == 0 || len(image) > maxCoverFile {
		return nil, fmt.Errorf("cover of %d bytes", len(image))
	}
	return image, nil
}

// embeddedCover is ExtractEmbeddedArtwork reading through read's pacing: a tag's picture can be
// megabytes at the head of the file a deck is playing.
func embeddedCover(full string, read ReadOptions) ([]byte, error) {
	f, err := openTrackFile(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if read.Pause != nil {
		noReadahead(f)
	}
	r := &pacedFile{f: f, chunk: read.Chunk, pause: read.Pause}
	hdr := make([]byte, 12)
	n, err := io.ReadFull(r, hdr)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	if n < 4 {
		return nil, ErrNoArtwork
	}
	try := func(extract func(io.ReadSeeker) ([]byte, error)) ([]byte, bool) {
		if _, err := r.Seek(0, io.SeekStart); err != nil || r.err != nil {
			return nil, false
		}
		art, err := extract(r)
		return art, err == nil && r.err == nil
	}
	ext := strings.ToLower(filepath.Ext(full))
	if string(hdr[:3]) == "ID3" {
		if art, ok := try(extractMP3Artwork); ok {
			return art, nil
		}
	}
	if string(hdr[:4]) == "fLaC" {
		if art, ok := try(extractFLACArtwork); ok {
			return art, nil
		}
	}
	if n >= 8 && (string(hdr[4:8]) == "ftyp" || string(hdr[4:8]) == "moov") || ext == ".m4a" || ext == ".mp4" {
		if art, ok := try(extractMP4Artwork); ok {
			return art, nil
		}
	}
	for _, extract := range []func(io.ReadSeeker) ([]byte, error){extractMP3Artwork, extractFLACArtwork, extractMP4Artwork} {
		if art, ok := try(extract); ok {
			return art, nil
		}
		if r.err != nil {
			return nil, r.err
		}
	}
	return nil, ErrNoArtwork
}

// pacedFile is an io.ReadSeeker over f whose reads take at most chunk bytes and call pause after
// each. A pause error sticks: every later read returns it.
type pacedFile struct {
	f     *os.File
	chunk int
	pause func(n int, took time.Duration) error
	err   error
}

func (p *pacedFile) Read(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	if p.pause != nil && p.chunk > 0 && len(b) > p.chunk {
		b = b[:p.chunk]
	}
	start := time.Now()
	n, err := p.f.Read(b)
	if p.pause != nil && n > 0 {
		if p.err = p.pause(n, time.Since(start)); err == nil {
			err = p.err
		}
	}
	return n, err
}

func (p *pacedFile) Seek(off int64, whence int) (int64, error) { return p.f.Seek(off, whence) }
