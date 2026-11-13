package enginedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestPrepare252UnreadableAudioDoesNotAbortImport(t *testing.T) {
	for _, failure := range []string{"missing", "stat-permission", "header-permission"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			testfixture.LargeExport(t, root, 3)
			l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
			if err != nil {
				t.Fatal(err)
			}
			bad := filepath.Join(root, strings.TrimPrefix(l.Tracks[1].Path, "/"))
			oldStat, oldHeader := statFile, audioHeader
			t.Cleanup(func() { statFile, audioHeader = oldStat, oldHeader })
			var removed []byte
			switch failure {
			case "missing":
				removed, err = os.ReadFile(bad)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(bad); err != nil {
					t.Fatal(err)
				}
			case "stat-permission":
				statFile = func(p string) (os.FileInfo, error) {
					if p == bad {
						return nil, &os.PathError{Op: "stat", Path: p, Err: os.ErrPermission}
					}
					return oldStat(p)
				}
			case "header-permission":
				audioHeader = func(p string) (compat.AudioInfo, error) {
					if p == bad {
						return compat.AudioInfo{}, &os.PathError{Op: "open", Path: p, Err: os.ErrPermission}
					}
					return oldHeader(p)
				}
			}
			dir := t.TempDir()
			r, err := Sync(context.Background(), root, l, Options{DatabaseDir: dir})
			if err != nil || r.Added != 3 || r.Degraded != 1 || r.Deferred != 1 {
				t.Fatalf("one unreadable file: %+v %v", r, err)
			}
			db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var members int
			if err = db.QueryRow(`SELECT count(*) FROM PlaylistEntity`).Scan(&members); err != nil || members != 3 {
				t.Fatalf("playlist members=%d: %v", members, err)
			}
			statFile, audioHeader = oldStat, oldHeader
			if failure == "missing" {
				if err = os.WriteFile(bad, removed, 0644); err != nil {
					t.Fatal(err)
				}
			}
			r, err = Sync(context.Background(), root, l, Options{DatabaseDir: dir, Resume: true})
			if err != nil || r.Updated != 1 || r.Unchanged != 2 || r.Degraded != 0 {
				t.Fatalf("recovered file: %+v %v", r, err)
			}
		})
	}
}

func TestPrepare252UnreadableFirstTrackCannotStarveResumedMounts(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	mp3, err := os.ReadFile("../../internal/audioheader/testdata/01-lame-cbr-info-44100.mp3")
	if err != nil {
		t.Fatal(err)
	}
	l := &rekordbox.Library{}
	for i := 1; i <= 4; i++ {
		p := fmt.Sprintf("Music/t%d.mp3", i)
		testfixture.Write(t, root, p, mp3)
		l.Tracks = append(l.Tracks, rekordbox.Track{ID: uint32(i), Path: "/" + strings.ToUpper(p)})
	}
	if _, err := Sync(context.Background(), root, l, Options{Fast: true, DatabaseDir: dir}); err != nil {
		t.Fatal(err)
	}
	oldStat, oldHeader := statFile, audioHeader
	t.Cleanup(func() { statFile, audioHeader = oldStat, oldHeader })
	clock := time.Unix(1000, 0)
	stats, headers := 0, 0
	broken := true
	statFile = func(p string) (os.FileInfo, error) {
		stats++
		clock = clock.Add(2 * time.Second)
		if broken && filepath.Base(p) == "t1.mp3" {
			return nil, &os.PathError{Op: "stat", Path: p, Err: os.ErrPermission}
		}
		return oldStat(p)
	}
	audioHeader = func(p string) (compat.AudioInfo, error) {
		headers++
		clock = clock.Add(2 * time.Second)
		return oldHeader(p)
	}
	for mount := 1; mount <= 4; mount++ {
		beforeStats, beforeHeaders := stats, headers
		if mount == 4 {
			broken = false
		}
		r, err := Sync(context.Background(), root, l, Options{DatabaseDir: dir, Resume: true, Deadline: clock.Add(time.Second), Now: func() time.Time { return clock }})
		if err != nil || r.Updated != 1 || r.Deferred != 4-mount {
			t.Fatalf("mount %d: %+v %v", mount, r, err)
		}
		if stats-beforeStats > 2 || headers-beforeHeaders != 1 {
			t.Fatalf("mount %d: stats=%d headers=%d", mount, stats-beforeStats, headers-beforeHeaders)
		}
	}
}

