package rbstate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	// BrowseIndexName sits beside .rb-bridge-snapshot in Engine Library/Database2 when on-demand
	// browse direct-read is active. It is a flat, pull-safe, C-readable index mapping Track.id ->
	// AnalysisPath (for on-demand .rgb / overviewWaveFormData) and base64url(DirectArtworkHash) ->
	// ArtworkPath (for on-demand Engine Library/Artwork/*.jpg -> PIONEER/Artwork/... redirect).
	BrowseIndexName = ".rb-bridge-browse"
	BrowseSwapName  = BrowseIndexName + ".swap"
	browseMagic     = "RBB1 "
)

// DirectArtworkHash is the deterministic 20-byte AlbumArt.hash used for on-demand direct-read
// covers from PIONEER/Artwork/... (AlbumArt.albumArt = NULL).
func DirectArtworkHash(artworkPath string) []byte {
	sum := sha1.Sum([]byte("rb:" + artworkPath))
	return sum[:]
}

// DirectArtworkStem is the Engine Library/Artwork/<stem>.jpg filename stem for artworkPath.
func DirectArtworkStem(artworkPath string) string {
	return base64.RawURLEncoding.EncodeToString(DirectArtworkHash(artworkPath))
}

func cleanBrowseField(s string) bool {
	return s != "" && !strings.ContainsAny(s, "\t\r\n\x00")
}

// BrowseTag binds a .rb-bridge-browse file to a specific snapshot generation, build mode, and export stamp.
func BrowseTag(h Header) string {
	return fmt.Sprintf("v1:%d:%d:%s:%s", h.Commit, h.Applied, h.Mode, h.Stamp)
}

// ReadBrowseHeader reads the RBB1 header line from file without loading track/artwork rows.
func ReadBrowseHeader(file string) (uuid, tag string, err error) {
	f, err := os.Open(file)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", "", err
	}
	if !st.Mode().IsRegular() {
		return "", "", fmt.Errorf("browse index is not a regular file")
	}
	r := bufio.NewReaderSize(io.LimitReader(f, 4096), 4096)
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", "", err
	}
	if !strings.HasSuffix(line, "\n") {
		return "", "", fmt.Errorf("browse index header truncated")
	}
	if !strings.HasPrefix(line, browseMagic) {
		return "", "", fmt.Errorf("browse index invalid magic")
	}
	rest := strings.TrimRight(line[len(browseMagic):], "\r\n")
	u, tg, _ := strings.Cut(rest, " ")
	if u == "" {
		return "", "", fmt.Errorf("browse index missing uuid")
	}
	return u, tg, nil
}

// BrowseIndexMatchesFile reports whether file is an RBB1 index whose UUID and tag match h.
func BrowseIndexMatchesFile(file string, h Header) bool {
	if h.LibraryUUID == "" {
		return false
	}
	uuid, tag, err := ReadBrowseHeader(file)
	return err == nil && uuid == h.LibraryUUID && tag != "" && tag == BrowseTag(h)
}

// BrowseIndexMatches reports whether dbDir/.rb-bridge-browse matches snapshot header h.
func BrowseIndexMatches(dbDir string, h Header) bool {
	return BrowseIndexMatchesFile(filepath.Join(dbDir, BrowseIndexName), h)
}

// EncodeBrowseIndex formats the snapshot's track analysis and artwork mappings as a flat text
// table readable directly by the C browse hook without parsing gob.
func EncodeBrowseIndex(s *Snapshot) []byte {
	var b bytes.Buffer
	if s == nil {
		return nil
	}
	b.Grow(64 + len(s.Tracks)*96)
	b.WriteString(browseMagic)
	b.WriteString(s.LibraryUUID)
	b.WriteByte(' ')
	b.WriteString(BrowseTag(s.Header))
	b.WriteByte('\n')

	tracks := make([]*Track, 0, len(s.Tracks))
	artByStem := make(map[string]string)
	for _, t := range s.Tracks {
		if t == nil || t.Native || t.EngineID <= 0 || t.Perf == PerfMissing {
			continue
		}
		if cleanBrowseField(t.AnalysisPath) {
			tracks = append(tracks, t)
		}
		if cleanBrowseField(t.ArtworkPath) {
			stem := DirectArtworkStem(t.ArtworkPath)
			if _, exists := artByStem[stem]; !exists {
				artByStem[stem] = t.ArtworkPath
			}
		}
	}
	slices.SortFunc(tracks, func(a, c *Track) int {
		if a.EngineID < c.EngineID {
			return -1
		}
		if a.EngineID > c.EngineID {
			return 1
		}
		return strings.Compare(a.Key, c.Key)
	})
	for _, t := range tracks {
		sr := t.SampleRate
		if sr <= 0 {
			sr = 44100
		}
		dur := t.Duration
		if dur < 0 {
			dur = 0
		}
		fmt.Fprintf(&b, "T\t%d\t%.3f\t%d\t%s\n", t.EngineID, dur, sr, t.AnalysisPath)
	}
	stems := make([]string, 0, len(artByStem))
	for stem := range artByStem {
		stems = append(stems, stem)
	}
	slices.Sort(stems)
	for _, stem := range stems {
		fmt.Fprintf(&b, "A\t%s\t%s\n", stem, artByStem[stem])
	}
	return b.Bytes()
}

// SaveBrowseIndex writes .rb-bridge-browse into dbDir, pull-safe.
func SaveBrowseIndex(dbDir string, s *Snapshot) error {
	return SaveBrowseIndexCtx(context.Background(), dbDir, s)
}

// SaveBrowseIndexCtx writes .rb-bridge-browse while honoring ctx cancellation.
func SaveBrowseIndexCtx(ctx context.Context, dbDir string, s *Snapshot) error {
	return SaveBrowseIndexAsCtx(ctx, filepath.Join(dbDir, BrowseIndexName), s)
}

// SaveBrowseIndexAs writes the browse index to file, pull-safe.
func SaveBrowseIndexAs(file string, s *Snapshot) error {
	return SaveBrowseIndexAsCtx(context.Background(), file, s)
}

// SaveBrowseIndexAsCtx encodes and writes the browse index while honoring ctx cancellation.
func SaveBrowseIndexAsCtx(ctx context.Context, file string, s *Snapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ReplaceFileCtx(ctx, file, EncodeBrowseIndex(s))
}
