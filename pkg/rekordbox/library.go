// Package rekordbox reads Pioneer exports without opening any source for writing.
package rekordbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"unicode/utf8"
)

type Track struct {
	ID                 uint32
	Path               string
	AnalysisPath       string
	Title              string
	Artist             string
	Album              string
	Genre              string
	Label              string
	Composer           string
	Remixer            string
	Comment            string
	Key                string
	ArtworkPath        string
	BPM                float64
	Duration           float64 // seconds
	SampleCount        uint64  // exact container sample count, when available
	SampleRate         int
	BitDepth           int
	Bitrate            int
	Year               int
	Rating             int
	FileSize           int64
	RecordHash         string
	StartOffsetSamples int   `json:",omitempty"`
	Cues               []Cue `json:",omitempty"`
}

// Metadata is the editable library metadata that Engine can carry from a deck
// edit back to a Pioneer export.
type Metadata struct {
	Genre   string
	Comment string
	Label   string
}

type Playlist struct {
	ID, ParentID, SortOrder uint32
	Name                    string
	Folder, Intelligent     bool
	Rules                   string
	Tracks                  []uint32 // ordered, materialized membership, including intelligent playlists
}

type Library struct {
	Tracks    []Track
	Playlists []Playlist
	Artwork   map[uint32]string
	Keys      map[uint32]string
	Columns   [][]byte // unknown browse-column definitions retained verbatim
}

func RecordHash(t Track) string {
	t.RecordHash = ""
	if math.IsNaN(t.BPM) || math.IsInf(t.BPM, 0) || math.IsNaN(t.Duration) || math.IsInf(t.Duration, 0) {
		return hashRecordBytes(nil)
	}
	var scratch [512]byte
	b := appendTrackJSON(scratch[:0], t)
	return hashRecordBytes(b)
}

func hashRecordBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func appendJSONString(b []byte, s string) []byte {
	fast := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c >= 0x80 || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			fast = false
			break
		}
	}
	if fast {
		b = append(b, '"')
		b = append(b, s...)
		return append(b, '"')
	}
	b = append(b, '"')
	for len(s) > 0 {
		r, n := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && n == 1 {
			r = utf8.RuneError
			n = 1
		}
		s = s[n:]
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\b':
			b = append(b, '\\', 'b')
		case '\f':
			b = append(b, '\\', 'f')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		case '<':
			b = append(b, '\\', 'u', '0', '0', '3', 'c')
		case '>':
			b = append(b, '\\', 'u', '0', '0', '3', 'e')
		case '&':
			b = append(b, '\\', 'u', '0', '0', '2', '6')
		case '\u2028':
			b = append(b, '\\', 'u', '2', '0', '2', '8')
		case '\u2029':
			b = append(b, '\\', 'u', '2', '0', '2', '9')
		default:
			if r < 0x20 {
				const hex = "0123456789abcdef"
				b = append(b, '\\', 'u', '0', '0', hex[r>>4], hex[r&0xf])
			} else {
				b = utf8.AppendRune(b, r)
			}
		}
	}
	return append(b, '"')
}

func appendJSONFieldName(b []byte, name string) []byte {
	b = append(b, '"')
	b = append(b, name...)
	return append(b, '"', ':')
}

func appendJSONUint(b []byte, v uint64) []byte {
	return strconv.AppendUint(b, v, 10)
}

func appendJSONInt(b []byte, v int64) []byte {
	return strconv.AppendInt(b, v, 10)
}

func appendJSONFloat(b []byte, v float64) []byte {
	return strconv.AppendFloat(b, v, 'g', -1, 64)
}

func appendJSONBool(b []byte, v bool) []byte {
	return strconv.AppendBool(b, v)
}

func appendJSONField(b []byte, first *bool, name string) []byte {
	if !*first {
		b = append(b, ',')
	}
	*first = false
	return appendJSONFieldName(b, name)
}

