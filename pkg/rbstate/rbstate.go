// Package rbstate holds the rekordbox bridge's state formats: the snapshot beside a stick's m.db,
// the stage on the deck, the volume lock and the stamp. It imports nothing else of EngineX, so
// every reader of bridge state can use it (docs/design/rekordbox-bridge-v2.md, section 1.3).
package rbstate

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	SnapshotName = ".rb-bridge-snapshot" // in Engine Library/Database2
	// Rules is part of every stamp and track signature: raising it stages every track again.
	// 5 follows the stamp versions v2-v4 of earlier builds.
	Rules  = 5
	Format = 2 // of the snapshot file
)

// ErrLocked: another process holds the volume's lock.
var ErrLocked = errors.New("rbstate: the volume is locked by another bridge process")

// Layouts of a stick with two libraries (the setting rb_library_view).
const (
	LayoutFolders    = "folders"    // three top folders: merged, Device Library, OneLibrary
	LayoutMerged     = "merged"     // the merged view at the root
	LayoutDevice     = "device"     // Device Library only
	LayoutOneLibrary = "onelibrary" // OneLibrary only
)

// LibraryKind names where a track's row comes from.
type LibraryKind uint8

const (
	DeviceLibrary LibraryKind = 1 // PIONEER/rekordbox/export.pdb
	OneLibrary    LibraryKind = 2 // PIONEER/rekordbox/exportLibrary.db
	LinkLibrary   LibraryKind = 3 // a CDJ library staged over Pro DJ Link
)

// FileSig is a file as a stat sees it. Rel is volume-relative, as spelled on the volume.
type FileSig struct {
	Rel     string `json:"r"`
	Size    int64  `json:"s"`
	MTimeNS int64  `json:"m"`
}

// IsZero reports a signature that names no file.
func (f FileSig) IsZero() bool { return f == FileSig{} }

// SourceRef is a track's row in one library.
type SourceRef struct {
	Kind       LibraryKind `json:"k"`
	Source     string      `json:"s,omitempty"` // the staged library's name for LinkLibrary
	ID         uint32      `json:"i"`
	RecordHash string      `json:"h,omitempty"`
}

// Pending is the work that is not in m.db yet.
type Pending struct {
	Rows   int  `json:"rows,omitempty"`   // tracks of the view without a row, or whose row lacks the export's fields
	Lists  bool `json:"lists,omitempty"`  // the lists are not the view's yet
	Perf   int  `json:"perf,omitempty"`   // rows without performance data for their current signature
	Verify bool `json:"verify,omitempty"` // the export changed and not every track's files were compared yet
}

// None reports that the library is complete for its stamp.
func (p Pending) None() bool { return p == Pending{} }

// Header is the snapshot's first line: all an unchanged insert reads.
type Header struct {
	Format      int       `json:"format"`
	Rules       int       `json:"rules"`
	LibraryUUID string    `json:"uuid"`
	Stamp       string    `json:"stamp"`
	Layout      string    `json:"layout"`
	Sources     []FileSig `json:"sources"`
	Tracks      int       `json:"tracks"`
	Lists       int       `json:"lists"`
	Pending     Pending   `json:"pending"`
	Applied     int64     `json:"applied"` // unix seconds of the last commit
	Commit      int64     `json:"commit"`  // the library's commit ID this snapshot describes (see SaveNext)
	// LegacyArtwork records that an adopted older library may still hold TEXT AlbumArt hashes
	// needing bounded background conversion and Artwork/*.jpg extraction.
	LegacyArtwork bool `json:"legacyArtwork,omitempty"`
	// Base, in a swap snapshot only: the state of the stick's library (enginedb.LibraryState)
	// whose every row the next library has taken.
	Base string `json:"base,omitempty"`
}

// PerfState says what a row's performance data is.
type PerfState uint8

const (
	PerfNone     PerfState = 0 // what the export alone gives, and what Engine added
	PerfApplied  PerfState = 1 // grid, cues, loops and waveforms written from Built
	PerfDegraded PerfState = 2 // the sources were unreadable for Built; tried again when they change
	PerfDeck     PerfState = 3 // the deck changed the row: it stays the deck's until the export changes
	PerfMissing  PerfState = 4 // the volume has no file for the track: it has no row until the file is back
)

