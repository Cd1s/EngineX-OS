package analyzer

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testaudio"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
)

func uncompress(t *testing.T, b []byte) []byte {
	t.Helper()
	if len(b) < 4 {
		t.Fatal("missing qCompress header")
	}
	r, err := zlib.NewReader(bytes.NewReader(b[4:]))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	v, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != int(binary.BigEndian.Uint32(b)) {
		t.Fatal("qCompress length mismatch")
	}
	return v
}
func TestEngine504AtomicImport(t *testing.T) {
	path, root, db := testlibrary.Create(t)
	library, err := OpenLibrary(path)
	if err != nil {
		t.Fatal(err)
	}
	defer library.Close()
	song := testaudio.Song(8)
	a, err := DecodeWAV(bytes.NewReader(testaudio.WAV(song)))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Analyze(a, Options{})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "first.wav")
	os.WriteFile(file, testaudio.WAV(song), 0600)
	input := Import{Path: file, Title: "First", Artist: "Artist", Comment: "[FLAC]", Playlist: "[Guest / 临时加歌]", Result: res}
	ctx := context.Background()
	id, err := library.Import(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var storedPath string
	if err = db.QueryRow("SELECT path FROM Track WHERE id=?", id).Scan(&storedPath); err != nil {
		t.Fatal(err)
	}
	if storedPath != "../first.wav" || filepath.Clean(filepath.Join(root, "Engine Library", filepath.FromSlash(storedPath))) != file {
		t.Fatal("track path is not relative to Engine Library", storedPath)
	}
	// #34: Engine stores kbit/s; the 16-bit mono test WAV is about Rate*16/1000.
	var bitrate int
	db.QueryRow("SELECT bitrate FROM Track WHERE id=?", id).Scan(&bitrate)
	if want := int(testaudio.Rate * 16 / 1000); bitrate < want*9/10 || bitrate > want*11/10 {
		t.Fatalf("bitrate %d, want about %d kbit/s", bitrate, want)
	}
	var track, overview, beats, cues []byte
	if err = db.QueryRow("SELECT trackData,overviewWaveFormData,beatData,quickCues FROM PerformanceData WHERE trackId=?", id).Scan(&track, &overview, &beats, &cues); err != nil {
		t.Fatal(err)
	}
	if len(cues) != 0 {
		t.Fatal("automatic Hot Cues")
	}
	tr := uncompress(t, track)
	ov := uncompress(t, overview)
	bt := uncompress(t, beats)
	if len(tr) != 44 || len(ov) != 3099 || len(bt) != 130 {
		t.Fatalf("blob lengths %d %d %d", len(tr), len(ov), len(bt))
	}
	if binary.BigEndian.Uint64(tr[8:16]) != uint64(len(song)) || math.Float64frombits(binary.BigEndian.Uint64(tr[:8])) != testaudio.Rate {
		t.Fatal("track units")
	}
	if binary.BigEndian.Uint64(ov[:8]) != 1024 || binary.BigEndian.Uint64(bt[17:25]) != 2 {
		t.Fatal("wrong byte order/count")
	}
	if math.Float64frombits(binary.LittleEndian.Uint64(bt[25:33])) != res.Beatgrid[0].SampleOffset {
		t.Fatal("beat marker order")
	}
	// Reimport must retain existing DJ cues and loops and avoid duplicate rows.
	if _, err = db.Exec("UPDATE PerformanceData SET quickCues=?,loops=? WHERE trackId=?", []byte("DJ cue"), []byte("DJ loop"), id); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE Track SET isBeatGridLocked=1 WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	res.Beatgrid[0].SampleOffset += 1000
	again, err := library.Import(ctx, input)
	if err != nil || again != id {
		t.Fatalf("reimport %d %v", again, err)
	}
	var count int
	db.QueryRow("SELECT count(*) FROM PlaylistEntity").Scan(&count)
	if count != 1 {
		t.Fatalf("memberships=%d", count)
	}
	db.QueryRow("SELECT quickCues FROM PerformanceData WHERE trackId=?", id).Scan(&cues)
	if string(cues) != "DJ cue" {
		t.Fatal("existing cue overwritten")
	}
	var kept []byte
	db.QueryRow("SELECT beatData FROM PerformanceData WHERE trackId=?", id).Scan(&kept)
	if !bytes.Equal(kept, beats) {
		t.Fatal("locked beatgrid overwritten")
	}
	// #34: some databases end a list with NULL instead of 0.
	if _, err = db.Exec("UPDATE PlaylistEntity SET nextEntityId=NULL"); err != nil {
		t.Fatal(err)
	}
	file2 := filepath.Join(root, "second.wav")
	os.WriteFile(file2, testaudio.WAV(song), 0600)
	input.Path = file2
	input.Title = "Second"
	_, err = library.Import(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var tail, head int
	db.QueryRow("SELECT count(*) FROM PlaylistEntity WHERE coalesce(nextEntityId,0)=0").Scan(&tail)
	db.QueryRow("SELECT count(*) FROM PlaylistEntity p WHERE NOT EXISTS(SELECT 1 FROM PlaylistEntity q WHERE q.nextEntityId=p.id)").Scan(&head)
	if tail != 1 || head != 1 {
		t.Fatalf("broken membership chain head=%d tail=%d", head, tail)
	}
	// A late failing playlist trigger must roll back Track and PerformanceData.
	if _, err = db.Exec("CREATE TRIGGER reject_import BEFORE INSERT ON PlaylistEntity BEGIN SELECT RAISE(ABORT,'fixture rejection'); END"); err != nil {
		t.Fatal(err)
	}
	input.Path = filepath.Join(root, "rollback.wav")
	os.WriteFile(input.Path, testaudio.WAV(song), 0600)
	if _, err = library.Import(ctx, input); err == nil {
		t.Fatal("expected transaction failure")
	}
	db.QueryRow("SELECT count(*) FROM Track").Scan(&count)
	if count != 2 {
		t.Fatalf("partial import persisted: %d tracks", count)
	}
	var integrity string
	if err = db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity=%s %v", integrity, err)
	}
	outside := filepath.Join(t.TempDir(), "outside.wav")
	os.WriteFile(outside, testaudio.WAV(song), 0600)
	input.Path = outside
	if _, err = library.Import(ctx, input); err == nil {
		t.Fatal("off-volume path accepted")
	}
}
func TestLibraryRejectsUnknownSchemaAndOffVolume(t *testing.T) {
	p, _, db := testlibrary.Create(t)
	db.Exec("UPDATE Information SET schemaVersionPatch=99")
	if l, err := OpenLibrary(p); err == nil {
		l.Close()
		t.Fatal("unknown schema accepted")
	}
}

