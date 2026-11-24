package enginedb

// One track of a rekordbox export, built from the volume into what the background job stages
// (rbstate.Staged): the audio header's facts, Engine's performance blobs, the body of the RGB
// overview file and the cover (the export's, else the audio file's own). It opens no library and writes nothing:
// the job puts the result in the stage on /data and a mount window applies it (design §1.5).
//
// The rules are the ones the import in sync.go applied:
//   - offsets: rule 4, one rekordbox offset for USB and Pro DJ Link (compat.AudioInfo.RekordboxOffset);
//   - AAC: an implicit SBR stream that advertises only its core rate keeps the export's timing,
//     and the priming of the file's header moves the grid and the cues;
//   - CF049: a tempo of 499.5 BPM or more is rekordbox's clamp of a misparsed file, not a tempo;
//   - the file header's sample rate wins over the export's;
//   - PWV sections: a .2EX with PWV7 supplies the detail and 3-band overview, its PWV6 the RGB
//     overview; the waveform bodies of .EXT and .DAT are then not read (#253);
//   - #53: a track whose audio header, analysis or cover cannot be used is staged without
//     performance data, with the reason, and read again only when its files change.

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

// ReadOptions paces what BuildTrack reads from the volume.
type ReadOptions struct {
	// Chunk is the most bytes one read of an analysis file or a cover takes while Pause is set;
	// 0 reads whole sections.
	Chunk int
	// Pause is called after each read with its size and duration: a deck may be playing from the
	// same stick. An error stops the build and is returned. nil reads at full speed.
	Pause func(n int, took time.Duration) error
}

// Seams for slow or failing media in tests: the stat of a track's files and the audio header.
var (
	trackStat        = os.Stat
	trackAudioHeader = compat.AudioHeader
)

// maxCoverFile is the largest cover file an export may name (the import's limit).
const maxCoverFile = 32 << 20

// TrackSources is a track's files as a stat sees them: what its staged data is built from.
type TrackSources struct {
	// Rel is the audio file's spelling on the volume; the export's, cleaned, when Missing.
	Rel string
	// Missing: the volume has no file for the track.
	Missing bool
	// Sig is the signature of the files; Rules and Row are the caller's to set.
	Sig rbstate.TrackSig
	// Broken is why the track's analysis or cover cannot be used (#53): its performance data is
	// not built. Nil otherwise.
	Broken error
}

// StatTrack stats the audio file, the .DAT, .EXT and .2EX the export names and its cover, and
// reads nothing else. Two calls on unchanged files give the same Sig, which is what the job
// compares with the snapshot and the stage. An error is a path the volume refuses (traversal,
// a symlink, a name two files share, not a regular file) or a file that cannot be stat'ed now
// (permission, I/O); the track is left unstaged and looked at again by the next job.
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
	st, err := trackStat(res.FullPath(rel))
	if errors.Is(err, os.ErrNotExist) && !stopped(err) {
		src.Rel, src.Missing = guess, true
		return src, nil
	}
	if err != nil {
		return src, fmt.Errorf("track %d: %w", t.ID, err)
	}
	if !st.Mode().IsRegular() {
		return src, fmt.Errorf("track %d: audio not a regular file: %s", t.ID, rel)
	}
	src.Sig.Audio = rbstate.FileSig{Rel: rel, Size: st.Size(), MTimeNS: st.ModTime().UnixNano()}
	src.Sig.ANLZ, src.Broken = statAnalysis(ctx, res, t.AnalysisPath)
	if stopped(src.Broken) {
		return src, src.Broken
	}
	if t.ArtworkPath != "" {
		cover, err := statCover(ctx, res, t.ArtworkPath)
		if stopped(err) {
			return src, err
		}
		if err != nil && src.Broken == nil {
			src.Broken = fmt.Errorf("artwork: %w", err)
		}
		src.Sig.Artwork = cover
	}
	return src, nil
}

// statAnalysis stats the analysis files of base (the export's .DAT path) that exist.
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
		st, err := trackStat(res.FullPath(rel))
		if err != nil {
			return files, err
		}
		if !st.Mode().IsRegular() {
			return files, fmt.Errorf("analysis not a regular file: %s", rel)
		}
		files = append(files, rbstate.FileSig{Rel: rel, Size: st.Size(), MTimeNS: st.ModTime().UnixNano()})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no analysis files for %s", analysisPath)
	}
	return files, nil
}

