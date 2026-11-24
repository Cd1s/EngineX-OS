// Package rbstate holds the rekordbox bridge's state files: the snapshot beside a stick's m.db,
// the stage of staged tracks on the deck, the volume lock and the stamp. The mount path, the
// background job, cue write-back and the readers of bridge state outside pkg/bridge all share
// these formats; the package imports nothing else of EngineX so that any of them can use it.
//
// See docs/design/rekordbox-bridge-v2.md, section 1.3.
package rbstate

import (
	"fmt"
	"slices"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	// SnapshotName is the snapshot's file name in Engine Library/Database2.
	SnapshotName = ".rb-bridge-snapshot"
	// SyncedName is cue write-back's own state file in the same directory.
	SyncedName = ".rb-bridge-synced"
	// Rules is part of every stamp and track signature: raising it stages every track again.
	// 5 is the first snapshot build; it follows the stamp versions v2-v4 of earlier builds.
	Rules = 5
	// Format is the snapshot's file format.
	Format = 1
)

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

// DBIdentity is m.db as the bridge left it: while it still matches, Engine has written nothing.
type DBIdentity struct {
	Size    int64  `json:"size"`
	Counter uint32 `json:"counter"` // SQLite's file change counter, header bytes 24-27
}

// Pending is the work that is not in m.db yet.
type Pending struct {
	Rows   int  `json:"rows,omitempty"`   // tracks of the view without a row
	Lists  bool `json:"lists,omitempty"`  // the lists are not the view's yet
	Perf   int  `json:"perf,omitempty"`   // rows without performance data for their current signature
	Art    int  `json:"art,omitempty"`    // rows without their cover
	Verify bool `json:"verify,omitempty"` // the export changed and not every track's files were compared yet
}

// None reports that the library is complete for its stamp.
func (p Pending) None() bool { return p == Pending{} }

// Header is the snapshot's first line: all an unchanged insert reads.
type Header struct {
	Format      int        `json:"format"`
	Rules       int        `json:"rules"`
	LibraryUUID string     `json:"uuid"`
	Stamp       string     `json:"stamp"`
	Layout      string     `json:"layout"`
	Sources     []FileSig  `json:"sources"`
	Tracks      int        `json:"tracks"`
	Lists       int        `json:"lists"`
	Pending     Pending    `json:"pending"`
	DB          DBIdentity `json:"db"`
	Applied     int64      `json:"applied"` // unix seconds of the last commit
}

// PerfState says what a row's performance data is.
type PerfState uint8

const (
	PerfNone     PerfState = 0 // what the export alone gives, and what Engine added
	PerfApplied  PerfState = 1 // grid, cues, loops and waveforms written from Built
	PerfDegraded PerfState = 2 // the sources were unreadable for Built; tried again when they change
	PerfDeck     PerfState = 3 // the deck changed the row: it stays the deck's until the export changes
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

// IsZero reports a signature of nothing.
func (s TrackSig) IsZero() bool { return s.Equal(TrackSig{}) }

// Track is one track of the stick's library.
type Track struct {
	Key      string      `json:"k"`
	EngineID int64       `json:"id,omitempty"` // Track.id; 0 while the row is not written
	Rel      string      `json:"p"`            // the row's path without "../"; the export's spelling until Resolved
	Resolved bool        `json:"res,omitempty"`
	Refs     []SourceRef `json:"refs,omitempty"`
	Row      string      `json:"row,omitempty"` // the view hash the row was written from
	Perf     PerfState   `json:"perf,omitempty"`
	Built    TrackSig    `json:"built"`
	Written  []byte      `json:"wr,omitempty"`  // digest of grid, cues and loops as the bridge last wrote them
	Art      string      `json:"art,omitempty"` // hex SHA-1 of the applied cover
	Detail   []string    `json:"det,omitempty"` // ANLZ files that serve the detail waveform
	Native   bool        `json:"nat,omitempty"` // an Engine-made row: lists refer to it, the bridge never writes it
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
// "/" or "../", in NFC and case-folded. It is the equivalence pkg/compat resolves paths by, so
// two spellings that name one file on the volume have one key.
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
// libraries. The sizes and times only name the files; they never choose between libraries.
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