func appendCueJSON(b []byte, c Cue) []byte {
	b = append(b, '{')
	b = appendJSONFieldName(b, "HotCue")
	b = appendJSONUint(b, uint64(c.HotCue))
	b = append(b, ',')
	b = appendJSONFieldName(b, "TimeMS")
	b = appendJSONUint(b, uint64(c.TimeMS))
	b = append(b, ',')
	b = appendJSONFieldName(b, "EndMS")
	b = appendJSONUint(b, uint64(c.EndMS))
	b = append(b, ',')
	b = appendJSONFieldName(b, "Loop")
	b = appendJSONBool(b, c.Loop)
	b = append(b, ',')
	b = appendJSONFieldName(b, "Active")
	b = appendJSONBool(b, c.Active)
	b = append(b, ',')
	b = appendJSONFieldName(b, "Name")
	b = appendJSONString(b, c.Name)
	b = append(b, ',')
	b = appendJSONFieldName(b, "Color")
	b = append(b, '[')
	for i, v := range c.Color {
		if i != 0 {
			b = append(b, ',')
		}
		b = appendJSONUint(b, uint64(v))
	}
	b = append(b, ']', ',')
	b = appendJSONFieldName(b, "ColorID")
	b = appendJSONUint(b, uint64(c.ColorID))
	b = append(b, ',')
	b = appendJSONFieldName(b, "ColorCode")
	b = appendJSONUint(b, uint64(c.ColorCode))
	b = append(b, ',')
	b = appendJSONFieldName(b, "LoopNumerator")
	b = appendJSONUint(b, uint64(c.LoopNumerator))
	b = append(b, ',')
	b = appendJSONFieldName(b, "LoopDenominator")
	b = appendJSONUint(b, uint64(c.LoopDenominator))
	b = append(b, ',')
	b = appendJSONFieldName(b, "Extended")
	b = appendJSONBool(b, c.Extended)
	return append(b, '}')
}

func appendTrackJSON(b []byte, t Track) []byte {
	b = append(b, '{')
	first := true
	b = appendJSONField(b, &first, "ID")
	b = appendJSONUint(b, uint64(t.ID))
	b = appendJSONString(appendJSONField(b, &first, "Path"), t.Path)
	b = appendJSONString(appendJSONField(b, &first, "AnalysisPath"), t.AnalysisPath)
	b = appendJSONString(appendJSONField(b, &first, "Title"), t.Title)
	b = appendJSONString(appendJSONField(b, &first, "Artist"), t.Artist)
	b = appendJSONString(appendJSONField(b, &first, "Album"), t.Album)
	b = appendJSONString(appendJSONField(b, &first, "Genre"), t.Genre)
	b = appendJSONString(appendJSONField(b, &first, "Label"), t.Label)
	b = appendJSONString(appendJSONField(b, &first, "Composer"), t.Composer)
	b = appendJSONString(appendJSONField(b, &first, "Remixer"), t.Remixer)
	b = appendJSONString(appendJSONField(b, &first, "Comment"), t.Comment)
	b = appendJSONString(appendJSONField(b, &first, "Key"), t.Key)
	b = appendJSONString(appendJSONField(b, &first, "ArtworkPath"), t.ArtworkPath)
	b = appendJSONFloat(appendJSONField(b, &first, "BPM"), t.BPM)
	b = appendJSONFloat(appendJSONField(b, &first, "Duration"), t.Duration)
	b = appendJSONUint(appendJSONField(b, &first, "SampleCount"), t.SampleCount)
	b = appendJSONInt(appendJSONField(b, &first, "SampleRate"), int64(t.SampleRate))
	b = appendJSONInt(appendJSONField(b, &first, "BitDepth"), int64(t.BitDepth))
	b = appendJSONInt(appendJSONField(b, &first, "Bitrate"), int64(t.Bitrate))
	b = appendJSONInt(appendJSONField(b, &first, "Year"), int64(t.Year))
	b = appendJSONInt(appendJSONField(b, &first, "Rating"), int64(t.Rating))
	b = appendJSONInt(appendJSONField(b, &first, "FileSize"), t.FileSize)
	b = appendJSONString(appendJSONField(b, &first, "RecordHash"), "")
	if t.StartOffsetSamples != 0 {
		b = appendJSONInt(appendJSONField(b, &first, "StartOffsetSamples"), int64(t.StartOffsetSamples))
	}
	if len(t.Cues) != 0 {
		if !first {
			b = append(b, ',')
		}
		first = false
		b = appendJSONFieldName(b, "Cues")
		b = append(b, '[')
		for i, c := range t.Cues {
			if i != 0 {
				b = append(b, ',')
			}
			b = appendCueJSON(b, c)
		}
		b = append(b, ']')
	}
	return append(b, '}')
}