// statCover stats the export's cover. Only a regular file within maxCoverFile counts: anything
// else has a zero signature, so the track is built again when a usable file appears.
func statCover(ctx context.Context, res *compat.Resolver, artworkPath string) (rbstate.FileSig, error) {
	rel, err := res.ResolveContext(ctx, artworkPath)
	if err != nil {
		return rbstate.FileSig{}, err
	}
	st, err := trackStat(res.FullPath(rel))
	if err != nil {
		return rbstate.FileSig{}, err
	}
	if !st.Mode().IsRegular() || st.Size() > maxCoverFile {
		return rbstate.FileSig{}, fmt.Errorf("not a regular file within 32 MiB: %s", rel)
	}
	return rbstate.FileSig{Rel: rel, Size: st.Size(), MTimeNS: st.ModTime().UnixNano()}, nil
}

// stopped reports a cancellation, also when a file error wraps it.
func stopped(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, new(buildStopped))
}

// buildStopped is the error a ReadOptions.Pause stopped the build with.
type buildStopped struct{ err error }

func (e buildStopped) Error() string { return "enginedb: track build stopped: " + e.err.Error() }
func (e buildStopped) Unwrap() error { return e.err }

// BuildTrack builds one track of the export from the volume. Key is the track's rbstate key of
// the export's path and Sig.Row is empty: the caller sets both from its view. A missing audio
// file gives a record with Missing; an unusable header, analysis or cover gives a record with
// Degraded and no performance data (#53). An error leaves the track unstaged: a path the volume
// refuses, a file that cannot be read now, or a stop from read.Pause (wrapped, so errors.Is
// finds the Pause's error).
func BuildTrack(res *compat.Resolver, t rekordbox.Track, read ReadOptions) (*rbstate.Staged, error) {
	if res == nil {
		return nil, errors.New("enginedb: nil resolver")
	}
	src, err := StatTrack(context.Background(), res, t)
	if err != nil {
		return nil, err
	}
	return BuildTrackFrom(res, t, src, read)
}

// BuildTrackFrom is BuildTrack from a StatTrack of the same track, which it does not repeat.
func BuildTrackFrom(res *compat.Resolver, t rekordbox.Track, src TrackSources, read ReadOptions) (*rbstate.Staged, error) {
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
		// The export's cover is read here, at the job's pace, so that no window reads the stick
		// for it. One that cannot be read is left out until its file changes.
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
	out.Digest = writtenDigest(perf.Beats, perf.QuickCues, perf.Loops)
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

// DetailFiles are the ANLZ files of a built track (the snapshot's Track.Built.ANLZ) that serve
// the scrolling detail waveform, the one DetailOf reads first in front.
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
	n, err := r.ReadAt(hdr, 0)
	if err != nil && err != io.EOF {
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

// pacedFile is an io.ReadSeeker and io.ReaderAt over f whose reads take at most chunk bytes
// and call pause after each. A pause error sticks: every later read returns it.
type pacedFile struct {
	f     *os.File
	chunk int
	pause func(n int, took time.Duration) error
	err   error
}

func (p *pacedFile) limit(b []byte) []byte {
	if p.pause != nil && p.chunk > 0 && len(b) > p.chunk {
		return b[:p.chunk]
	}
	return b
}

func (p *pacedFile) paced(n int, start time.Time) {
	if p.pause != nil && n > 0 && p.err == nil {
		p.err = p.pause(n, time.Since(start))
	}
}

func (p *pacedFile) Read(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	start := time.Now()
	n, err := p.f.Read(p.limit(b))
	p.paced(n, start)
	if err == nil && p.err != nil {
		err = p.err
	}
	return n, err
}

func (p *pacedFile) ReadAt(b []byte, off int64) (int, error) {
	done := 0
	for done < len(b) {
		if p.err != nil {
			return done, p.err
		}
		start := time.Now()
		n, err := p.f.ReadAt(p.limit(b[done:]), off+int64(done))
		done += n
		p.paced(n, start)
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

func (p *pacedFile) Seek(off int64, whence int) (int64, error) { return p.f.Seek(off, whence) }