func TestPrepare252MissingCaseDifferentFinishedTrackKeepsDeckEdits(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	testfixture.LargeExport(t, root, 2)
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Sync(context.Background(), root, l, Options{DatabaseDir: dir}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`UPDATE PerformanceData SET quickCues=x'123456' WHERE trackId=(SELECT id FROM Track WHERE path=?)`, ".."+l.Tracks[0].Path); err != nil {
		t.Fatal(err)
	}
	actual := strings.TrimPrefix(l.Tracks[0].Path, "/")
	data, err := os.ReadFile(filepath.Join(root, actual))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(root, actual)); err != nil {
		t.Fatal(err)
	}
	l.Tracks[0].Path = strings.ToUpper(l.Tracks[0].Path)
	for _, restored := range []bool{false, true} {
		if restored {
			if err = os.WriteFile(filepath.Join(root, actual), data, 0644); err != nil {
				t.Fatal(err)
			}
		}
		r, err := Sync(context.Background(), root, l, Options{DatabaseDir: dir, Resume: true})
		if err != nil || r.Unchanged != 2 || r.Updated != 0 || r.Deferred != 0 {
			t.Fatalf("restored=%v: %+v %v", restored, r, err)
		}
		var cues string
		if err = db.QueryRow(`SELECT hex(quickCues) FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.path=?`, "../"+actual).Scan(&cues); err != nil || cues != "123456" {
			t.Fatalf("restored=%v: cues=%q %v", restored, cues, err)
		}
	}
	// A legacy-offset upgrade still has to inspect this file when it returns.
	if err = os.Remove(filepath.Join(root, actual)); err != nil {
		t.Fatal(err)
	}
	r, err := Sync(context.Background(), root, l, Options{DatabaseDir: dir, Resume: true, LegacyOffsets: true})
	if err != nil || r.Deferred != 1 || r.Updated != 0 {
		t.Fatalf("missing legacy-offset upgrade: %+v %v", r, err)
	}
	if err = os.WriteFile(filepath.Join(root, actual), data, 0644); err != nil {
		t.Fatal(err)
	}
	r, err = Sync(context.Background(), root, l, Options{DatabaseDir: dir, Resume: true, LegacyOffsets: true})
	if err != nil || r.Deferred != 0 || r.Updated != 1 {
		t.Fatalf("resumed legacy-offset upgrade: %+v %v", r, err)
	}
	// A failed move cannot borrow another source's completed path.
	l.Tracks[0].Path = "/missing.mp3"
	l.Tracks[1].Path = "/" + actual
	if _, err = Sync(context.Background(), root, l, Options{DatabaseDir: dir, Resume: true}); err == nil || !strings.Contains(err.Error(), "same audio path") {
		t.Fatalf("conflicting retained target: %v", err)
	}
}

func TestPrepare252ErrorsRemainFatal(t *testing.T) {
	root := t.TempDir()
	testfixture.Write(t, root, "MUSIC/a.mp3", []byte("audio"))
	for _, name := range []string{"traversal", "backslash-traversal", "nul", "duplicate", "symlink", "canceled", "deadline", "nonregular", "ambiguous"} {
		t.Run(name, func(t *testing.T) {
			l := &rekordbox.Library{Tracks: []rekordbox.Track{{ID: 1, Path: "/MUSIC/a.mp3"}}}
			done := map[string]string{"../MUSIC/a.mp3": "finished"}
			ctx := context.Background()
			switch name {
			case "traversal":
				l.Tracks[0].Path = "/elsewhere/../MUSIC/a.mp3"
			case "backslash-traversal":
				l.Tracks[0].Path = `\elsewhere\..\MUSIC\a.mp3`
			case "nul":
				l.Tracks[0].Path += "\x00"
			case "duplicate":
				l.Tracks = append(l.Tracks, rekordbox.Track{ID: 2, Path: "/music/A.MP3"})
			case "symlink":
				if err := os.Symlink(filepath.Join(root, "MUSIC/a.mp3"), filepath.Join(root, "link.mp3")); err != nil {
					t.Fatal(err)
				}
				l.Tracks[0].Path = "/link.mp3"
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "deadline":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			case "nonregular":
				l.Tracks[0].Path = "/MUSIC"
			case "ambiguous":
				testfixture.Write(t, root, "MUSIC/A.mp3", []byte("other audio"))
				l.Tracks[0].Path = "/MUSIC/a.MP3"
			}
			res, err := compat.NewResolver(root)
			if err != nil {
				t.Fatal(err)
			}
			_, err = prepareTracks(ctx, res, l, time.Time{}, time.Now, done)
			if err == nil {
				t.Fatal("import accepted a fatal error")
			}
			if name == "canceled" && !errors.Is(err, context.Canceled) || name == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancellation meaning lost: %v", err)
			}
			if name == "duplicate" && !strings.Contains(err.Error(), "same audio path") {
				t.Fatalf("duplicate meaning lost: %v", err)
			}
		})
	}
}