// TrackSig is everything a track's staged data was built from.
type TrackSig struct {
	Rules   int       `json:"v,omitempty"`
	Row     string    `json:"w,omitempty"` // the view's hash of the export fields
	Audio   FileSig   `json:"a"`
	ANLZ    []FileSig `json:"z,omitempty"` // the .DAT, .EXT and .2EX that exist
	Artwork FileSig   `json:"c"`           // zero when the export names none
}

// Equal reports the same inputs.
func (s TrackSig) Equal(o TrackSig) bool {
	return s.Rules == o.Rules && s.Row == o.Row && s.Audio == o.Audio && s.Artwork == o.Artwork && slices.Equal(s.ANLZ, o.ANLZ)
}

// Track is one track of the stick's library.
type Track struct {
	Key      string      `json:"k"`
	EngineID int64       `json:"id,omitempty"` // Track.id; 0 while the row is not written
	Rel      string      `json:"p"`            // the row's path without "../"; the export's spelling until Resolved
	Resolved bool        `json:"res,omitempty"`
	Refs     []SourceRef `json:"refs,omitempty"`
	Row      string      `json:"row,omitempty"` // the view hash the row was written from
	Rating   int         `json:"rt,omitempty"`  // the rating the bridge last wrote: another one in the row is the deck's
	Perf     PerfState   `json:"perf,omitempty"`
	Built    TrackSig    `json:"built"`
	Written  []byte      `json:"wr,omitempty"` // digest of grid, cues and loops as the bridge last wrote them
	// Write-back's note of the row: the digest of its cues, loops and tags the export holds, and
	// the row's lastEditTime then. A row whose time has not moved is not read.
	Synced     []byte `json:"sy,omitempty"`
	Edit       int64  `json:"ed,omitempty"`
	Native     bool   `json:"nat,omitempty"` // an Engine-made row: lists refer to it, the bridge never writes it
	FastCues   []byte `json:"frc,omitempty"`
	FastLoops  []byte `json:"frl,omitempty"`
	FastActive uint32 `json:"fra,omitempty"`
	FastKnown  bool   `json:"frk,omitempty"`
}

// List is one playlist or folder of the view.
type List struct {
	Key      string `json:"k"`
	EngineID int64  `json:"id,omitempty"` // Playlist.id
	Hash     string `json:"h"`
}

// Snapshot is the last import of a stick: what m.db holds of the export, by key.
type Snapshot struct {
	Header
	Tracks map[string]*Track
	Lists  map[string]*List
}

// New returns an empty snapshot of the library uuid.
func New(uuid string) *Snapshot {
	return &Snapshot{
		Header: Header{Format: Format, Rules: Rules, LibraryUUID: uuid},
		Tracks: map[string]*Track{},
		Lists:  map[string]*List{},
	}
}

// TrackAt is the track of an Engine path ("../Contents/…") or of a volume-relative path.
func (s *Snapshot) TrackAt(enginePath string) (*Track, bool) {
	t, ok := s.Tracks[FoldPath(enginePath)]
	return t, ok
}

// EngineIDs is the set of Track.id values the bridge owns in the library.
func (s *Snapshot) EngineIDs() map[int64]bool {
	ids := make(map[int64]bool, len(s.Tracks))
	for _, t := range s.Tracks {
		if t.EngineID != 0 && !t.Native {
			ids[t.EngineID] = true
		}
	}
	return ids
}

// FoldPath is a track's key: its volume-relative path with forward slashes, without a leading
// "/" or "../", NFC and case-folded, the equivalence pkg/compat resolves paths by.
func FoldPath(rel string) string {
	rel = strings.ReplaceAll(rel, "\\", "/")
	rel = strings.TrimPrefix(rel, "../")
	rel = strings.TrimLeft(rel, "/")
	ascii, upper := true, false
	for i := 0; i < len(rel); i++ {
		c := rel[i]
		if c >= 0x80 {
			ascii = false
			break
		}
		upper = upper || (c >= 'A' && c <= 'Z')
	}
	if ascii {
		if !upper {
			return rel
		}
		return strings.ToLower(rel)
	}
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(rel)))
}

// Stamp names an import's inputs: the rules, the layout, each export file and the staged CDJ
// libraries. Sizes and times only name the files; they never choose between libraries.
func Stamp(layout string, sources []FileSig, staged string) string {
	if len(sources) == 0 && staged == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "v%d %s", Rules, layout)
	for _, f := range sources {
		fmt.Fprintf(&b, " | %s %d %d", f.Rel, f.Size, f.MTimeNS)
	}
	if staged != "" {
		b.WriteString(" | staged " + staged)
	}
	return b.String()
}