// Validate rejects broken trees instead of silently losing subfolders or tracks.
func (l *Library) Validate() error {
	tracks := map[uint32]bool{}
	for i := range l.Tracks {
		t := &l.Tracks[i]
		if t.ID == 0 || tracks[t.ID] {
			return fmt.Errorf("rekordbox: duplicate/zero track id %d", t.ID)
		}
		tracks[t.ID] = true
		if t.Path == "" {
			return fmt.Errorf("rekordbox: track %d has no path", t.ID)
		}
		if math.IsNaN(t.BPM) || math.IsInf(t.BPM, 0) || t.BPM < 0 || t.BPM > 1000 || math.IsNaN(t.Duration) || math.IsInf(t.Duration, 0) || t.Duration < 0 || t.Duration > 1e7 || t.SampleRate < 0 || t.SampleRate > 768000 || t.StartOffsetSamples < 0 {
			return fmt.Errorf("rekordbox: invalid audio metadata for track %d", t.ID)
		}
		for _, c := range t.Cues {
			if c.HotCue > 8 || (c.Loop && c.EndMS < c.TimeMS) {
				return fmt.Errorf("rekordbox: invalid cue metadata for track %d", t.ID)
			}
		}
		if t.RecordHash == "" {
			t.RecordHash = RecordHash(*t)
		}
	}
	parents := map[uint32]uint32{}
	for i := range l.Playlists {
		p := &l.Playlists[i]
		if _, ok := parents[p.ID]; p.ID == 0 || ok {
			return fmt.Errorf("rekordbox: duplicate/zero playlist id %d", p.ID)
		}
		parents[p.ID] = p.ParentID
		validTracks := make([]uint32, 0, len(p.Tracks))
		for _, id := range p.Tracks {
			if tracks[id] {
				validTracks = append(validTracks, id)
			}
		}
		p.Tracks = validTracks
	}
	promoted := map[uint32]bool{}
	for i := range l.Playlists {
		p := &l.Playlists[i]
		if p.ParentID != 0 {
			if _, ok := parents[p.ParentID]; !ok {
				p.ParentID = 0
				parents[p.ID] = 0
				promoted[p.ID] = true
			}
		}
	}
	names := map[string]bool{}
	for _, wantPromoted := range []bool{false, true} {
		for i := range l.Playlists {
			p := &l.Playlists[i]
			if promoted[p.ID] != wantPromoted {
				continue
			}
			k := fmt.Sprintf("%d/%s", p.ParentID, p.Name)
			if names[k] {
				baseName := p.Name
				for count := 2; ; count++ {
					cand := fmt.Sprintf("%s (%d)", baseName, count)
					candKey := fmt.Sprintf("%d/%s", p.ParentID, cand)
					if !names[candKey] {
						p.Name = cand
						names[candKey] = true
						break
					}
				}
			} else {
				names[k] = true
			}
		}
	}
	for id := range parents {
		seen := map[uint32]bool{}
		for p := id; p != 0; p = parents[p] {
			if seen[p] {
				return fmt.Errorf("rekordbox: playlist cycle at %d", p)
			}
			seen[p] = true
		}
	}
	sort.SliceStable(l.Playlists, func(i, j int) bool {
		a, b := l.Playlists[i], l.Playlists[j]
		if a.ParentID != b.ParentID {
			return a.ParentID < b.ParentID
		}
		if a.SortOrder != b.SortOrder {
			return a.SortOrder < b.SortOrder
		}
		return a.ID < b.ID
	})
	return nil
}