func TestLibraryResolvesDatabaseSymlinkBeforeVolumeRoot(t *testing.T) {
	p, root, _ := testlibrary.Create(t)
	link := filepath.Join(t.TempDir(), "alias.db")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	l, err := OpenLibrary(link)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.root != root {
		t.Fatalf("root=%s want=%s", l.root, root)
	}
	// A real database in a non-Engine directory must not imply root=/.
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	wrong := filepath.Join(t.TempDir(), "m.db")
	os.WriteFile(wrong, b, 0600)
	if l, err := OpenLibrary(wrong); err == nil {
		l.Close()
		t.Fatal("non-Engine path accepted")
	}
}

func TestUnanalyzedLockedTrackGetsWaveformsAndMetadata(t *testing.T) {
	path, root, db := testlibrary.Create(t)
	file := filepath.Join(root, "old.wav")
	song := testaudio.Song(4)
	os.WriteFile(file, testaudio.WAV(song), 0600)
	_, err := db.Exec("INSERT INTO Track(path,title,isAnalyzed,isBeatGridLocked,bpm,bpmAnalyzed) VALUES('../old.wav','Old title',0,1,121,121)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE PerformanceData SET beatData=?,quickCues=?", []byte("locked grid"), []byte("DJ cues")); err != nil {
		t.Fatal(err)
	}
	audio, err := DecodeWAV(bytes.NewReader(testaudio.WAV(song)))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Analyze(audio, Options{})
	if err != nil {
		t.Fatal(err)
	}
	l, err := OpenLibrary(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err = l.Import(context.Background(), Import{Path: file, Title: "Fresh title", Artist: "Artist", Comment: "[FLAC]", Format: "wav", Playlist: "Stream", Result: r}); err != nil {
		t.Fatal(err)
	}
	var title, format string
	var analyzed, bpm int
	var size int64
	var track, wave, beat, cue []byte
	err = db.QueryRow("SELECT title,fileType,isAnalyzed,bpm,fileBytes,trackData,overviewWaveFormData,beatData,quickCues FROM Track JOIN PerformanceData ON trackId=Track.id").Scan(&title, &format, &analyzed, &bpm, &size, &track, &wave, &beat, &cue)
	if err != nil {
		t.Fatal(err)
	}
	if title != "Fresh title" || format != "wav" || analyzed != 1 || bpm != 121 || size == 0 || len(track) == 0 || len(wave) == 0 || string(beat) != "locked grid" || string(cue) != "DJ cues" {
		t.Fatalf("incomplete or destructive reimport: %s %s %d %d", title, format, analyzed, bpm)
	}
}

func TestLibraryImportRejectsInvalidAnalysisBounds(t *testing.T) {
	path, root, db := testlibrary.Create(t)
	l, err := OpenLibrary(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	file := filepath.Join(root, "bounds.wav")
	if err := os.WriteFile(file, testaudio.WAV(testaudio.Song(4)), 0600); err != nil {
		t.Fatal(err)
	}
	base := Result{
		SampleRate: testaudio.Rate, TotalSamples: 4 * testaudio.Rate, Duration: 4, BPM: 120,
		OverviewSamplesPerEntry: 4.0 * testaudio.Rate / OverviewEntries,
		Overview:                make([]WavePoint, OverviewEntries),
		Beatgrid:                buildBeatgrid(120, .25, testaudio.Rate, 4*testaudio.Rate),
	}
	for _, tc := range []struct {
		name   string
		change func(*Result)
	}{
		{"rate_zero", func(r *Result) { r.SampleRate = 0 }},
		{"rate_low", func(r *Result) { r.SampleRate = 7999 }},
		{"rate_high", func(r *Result) { r.SampleRate = 384001 }},
		{"overview_zero", func(r *Result) { r.OverviewSamplesPerEntry = 0 }},
		{"overview_negative", func(r *Result) { r.OverviewSamplesPerEntry = -1 }},
		{"overview_nan", func(r *Result) { r.OverviewSamplesPerEntry = math.NaN() }},
		{"overview_positive_inf", func(r *Result) { r.OverviewSamplesPerEntry = math.Inf(1) }},
		{"overview_negative_inf", func(r *Result) { r.OverviewSamplesPerEntry = math.Inf(-1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.change(&r)
			id, err := l.Import(context.Background(), Import{Path: file, Playlist: "Bounds", Result: &r})
			if err == nil || err.Error() != "library: incomplete analysis" || id != 0 {
				t.Fatalf("invalid analysis was not rejected before import: id=%d err=%v", id, err)
			}
		})
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM Track").Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid analysis changed Track: count=%d err=%v", count, err)
	}
	// Both endpoints are valid and must survive serialization into Engine blobs.
	for _, sr := range []int{8000, 384000} {
		// A separate track is required: reimport preserves an existing analysis.
		rateFile := filepath.Join(root, fmt.Sprintf("%d.wav", sr))
		if err := os.WriteFile(rateFile, testaudio.WAV(testaudio.Song(4)), 0600); err != nil {
			t.Fatal(err)
		}
		r := base
		r.SampleRate = sr
		r.TotalSamples = int64(sr * 4)
		r.OverviewSamplesPerEntry = float64(r.TotalSamples) / OverviewEntries
		r.Beatgrid = buildBeatgrid(120, .25, sr, r.TotalSamples)
		id, err := l.Import(context.Background(), Import{Path: rateFile, Playlist: "Bounds", Result: &r})
		if err != nil {
			t.Fatalf("valid sample rate %d rejected: %v", sr, err)
		}
		var track, overview []byte
		if err := db.QueryRow("SELECT trackData,overviewWaveFormData FROM PerformanceData WHERE trackId=?", id).Scan(&track, &overview); err != nil {
			t.Fatal(err)
		}
		if got := math.Float64frombits(binary.BigEndian.Uint64(uncompress(t, track)[:8])); got != float64(sr) {
			t.Fatalf("serialized sample rate = %v, want %d", got, sr)
		}
		if got := math.Float64frombits(binary.BigEndian.Uint64(uncompress(t, overview)[16:24])); got != r.OverviewSamplesPerEntry {
			t.Fatalf("serialized overview scale = %v, want %v", got, r.OverviewSamplesPerEntry)
		}
	}
}