func TestPrepare252FileCancellationRemainsFatal(t *testing.T) {
	for _, stage := range []string{"stat", "header"} {
		for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
			t.Run(stage+"/"+cause.Error(), func(t *testing.T) {
				root := t.TempDir()
				testfixture.Write(t, root, "audio.mp3", []byte("audio"))
				res, err := compat.NewResolver(root)
				if err != nil {
					t.Fatal(err)
				}
				oldStat, oldHeader := statFile, audioHeader
				t.Cleanup(func() { statFile, audioHeader = oldStat, oldHeader })
				wrapped := &os.PathError{Op: stage, Path: "audio.mp3", Err: cause}
				if stage == "stat" {
					statFile = func(string) (os.FileInfo, error) { return nil, wrapped }
				} else {
					audioHeader = func(string) (compat.AudioInfo, error) { return compat.AudioInfo{}, wrapped }
				}
				l := &rekordbox.Library{Tracks: []rekordbox.Track{{ID: 1, Path: "/audio.mp3"}}}
				if _, err = prepareTracks(context.Background(), res, l, time.Time{}, time.Now, nil); !errors.Is(err, cause) {
					t.Fatalf("file cancellation lost: %v", err)
				}
			})
		}
	}
}

func TestPrepare252UnreadableDirectoryCannotHideSymlink(t *testing.T) {
	if os.Geteuid() == 0 {
		// Root can list execute-only directories; run this boundary as a user.
		dir, err := os.MkdirTemp("", "prepare252-permission-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		if err = os.Chmod(dir, 0755); err != nil {
			t.Fatal(err)
		}
		binary, err := os.ReadFile(os.Args[0])
		if err != nil {
			t.Fatal(err)
		}
		exe := filepath.Join(dir, "permission.test")
		if err = os.WriteFile(exe, binary, 0755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^TestPrepare252UnreadableDirectoryCannotHideSymlink$")
		cmd.Dir = dir
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("unprivileged boundary: %v\n%s", err, out)
		}
		return
	}
	root, outside := t.TempDir(), t.TempDir()
	testfixture.Write(t, outside, "audio.mp3", []byte("audio"))
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "audio.mp3"), filepath.Join(private, "audio.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(private, 0111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(private, 0755) })
	res, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	l := &rekordbox.Library{Tracks: []rekordbox.Track{{ID: 1, Path: "/private/audio.mp3"}}}
	if _, err = prepareTracks(context.Background(), res, l, time.Time{}, time.Now, nil); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unverified path was accepted: %v", err)
	}
}

func TestPrepare252CaseDifferencesBoundWorkAndMakeProgress(t *testing.T) {
	const n = 8
	root := t.TempDir()
	mp3, err := os.ReadFile("../../internal/audioheader/testdata/01-lame-cbr-info-44100.mp3")
	if err != nil {
		t.Fatal(err)
	}
	l := &rekordbox.Library{}
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("Music/t%d.mp3", i)
		testfixture.Write(t, root, p, mp3)
		l.Tracks = append(l.Tracks, rekordbox.Track{ID: uint32(i + 1), Path: "/" + strings.ToUpper(p)})
	}
	// These are distinct files on a case-sensitive volume, both in the export.
	testfixture.Write(t, root, "Music/T0.mp3", mp3)
	l.Tracks = append(l.Tracks, rekordbox.Track{ID: n + 1, Path: "/Music/T0.mp3"})
	l.Tracks[0].Path = "/Music/t0.mp3"
	oldStat, oldHeader := statFile, audioHeader
	t.Cleanup(func() { statFile, audioHeader = oldStat, oldHeader })
	clock := time.Unix(1000, 0)
	stats, headers := 0, map[string]int{}
	statFile = func(p string) (os.FileInfo, error) {
		stats++
		clock = clock.Add(time.Second)
		return oldStat(p)
	}
	audioHeader = func(p string) (compat.AudioInfo, error) {
		headers[p]++
		clock = clock.Add(time.Second)
		return oldHeader(p)
	}
	done := map[string]string{}
	for mount := 0; mount <= n+1; mount++ {
		res, err := compat.NewResolver(root)
		if err != nil {
			t.Fatal(err)
		}
		before, previous := stats, len(done)
		p, err := prepareTracks(context.Background(), res, l, clock.Add(3*time.Second), func() time.Time { return clock }, done)
		if err != nil {
			t.Fatal(err)
		}
		for _, track := range p {
			if !track.late && !track.uninspected {
				done["../"+track.rel] = track.hash
			}
		}
		if stats-before > 2 {
			t.Fatalf("mount %d stat'ed %d tracks beyond budget", mount, stats-before)
		}
		if previous == n+1 {
			if stats != before {
				t.Fatal("finished tracks repeated preparation")
			}
			for p, reads := range headers {
				if reads != 1 {
					t.Fatalf("%s read %d times", p, reads)
				}
			}
			if len(headers) != n+1 {
				t.Fatal("distinct case-sensitive paths were merged")
			}
			return
		}
		if len(done) <= previous {
			t.Fatalf("mount %d made no progress: %d finished", mount, len(done))
		}
	}
	t.Fatal("repeated mounts never finished")
}
