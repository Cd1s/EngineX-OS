# rekordbox bridge v2: design

Branch `agent/5306-rb-rewrite`, from `7a979405`. Section 1 fixes the module boundaries (the parallel lanes built
against it). Sections 3 to 7 give the deck's limits, the scenarios, the state machine with its crash safety, the test
gate and the evidence for the open questions. Section 2, the requirements inventory with its tests, is pushed
separately.

Words used here:

- *stick*: a USB or SD volume with a rekordbox export.
- *window*: the time the edisksd hook gives `rb-bridge --mount-path` before Engine sees the stick. It happens at an
  insert and at the synthetic remount the background job asks for.
- *Device Library*: `PIONEER/rekordbox/export.pdb`. *OneLibrary*: `PIONEER/rekordbox/exportLibrary.db`.
- *key*: a track's identity, the case-folded NFC form of its volume-relative path (`rbstate.FoldPath`).

## 0. Why the old code failed on the deck on 2026-11-24

It was not a slow step inside `quickUpdate`. The quick update never ran:

- The hook exports `ENGINEX_HOOK_REMAINING_MS` (`tools/edisksd-hook/edisksd_hook.c:1860`).
- `internal/cli/rbbridge.go:138-142` turns it into `opts.Deadline`, which is the instant of the hook's own SIGKILL.
- The quick-update branch requires `opts.Deadline.IsZero()` (`pkg/bridge/bridge.go:986`), so a mount started by the
  hook skips it and its hand-off.
- The mount then updates in place: a folder walk for all tracks (`ReconcileContext`), then file reads until
  `opts.Deadline`. Its give-up time is `max(start + 17 s, opts.Deadline)` (`bridge.go:1034-1038`), never before the
  kill. Nothing is logged, nothing is handed off.
- No test sets the variable, and the replica called `bridge.ConvertAtMount` with a zero deadline, so it measured a
  path the deck cannot reach.

Two rules for v2 follow. The hook's deadline is the only clock, every step is planned against it, and the command
line the hook runs is what the tests and the replica run.

## 1. Module boundaries

### 1.1 The shape

```
exports on the stick ──ReadView──▶ View ──Diff──▶ Plan ──Apply──▶ m.db (live, in a window only)
                                     ▲               │
                 snapshot (stick) ───┘               └──▶ snapshot (stick), written after each commit
ANLZ + audio headers ──FillInBackground──▶ stage (/data) ──applied by the next window──▶ m.db + snapshot
what one window cannot do ──the job──▶ next library (.rb-next.db beside m.db) ──one window: two renames──▶ m.db + snapshot
m.db rows the deck changed ──WriteBack──▶ ANLZ + every library holding the track; then NoteWriteBack ──▶ snapshot
```

Four stores, each with one writer:

| Store | Where | Writer | Readers |
|---|---|---|---|
| Snapshot `.rb-bridge-snapshot` | on the stick, beside `m.db` | the mount path (and `NoteWriteBack`, under the volume lock) | mount path, fill job, write-back, shadow display, dbhealer, MP3 grid fix, the hook (existence only) |
| Stage `<library uuid>/` | on the deck, `/data/enginex/rb-stage/` | the fill job | the mount path |
| Next library `.rb-next.db` and its snapshot `.rb-bridge-snapshot.swap` | on the stick, beside `m.db`; while it is built its snapshot, the snapshot of the `m.db` it was made from and what it took of `m.db` are on the deck, `<stage>/<uuid>/next/` | the fill job | the mount path, which renames both into place |
| Write-back's record of each row (`Track.Edit`, `Track.Synced`) | in the snapshot | write-back (`NoteWriteBack`) and the window (a row only the bridge wrote) | write-back |

Rules that hold everywhere:

1. The live `m.db` is written only inside a window: in place, in short transactions, or replaced whole by one
   rename. A library the stick does not have yet (no `m.db`, or Engine's empty skeleton) is built aside and renamed
   into place. A change one window cannot write is made in a next library (rule 7), which one window renames into
   place. (Until 2026-11-25 the rule was "never copied, replaced by a copy or merged with a copy". What it protected
   is kept by rule 7: every row is copied once, rows Engine writes afterwards are taken again before the rename, and
   nothing of Engine's is dropped.)
2. Everything the bridge writes to `m.db` is idempotent and keyed by path. After each commit the snapshot is saved. A
   cut between the two repeats the same writes at the next window.
3. The background job never writes `m.db`; it reads it, in short statements. It writes the stage in `/data`, `.rgb`
   files for tracks it has finished (they are files Engine reads on demand), and the next library with its swap
   snapshot, which are new files beside `m.db` and the snapshot. It writes the snapshot in one case: a window left an
   export to it that changes no row and no list, and the job gives the snapshot that export's stamp, as the window
   would have, under the volume lock.
4. A window does work in this order: rows, lists, staged performance data, artwork. A window of the hook ends at
   least 3 s before the kill, whatever the library's size: before every step that grows with the library it asks
   `work.cost()` (`pkg/bridge/mount.go`: deck-speed costs measured on the replica) whether the step ends in time, and
   leaves what does not to the job. An insert applies no staged data: the DJ gets the stick.
5. A row the deck has changed is never overwritten. "Changed" means the row no longer holds what the bridge last
   wrote (`Track.Written`), decided per row when staged data is applied, with the field rule of #328.
6. Identity is the key. A source ID is used only together with its library (`SourceRef`), and only to follow a file
   that moved.
7. The next library. What does not fit one window (the rows and lists of a big export, the staged data of
   thousands of tracks) is not written into `m.db` a window at a time. The job copies every row of `m.db` into
   `Database2/.rb-next.db` (`enginedb.MirrorLibrary`: short read-only statements on the live file, paced like the
   job's other reads; all `Track` rows first, so that they lie together in the file), writes the rows, lists and
   staged data there with the window's own code and no deadline, and saves its snapshot as
   `.rb-bridge-snapshot.swap`. One window renames both into place and does nothing else: its work does not grow with
   the library, and neither does the number of remounts (KDATA, 5,962 tracks: 2 instead of 36).
   - Rows Engine wrote to `m.db` after the copy are taken again before the rename (`carryOver`): by the remount
     helper right before it triggers the remount, paced like the job and without a limit (`TakeDeckRows`), and by
     that window for what Engine wrote after that. They are found by
     a fingerprint of every `Track` row (every column; a blob by its length; Engine's triggers move
     `Track.lastEditTime` with every write of a track's performance data) and of every other table. A row taken
     again loses what the bridge wrote on it in the next library; its track is pending and the job's next round
     publishes it under rule 5. Tracks, lists, prepare-list entries and play marks Engine made are in the library
     after the swap as they were (#252); Engine's history is a file of its own, which names tracks by row ID and
     library UUID, and both are kept.
   - Row IDs are kept. A row the bridge adds to the next library gets an ID 65,536 above the highest Engine had
     issued, so a row Engine adds to the live file meanwhile never has the same ID. When Engine has used up that gap,
     or both added the same file, the next library is made again.
   - The stick needs room for a second copy of the library while the next one waits. Without it the job publishes
     in place, a window at a time, as before.
   - Small changes are still written in place: a new list, 30 new tracks, staged data one window can apply. The
     job decides with the same cost model.

### 1.2 Packages and owners

| Package / files | Job | Owner |
|---|---|---|
| `pkg/rbstate` (new, stdlib only) | Formats of the snapshot and the stage, the volume lock, the stamp | this lane; frozen below |
| `pkg/bridge/v2/source.go`, `view.go` | Find the export files, read them, build the view (join by path, three folders, ghost lists, Pro DJ Link staged libraries) | this lane |
| `pkg/bridge/v2/plan.go`, `apply.go`, `mount.go`, `adopt.go` | Diff, apply inside a window, the mount entry, taking over a library of an older build | this lane |
| `pkg/enginedb/library.go`, `build.go` (new) | The only code that writes `m.db`: row, list, performance and artwork writes; the bulk build of a new library | this lane |
| `pkg/bridge/v2/fill.go`, `priority.go`, `pace.go`; `pkg/enginedb/trackbuild.go` (new) | The background job: ANLZ and audio headers into staged records, priority, pacing, `.rgb` | fill lane |
| `pkg/bridge/v2/writeback.go`, `writeback_files.go`, `cuewatch*.go`, `fat*.go` | Deck cues back to the stick, the watcher, the eject pass, pull safety | write-back lane |
| `tools/ui-mod`, `pkg/webui/ui_settings.go`, `i18n.tsv`, manuals | The setting `rb_library_view` in the deck's Settings and in the console | settings lane |
| `internal/cli/rbbridge*.go`, `tools/edisksd-hook`, `pkg/webui`, `pkg/dbhealer` callers | Integration, the switch from `pkg/bridge` to v2, deletion of the old code | this lane |

`pkg/bridge/v2` is package `bridge`. It lives in its own directory until the switch so that the old package and its
tests keep building; at the switch its files move to `pkg/bridge` and the old files are deleted. `pkg/rekordbox` is
used through its present API and is not changed.

### 1.3 `pkg/rbstate`: the frozen interface

The package is on the branch (`pkg/rbstate`, with its tests); where this text and the code differ, the code is
right. It imports nothing else of EngineX.

```go
package rbstate

const (
	SnapshotName = ".rb-bridge-snapshot" // in Engine Library/Database2
	// Rules is part of every stamp and track signature. Raising it stages every track again.
	// It replaces stamp versions v2-v4, performanceEncoding and offsetRule.
	Rules = 5
)

type LibraryKind uint8

const (
	DeviceLibrary LibraryKind = 1 // export.pdb
	OneLibrary    LibraryKind = 2 // exportLibrary.db
	LinkLibrary   LibraryKind = 3 // a CDJ library staged over Pro DJ Link (#59)
)

// FileSig is a file as a stat sees it. Rel is volume-relative, as spelled on the volume.
type FileSig struct {
	Rel     string
	Size    int64
	MTimeNS int64
}

// SourceRef is a track's row in one library.
type SourceRef struct {
	Kind       LibraryKind
	Source     string // "" for the stick's own libraries; the staged library's name for LinkLibrary
	ID         uint32
	RecordHash string // rekordbox.Track.RecordHash
}

type Pending struct {
	Rows   int  // tracks of the view without a row yet
	Lists  bool // the lists are not the view's yet
	Perf   int  // rows whose performance data is not applied for their current signature
	Art    int  // rows whose cover is not applied
	Verify bool // the export changed: the job has not yet compared every track's files with Built
}

func (p Pending) None() bool

type Header struct {
	Format      int // 1
	Rules       int
	LibraryUUID string    // Information.uuid of the m.db this snapshot describes
	Stamp       string    // Stamp() of what the rows and lists were made from
	Layout      string    // "folders", "merged", "device", "onelibrary"
	Sources     []FileSig // the export files behind Stamp
	Tracks      int
	Lists       int
	Pending     Pending
	Applied     int64 // unix seconds of the last commit
	Commit      int64 // the library's commit ID this snapshot describes (see "Commit and snapshot" below)
}

type PerfState uint8

const (
	PerfNone     PerfState = 0 // the row has what the export alone gives (and what Engine added)
	PerfApplied  PerfState = 1 // grid, cues, loops and waveforms written from Built
	PerfDegraded PerfState = 2 // sources unreadable for Built; tried again when they change (#53)
	PerfDeck     PerfState = 3 // the deck changed the row; it stays the deck's until the export changes it
	PerfMissing  PerfState = 4 // the volume has no file for the track: it has no row until the file is back
)

// TrackSig is everything a track's staged data was built from.
type TrackSig struct {
	Rules   int
	Row     string    // hash of the export fields (ViewTrack.Hash)
	Audio   FileSig
	ANLZ    []FileSig // .DAT, .EXT, .2EX that exist
	Artwork FileSig   // zero when the export names none
}

func (s TrackSig) Equal(o TrackSig) bool

type Track struct {
	Key      string      // identity
	EngineID int64       // Track.id; 0 while the row is not written
	Rel      string      // the row's path without "../"; the export's spelling until Resolved
	Resolved bool        // Rel was found on the volume under this spelling
	Refs     []SourceRef // every library that holds the track
	Row      string      // ViewTrack.Hash the row was written from
	Rating   int         // the rating the bridge last wrote; another one in the row is the deck's
	Perf     PerfState
	Built    TrackSig // of the performance data in the row
	Written  []byte   // enginedb.WrittenDigest of grid, cues, loops as the bridge last wrote them
	Art      string   // hex SHA-1 of the applied cover, "" for none
	Detail   []string // ANLZ files that serve the detail waveform (was BridgeAnalysis RBA2)
	Native   bool     // an Engine-made row at this path: lists refer to it, the bridge never writes it (#252)
}

type List struct {
	Key      string // view folder, folder path and name joined by "\x00"; "#n" added for same-name siblings
	EngineID int64  // Playlist.id
	Hash     string // of title, parent key, position, folder flag and the entry keys in order
}

type Snapshot struct {
	Header
	Tracks map[string]*Track // by Key
	Lists  map[string]*List  // by Key
}

// FoldPath is the key of a volume-relative path: slashes normalised, NFC, case-folded.
func FoldPath(rel string) string

// Stamp names an import's inputs: Rules, the layout, each export file's Rel, Size and MTimeNS, and the stamp of
// the staged CDJ libraries. Sizes and times only name the files; they never choose between libraries.
func Stamp(layout string, sources []FileSig, staged string) string

// ReadHeader reads the first line only. os.ErrNotExist: no snapshot, or one cut short.
func ReadHeader(dbDir string) (Header, error)
func Load(dbDir string) (*Snapshot, error)

// Save replaces the file: temp file, fsync, the pull-safe rename of #321, directory fsync. Callers hold LockVolume.
func (s *Snapshot) Save(dbDir string) error

// TrackAt is the track of an Engine path ("../Contents/…").
func (s *Snapshot) TrackAt(enginePath string) (*Track, bool)

// EngineIDs is the set of Track.id values the bridge owns (the readers of 1.8 use it).
func (s *Snapshot) EngineIDs() map[int64]bool

// ReplaceFile and RemoveFile are the pull-safe file writes of #321 for anything the bridge puts on the stick.
// UnsafeToFree is set by pkg/bridge to its FAT chain check; a file it names is left alone (ErrUnownedClusters).
func ReplaceFile(dst string, data []byte) error
func RemoveFile(path string) error
func CleanTemps(dir string) int
var UnsafeToFree func(path string) bool

// LockVolume is the one lock for everything that writes the stick's library or its export files: the mount path,
// write-back, and NoteWriteBack. It is a flock in /run keyed by the volume's device, never a file on the stick.
// wait 0 fails at once with ErrLocked.
func LockVolume(mount string, wait time.Duration) (unlock func(), err error)
```

File format of the snapshot: a header line, a binary body, an end line.

```
RBS2 {header as JSON}       one line: all an unchanged insert reads
<body>                      the lists and the tracks, by key, as encoding/gob writes them
E <records> <crc32 of all bytes above, hex>
```

The body is gob and not JSON lines (`RBS1`, 2026-11-24 morning): 2,390 tracks as JSON cost 0.4 s of a deck's CPU
for one read and one write. A file without its `E` line is no snapshot. A reader that is not Go (the UI hook) reads
the header line only.

Commit and snapshot (changed on 2026-11-24 after part 1 was first pushed: `Header.DB` and `DBIdentity` are gone).
Every transaction of the bridge on `m.db` adds one to `Information.lastRekordBoxLibraryImportReadCounter`, Engine's
own counter of rekordbox imports, which earlier builds also moved at each import. That value is the library's commit
ID. A window writes the snapshot its transaction leads to as `.rb-bridge-snapshot.next`, with `Header.Commit` set to
the ID the commit will leave, then commits, then renames the file over the snapshot. A window that finds a `.next`
file reads the library's commit ID: equal means the commit happened and the file is the snapshot, anything else
means it did not and the file is dropped. Readers other than the mount path ignore the `.next` file.

```go
func (s *Snapshot) SaveNext(dbDir string) error
func Promote(dbDir string) error
func LoadNext(dbDir string) (*Snapshot, error)
func DropNext(dbDir string) error
func RenameFile(src, dst string) error  // pull-safe rename of a file already written and synced
func InstallFile(src, dst string) error // pull-safe copy from another file system
```

A file without a matching `E` line does not exist as far as `ReadHeader` and `Load` are concerned. The unchanged
insert reads line 1 only. Sizes: about 200 bytes a track, so 0.5 MB for 2,416 tracks and 1.2 MB for 6,000.

The stage:

```go
// StageRoot is /data/enginex/rb-stage; ENGINEX_RB_STAGE_DIR overrides it, and tests must set it.
func OpenStage(libraryUUID string) (*Stage, error)

type Facts struct { // from the audio header, as enginedb reads it today
	Duration           float64
	SampleRate         int
	SampleCount        uint64
	BitDepth, Bitrate  int
	StartOffsetSamples int
	BPM                float64 // the grid's first tempo when the export has none
	FileBytes          int64
}

type Staged struct {
	Key      string
	EngineID int64    // "id": the track's row in the stick's library (Track.id) when it was staged; left out without one
	Sig      TrackSig // a window applies it only while Sig.Row and Sig.Rules are still the view's
	Rel      string   // the audio file's spelling on the volume
	Missing  bool     // the volume has no file for this track
	Facts    Facts
	Perf     Perf     // Engine's five blobs, ActiveLoops and Complete (the fields of enginedb.Performance); zero when Degraded != ""
	RGB      []byte   // body of OverviewData/<uuid>/<id>.rgb, nil for none
	Art      []byte   // only a cover taken from the audio file itself; export covers are read from Sig.Artwork
	Detail   []string
	Degraded string // why there is no performance data (#53), "" otherwise
}

func (s *Stage) Put(t *Staged) error                   // the track's files, each whole and synced, then one index line
func (s *Stage) Get(key string) (*Staged, error)       // os.ErrNotExist
func (s *Stage) Index() (map[string]TrackSig, error)   // what is staged, without reading the blobs
func (s *Stage) Entries() (map[string]*Staged, string, error) // every index line without blobs, and Verified
func (s *Stage) Read(entry *Staged) (*Staged, error)   // an entry with its blobs
func (s *Stage) Sync() error                           // the index to the disk; call it every few Puts
func (s *Stage) Close() error
func (s *Stage) MarkVerified(stamp string) error       // every track of this stamp was compared with Built
func (s *Stage) Verified() string
func (s *Stage) Drop(keys ...string) error             // after a window applied them
func (s *Stage) Remove() error
func (s *Stage) Filled() map[string]Filled             // the UI hook's record of the rows it filled, the last line of each key
```

On disk: `<root>/<uuid>/index.jsonl` (append only, last line of a key wins, a torn last line is ignored) and one
`<sha1 of key>.bin` per track. The `.bin` file is the `RBXA1` layout the UI hook already parses
(`tools/ui-mod/rekordbox_analysis.h`): the magic, then `trackData`, `overviewWaveFormData`, `beatData`, `quickCues`,
`loops`, each a little-endian uint32 length and its bytes, then one byte for `Complete`. The other fields are in the
index line; `RGB` and `Art` are files beside it (`.rgb`, `.jpg`). `Put` refuses when `/data` has less than 256 MiB
free. A stage not touched for 30 days is removed by `OpenStage` of another library.

Two things in the stage are for the deck's UI hook (`tools/ui-mod/rekordbox_stage.h`), which writes a staged
track's data into its row when a deck loads the track before a window has published it:

- An index line has `"id"`, the row's `Track.id` in the stick's library when the job staged the track. A track
  whose row a window has not made yet has no `"id"`. The ID is the row's at that moment: a reader checks the row's
  path against `"k"`.
- The hook appends one line per fill to `<root>/<uuid>/filled.jsonl`: `{"id":<Track.id>,"k":"<key>","wr":"<the
  staged line's wr>","v":<rules>,"w":"<row hash>","at":<unix time>}`. The bridge only reads it (`Stage.Filled`). A
  row with a line there whose `id` is the row's, and for which the snapshot has no `Written`, is applied as a row the
  bridge wrote with that digest (`PerfPrev.Written`): staged data replaces it, it is recorded `PerfApplied`, and only
  a difference from that digest is a deck edit. The file goes with the stage directory.

Engine writes its own figure into the last field of `quickCues` (the main cue as imported) every time a deck loads
a track. `ApplyPerformance` compares a row with that field set back to the main cue, as the bridge writes it
(`asWritten`), so a load alone is never a deck edit; a main cue the DJ adjusted has its flag set and is one.

### 1.4 This lane: sources, view, plan, apply, mount

```go
package bridge // pkg/bridge/v2

// FindSources stats the export files of a mount. No file is opened. An export.pdb the deck generated itself
// (prolink.GeneratedExport) is not a source.
func FindSources(r *compat.Resolver, mount string) ([]rbstate.FileSig, error)

// SourceLibrary is one library as pkg/rekordbox read it.
type SourceLibrary struct {
	Kind   rbstate.LibraryKind
	Source string
	File   rbstate.FileSig
	Lib    *rekordbox.Library
}

// ReadSources reads each export once. keys is the SQLCipher key cache; nil uses the process cache.
// An unreadable library is returned in failed and left out; with none readable the error is returned.
func ReadSources(ctx context.Context, r *compat.Resolver, mount string, files []rbstate.FileSig, keys *rekordbox.KeyCache) (libs []SourceLibrary, failed []error, err error)

// Layout is the setting rb_library_view: "folders" (default), "merged", "device", "onelibrary".
// A stick with one library ignores it and shows that library's lists at the root.
func ReadLayout(settingsPath string) string

type ViewTrack struct {
	Key   string
	Track rekordbox.Track     // the fields the row is written from; Path as the export wrote it
	Refs  []rbstate.SourceRef
	Hash  string
}

type ViewList struct {
	Key, Parent string // rbstate.List.Key of this list and of its folder ("" at the root)
	Name        string
	Folder      bool
	Tracks      []string // keys, in order, each at most once
	Hash        string
}

type View struct {
	Stamp   string
	Layout  string
	Sources []rbstate.FileSig
	Tracks  []ViewTrack // every track once
	Lists   []ViewList  // parents before children, siblings in display order
}

// BuildView joins the libraries. Tracks are joined by key. With the layout "folders" and two libraries the root
// holds three folders: the merged view, Device Library, OneLibrary, each with that view's whole tree. The merged
// view joins lists by folder path and name; a list in both takes the longer order and appends what only the
// other has. OneLibrary's ghost empty root lists are left out of every folder, with the PDB as evidence.
func BuildView(libs []SourceLibrary, layout, lang string) *View

// Plan is what a window has to do to make m.db the view. Its sizes are what the budget is planned with.
type Plan struct {
	AddTracks    []int    // indexes into View.Tracks
	UpdateTracks []int    // row hash changed
	MoveTracks   [][2]string // old key, new key: the same SourceRef at a path the old view no longer has
	RemoveTracks []string // keys the view no longer has
	PutLists     []int    // new, or Hash changed
	RemoveLists  []string
	Bulk         bool     // the stick has no library to keep: build aside and rename
}

func Diff(v *View, s *rbstate.Snapshot) *Plan

type MountOptions struct {
	Deadline        time.Time    // the hook's kill; zero: none (a manual run)
	Full            bool         // --full: stage and apply everything now, no budget
	SmartMatch      uint32       // --smart-match
	Settings        string       // settings.conf path
	StartBackground func() error // starts the job of this mount; called before any step that can be long
	Progress        func(done, total int)
}

type MountResult struct {
	Bypassed   bool // nothing to do, or nothing the bridge may write
	Background bool // the job was started
	Added, Updated, Removed int
	ListsChanged bool
	Applied int // staged tracks written in this window
	Pending rbstate.Pending
}

// ConvertAtMount is one window. It logs one line per step with its duration:
// "rb-bridge: mount <step> <ms> ms (<counts>)" for stat, read, snapshot, diff, rows, lists, staged, art, save.
func ConvertAtMount(ctx context.Context, mount string, o MountOptions) (MountResult, error)

// ImportState is what write-back and the status endpoint ask: is this stick's library the current export's?
type State struct {
	Header  rbstate.Header
	Current string // Stamp of the export files now on the stick
}
func (s State) Matches() bool // Header.Stamp == Current
func ImportState(mount string) (State, error)

// NoteWriteBack records that write-back rewrote export files and ANLZ files: it refreshes Header.Sources,
// Header.Stamp and the Built.ANLZ of the written tracks, so the next insert does not take them for a re-export.
// The caller holds LockVolume.
func NoteWriteBack(mount string, written map[string][]rbstate.FileSig) error

// HasPopulatedLibrary, LibraryHasFile and ReadLibrary keep their present signatures.
```

`pkg/enginedb/library.go` (only this lane calls it):

```go
// OpenLibrary opens dir/m.db for a window. ErrLibraryInUse when another process has it open.
func OpenLibrary(ctx context.Context, dir string) (*Library, error)
func (l *Library) UUID() string
func (l *Library) Identity() rbstate.DBIdentity
func (l *Library) Begin(ctx context.Context) (*Batch, error)

func (b *Batch) InsertTrack(ctx context.Context, row TrackRow) (id int64, err error)
func (b *Batch) UpdateTrack(ctx context.Context, id int64, row TrackRow) error // keeps what the deck changed
func (b *Batch) MoveTrack(ctx context.Context, id int64, enginePath string) error
func (b *Batch) DeleteTrack(ctx context.Context, id int64) error
func (b *Batch) NativeTrack(ctx context.Context, enginePath string) (id int64, ok bool, err error)
func (b *Batch) PutList(ctx context.Context, id int64, row ListRow, entries []int64) (int64, error)
func (b *Batch) DeleteList(ctx context.Context, id int64) error
// ApplyPerformance writes staged data unless the deck changed the row since `written`; it returns the digest of
// what the row holds afterwards and what it did (written, kept the deck's, merged by the #328 rule).
func (b *Batch) ApplyPerformance(ctx context.Context, id int64, p Performance, f rbstate.Facts, written []byte) (digest []byte, outcome Outcome, err error)
func (b *Batch) PutArtwork(ctx context.Context, id int64, jpeg []byte) (sha1hex string, err error)
func (b *Batch) Commit() (rbstate.DBIdentity, error)

// BuildLibrary writes a new library file from rows and lists (journal in memory, indexes and the two
// triggers recreated after the load) and returns the IDs it gave.
func BuildLibrary(ctx context.Context, dst string, tracks []TrackRow, lists []ListRow) (trackIDs, listIDs []int64, uuid string, err error)
```

### 1.5 Fill lane: the background job

Contract:

- Input: the mount, the snapshot (read only), the exports (through `ReadSources` and `BuildView`), the priority
  queue and the deck-activity file of today's `priority.go` (kept as they are).
- Output: `Staged` records in the stage; `.rgb` files for finished tracks in the live `OverviewData/<uuid>/` through
  `enginedb.WriteRGBOverviewAtomic` (never over a valid file); progress through the callback.
- It never opens `m.db` for writing, never writes the snapshot, and writes nothing else on the stick.

```go
type FillOptions struct {
	Progress func(done, total int)
	Stage    *rbstate.Stage // nil: OpenStage(snapshot.LibraryUUID)
}

type FillResult struct {
	Staged, Degraded, Missing int
	Left    int  // tracks still to stage (ctx ended)
	Publish bool // the stage holds something a window would apply: the caller asks for the remount
}

// FillInBackground is `rb-bridge --import-path`. Order of work: tracks in the priority queue, then every track
// whose signature differs from Track.Built or whose Perf is PerfNone, sorted by path. For each: stat the audio
// file and the ANLZ files (this is also what finds a file that moved or is gone), skip when the stage or the
// snapshot already has that signature, else build and Put. When all tracks of the stamp are compared it calls
// MarkVerified. It yields to a playing deck as today (64 KiB reads, 20 ms gaps, small batches).
func FillInBackground(ctx context.Context, mount string, o FillOptions) (FillResult, error)

// In pkg/enginedb/trackbuild.go: one track from the volume, with today's rules for offsets (rule 4), the AAC
// priming, the 499.5 BPM clamp, sample-rate disagreement, PWV section choice and degraded imports (#53).
func BuildTrack(res *compat.Resolver, t rekordbox.Track, read ReadOptions) (*rbstate.Staged, error)
```

The mount path applies a staged record when `Sig.Rules == rbstate.Rules`, `Sig.Row` is the view's hash for the key and
the row exists. It then sets `Track.Built = Sig`, `Perf`, `Written`, `Rel`, `Resolved`, `Detail`, and drops the record.
A record with `Missing` removes the row unless the deck changed it, under the rule of `ErrFilesUnreadable` (many
"missing" tracks with look-alike files on the volume means the names cannot be read: nothing is removed).

The job is started by the mount path through `MountOptions.StartBackground` (today's `startImport`: the systemd
unit, the slice, idle CPU and I/O class). It starts before the exports are read. It waits for the visible mount and
for the volume lock, so it never runs beside a window. `internal/cli` keeps the remount helper and its conditions.

### 1.6 Write-back lane

Contract:

- Gate: `st, _ := ImportState(mount)`; write only when `st.Matches() && st.Header.Pending.None()`. Anything else is
  no write and one log line. The recovery mark never bypasses the gate.
- Identity: `snapshot.TrackAt(Track.path)` gives the key and the `Refs`. A cue is written to the track's ANLZ files
  and to every library in `Refs` (`OneLibrary`: the cue table and the metadata; `DeviceLibrary`: the metadata the PDB
  holds). `pdbImportKey` is not read. A row without a snapshot track is not the bridge's and is skipped.
- Baseline: a row whose `enginedb.WrittenDigest` equals `Track.Written` holds what the import wrote and is not read
  further. What the stick's files hold of a row is in the snapshot: `Track.Edit` (the row's `lastEditTime` when it
  was noted) and `Track.Synced` (the digest of its cues, loops and tags); a row whose `lastEditTime` is the noted one
  is not read at all. There is no separate state file (`8cfc4924`).
- After a pass that wrote files it calls `NoteWriteBack` under the same volume lock.
- Kept as they are: the quiet watcher in stream-sync, the eject pass through the local endpoint, the key and export
  caches, first-original backups in `.enginex-backup`, the staged-file marks and the put-back at the next insert, the
  FAT chain check, the fsync order of #321 R4b.

```go
func WriteBackCues(ctx context.Context, mount string, o WriteBackOptions) (WriteBackResult, error) // signature kept
func NewCueWatcher(mediaRoot string) *CueWatcher                                                    // kept
func CleanWriteBackLeftovers(mount string) int                                                      // called by the mount path
```

### 1.7 Settings lane

- Key `rb_library_view` in `/data/enginex/ui/settings.conf`: `folders` (default when absent), `merged`, `device`,
  `onelibrary`. `ReadLayout` is the only reader. The value is part of the stamp, so a change applies at the next
  insert as a list-only diff.
- The deck's Settings entry and the console entry, the three languages in `i18n.tsv` and the manuals, a checklist
  row. No other QML change: the three folders are playlist folders in `m.db`.
- Folder titles come from `BuildView`'s `lang` argument (the deck language in `settings.conf`, key `lang`). The two
  library folders use rekordbox's names, the merged folder a translated one. The lane supplies the strings.

### 1.8 Other readers of the old state database

| Reader | Today | v2 |
|---|---|---|
| `pkg/webui/shadow_display.go:914` | `BridgeAnalysis` row by track id and path | `rbstate` snapshot: `Track.Detail` for the key of the path, checked against `EngineID` |
| `pkg/dbhealer/db.go:578-596` | `SELECT trackId FROM BridgeState` | the snapshot's `EngineID` set |
| `pkg/enginedb/mp3grid.go:159-169` | the same | the same |
| `tools/edisksd-hook/edisksd_hook.c:2526` | eject write-back runs when `.rb-bridge-state.db` exists | when `.rb-bridge-snapshot` exists (the prebuilt `.so` is rebuilt with it) |

### 1.9 What each old file becomes

| Old file (lines) | Becomes |
|---|---|
| `pkg/bridge/bridge.go` (1,341) | `source.go`, `view.go`, `mount.go`; export choice by time, the in-process export cache, `Reconcile` at the mount, the read budget, overview backfill, artwork migration and bridge-analysis shrink are deleted |
| `pkg/bridge/update.go` (1,240) | deleted: quick update, list-first stage, snapshot copy, `VACUUM INTO`, passes, hand to mounts, reread mark. `plan.go` + `apply.go` replace it |
| `pkg/bridge/firstimport.go` (566) | deleted: stage on the stick, `BASE`, `KIND`, `RESUME`, publish directory, `m.db.before-import`. `BuildLibrary` plus one rename replace the fast publish |
| `pkg/bridge/publish.go` (257) | deleted except the pull-safe file replace, which moves to `rbstate` |
| `pkg/bridge/priority.go` (469), `priority_consumer.go` (313) | `priority.go` kept; the consumer becomes part of `fill.go` |
| `pkg/bridge/cue_writeback.go` (1,927), `cue_watch*.go` (579), `fatchain.go`, `leftover_*.go` | write-back lane; identity and gate change as in 1.6 |
| `pkg/bridge/prolink_merge.go` (156) | kept, feeding `BuildView` |
| `pkg/enginedb/sync.go` (1,575), `prepare.go` (776) | `library.go` (row and list writes), `trackbuild.go` (one track from the volume) |
| `pkg/enginedb/merge.go` (381), `bulk.go` (211), `plan.go` (65), `stmtcache.go` (71) | deleted; `build.go` keeps the bulk load |
| `pkg/enginedb/bridge_analysis.go` (417), `bridge_compact.go` (193), `bridge_shrink.go` (439), `overview_progress.go` (122) | deleted: `Track.Detail` replaces the reference rows; copies of waveforms are no longer kept |
| `pkg/enginedb/fastedits.go` (645), `realign.go` (414) | the row rule inside `ApplyPerformance`; the fast-base file and the live-edit carry-over are deleted |
| `pkg/enginedb/rgb_overview.go` (969) | encode, validate and atomic write kept; stage backfill, publish and the verified cache deleted |
| `pkg/enginedb/artwork.go` (561) | read, hash, `Artwork/` file and embedded covers kept; the migration deleted |
| `pkg/enginedb/performance.go`, `display.go`, `pending.go`, `relocate.go`, `mp3grid.go`, `playlists.go`, `smart.go`, `inuse.go` | kept |
| `internal/cli/rbbridge_stage.go` (299) | kept: it is the staging of Pioneer players' libraries over Pro DJ Link (#59, `stager`), which `source.go` reads as a library, not the old import's stage; `rbbridge_import.go`, `rbbridge_remount.go`, `rbbridge_status.go` kept |

Marks and files that disappear from the stick: `.rb-bridge-state.db` (and its journal and `.compact`),
`.rb-bridge-stamp`, `.rb-bridge-pending`, `.rb-bridge-reread`, `.rb-bridge-import/` with `BASE`, `BASE.counts`,
`BASE.times`, `KIND`, `RESUME`, `QUICK`, `fast-base.db`, `.bulk`, `.rb-bridge-publish/READY`, `m.db.before-import`,
`.rb-bridge-overview`, `.rb-bridge-rgb-verified`, `.rb-bridge.lock`. What remains: `.rb-bridge-snapshot` (with
`.next` around a commit) and write-back's mark of a pass in progress (`.rb-bridge-writeback`).

A library of an older build (it has `.rb-bridge-state.db` and no snapshot) is taken over once, in a window, by
`adopt.go`: paths, Engine IDs, list IDs and written digests are read from the old state into a snapshot, every track
is marked `PerfNone` with `Pending.Verify`, and the job stages it again under rule 5 of 1.1, which is what keeps deck
edits. The old state file is removed after the snapshot is saved.

### 1.10 Order of work

1. `pkg/rbstate` (this lane, first, so the others can compile against it).
2. In parallel: sources + view + plan + apply + mount (this lane); fill; write-back; settings.
3. Integration on this branch: the CLI, the hook's one file name and its rebuilt `.so`, the three other readers, the
   old tests ported per area by the lane that owns the area.
4. Deletion of `pkg/bridge` old files and the old `enginedb` files, with the before/after table.

### 1.11 Known limits of this design (decisions for the PM)

1. **A track loaded right after the first insert of a never-seen export.** Its row has no grid and no waveform yet,
   and no process may write `m.db` while Engine has it. Engine analyses the track itself, as it does today; the
   earlier "0.47 s" was the time until the old stage held the track, which Engine does not read. Staged data reaches
   Engine at the next window. The only way to serve such a load in about a second is to fill the row on Engine's own
   connection when Engine reads it. The UI hook already does that for Pro DJ Link tracks (`sc_rbx_fill_track`,
   `tools/ui-mod/rekordbox_analysis.h:557`) and skips a drive's database on purpose (line 565). The stage's `.bin`
   file has that hook's format so that a hook lane could reuse it. That is a change inside Engine (32-bit builds
   only today), outside this brief: the PM decides.
2. **Volume through a window.** Performance data is about 13 KB a track (31 MB for 2,416 tracks, 78 MB for 5,984),
   and covers are larger. A window writes them to a USB 2.0 stick at about 11 MB/s and stops at its budget, so a first
   import reaches Engine over more than one window: rows and lists at the insert, performance data at the job's
   remount, covers after that. The numbers per window at deck speed are measured in part 3 before the budgets are
   fixed.
3. **Waveform copies.** Older builds kept a copy of the detail waveform for a track whose ANLZ later became
   unreadable (#253 review). v2 keeps references only; such a track shows Engine's own overview on the shadow deck.
4. **An older build after v2.** A deck running 5.3.0.5-r1 does not know the snapshot. What it does with a stick last
   written by v2 is tested with the `147e37fb` step binary before the deck test and reported.

5. **More than about 32,700 tracks on a FAT32 stick have no waveform in the lists.** Engine keeps one `.rgb` file
   a track in `OverviewData/<uuid>/`. A FAT32 folder holds 65,536 entries and each of these files takes two (found
   with 50,000 synthetic tracks, 2026-11-25). The layout is Engine's. The job stops writing these files at the
   first refusal and says so once; rows, lists, cues, grids and the deck's own waveforms are not affected. exFAT
   has no such limit.
6. **At the largest sizes a next library needs the stick's library to be quiet between the helper's pass and the
   remount.** Finding the rows Engine wrote since the job's last pass reads every row of every table (0.15 ms a
   row at deck speed). The remount helper does that right before it triggers the remount, when no deck holds a
   track of the stick, and looks at the decks again afterwards; the window then only renames. What Engine writes
   in the second or so between that pass and the remount is the window's to take: when that cannot end 3 s before
   the kill (above about 60,000 rows of all tables together) the window does not swap; the job takes the rows and
   asks for the remount again. Engine itself writes nothing in that time: in the sandbox the real Engine (JP07,
   5.3.0) wrote `m.db` once a session, when it opened the library (`Information.currentPlayedIndiciator`), not
   while idle for a minute and not when the stick was removed; it let go of the stick 1.4 s after the remove
   (report `rb-window-fable.md`, "Lane 6"). An Eject stops the helper's pass.
7. **A big re-export cannot be browsed before its switch.** Engine shows the one library it opened at the mount and
   never sees another process's writes to it. The old library is on show at once; the new export's lists and
   tracks appear with the one remount that swaps the next library in (report `rb-window-fable.md`, "Proposal").
8. **`/data` smaller than a library's stage.** At the stage's floor (256 MiB free) the job writes what is staged
   into the next library, empties the stage and goes on; one remount publishes all of it. Until that remount a
   track loaded on a deck is not in the stage any more, so the UI hook cannot fill it and Engine analyses it.
9. **A stick without room for a second copy of its library** (the library's size, what its stage holds, and
   64 MiB) gets no next library. Staged data and row updates are then written in place, the part that fits each
   window (one remount a part, as before the next library; by the cost model a remount's window updates up to
   about 4,000 rows of a 5,962-track library). A
   change of rows and lists that one window cannot write waits for an insert with the OneLibrary key cached; an
   export whose read alone does not fit a window (by the cost model a OneLibrary of about 20,000 tracks, a Device
   Library of about 45,000) is never taken on such a stick,
   and every insert then costs a stat and the job's refusal. The job's line says how much is free and how much
   is needed.
10. **A loaded track whose main cue a re-export moved is kept as the deck's.** Engine 5.3.0 writes its own figure
   into the main cue and the main cue as imported at every load of a track whose main cue the DJ has not adjusted
   (measured on JP07 and SC LIVE 4: 9920.0 twice became -1105.0 twice, the flag stayed 0). The bridge therefore
   compares such a row with those two fields read as the staged record has them (`asWritten`,
   `pkg/enginedb/library.go`): a load alone is no edit, whether a window or the UI hook wrote the row. `Written`
   is a digest of the bytes written, so when a later export moved the track's main cue (its earliest memory cue)
   the fields written before cannot be put back, and the loaded row counts as changed by the deck: its cues,
   loops and grid stay (rule 5 errs on that side) and write-back treats them as the deck's. Closing this needs a
   second digest without the two fields, in the snapshot and in the hook's `filled.jsonl` line. Also seen in that
   measurement: after a load the row no longer holds the main cue the bridge wrote from rekordbox, since Engine
   replaced it.

## 2. Requirements and the tests that cover them

Each table row is one requirement of the bridge as a DJ or a maintainer sees it: a behaviour of the deck, or an
invariant that must hold after a pull, a kill or a re-export. The `Tests` column names the test functions that assert
it today, each with its file under the repository root; **no test** means that no test asserts it (listed again in
2.9). A test is cited only when its assertions state the requirement. The tables say what a test asserts, not whether
it passes; the gate run decides that. Issue numbers (#n) are those of the old bridge's tests and the regression
checklist.

### 2.1 Mount: reading both libraries, the view, three folders, the `rb_library_view` setting, ghost lists

| Requirement | Tests |
|---|---|
| A stick with both libraries reads both and joins them by path: every track shows once, and the root holds three folders (merged, Device Library, OneLibrary), each with that view's whole tree (#327). | `TestViewOfTwoLibrariesHasThreeFoldersAndEveryTrackOnce` (`pkg/bridge/view_test.go`) |
| The setting `rb_library_view` (folders, merged, device, onelibrary) chooses what the root shows, a stick with one library ignores it, the merged folder is titled in the deck's language (Simplified and Traditional Chinese, Spanish, English otherwise), and a change is seen at the next insert because the layout is part of the stamp. | `TestViewLayouts`, `TestViewOfTwoLibrariesHasThreeFoldersAndEveryTrackOnce` (`pkg/bridge/view_test.go`); `TestStampNamesRulesLayoutAndFiles` (`pkg/rbstate/rbstate_test.go`) |
| The console and `settings.conf` keep the setting: default `folders` (not written to the file), a round trip through the console API, an invalid value reads as `folders`. | `TestRbLibraryViewSetting` (`pkg/webui/ui_settings_test.go`) |
| The bridge reads the key `rb_library_view` from `settings.conf` (`ReadLayout`), and the deck's settings menu (Profile, Library) offers the four choices and writes it. | **no test** |
| rekordbox 7's empty root lists that repeat a list with tracks and are absent from the Device Library are hidden in every folder (11-23 deck test). | `TestViewHidesOneLibraryGhostRootLists` (`pkg/bridge/view_test.go`) |
| In a list a file named twice and a repeated entry show once, an unknown track is dropped, and two lists with one name both exist (#252); a track is identified by its case-folded NFC path, so two records naming one file are one track, and a path that leaves the volume or holds a NUL byte is refused. | `TestViewTreeEdgeCases` (`pkg/bridge/view_test.go`); `TestFoldPath` (`pkg/rbstate/rbstate_test.go`) |
| A OneLibrary smart list shows as an ordinary list of the tracks rekordbox resolved; with a reference track, `[Smart Match]` is the last root list and is recomputed when the BPM or a list changes, an unknown reference is an error, and compatible keys follow the wheel. | `TestPort2IntelligentListCarriesMaterializedTracks`, `TestPort2SmartMatchRecomputesAndValidatesReference` (`pkg/bridge/port2_mount_misc_test.go`); `TestSmartMatchWheel` (`pkg/enginedb/performance_test.go`) |
| The Device Library (`export.pdb`) reads whole: unanalysed or out-of-range BPM and rate are 0, orphan list entries are dropped, titles starting with byte 0x03 read in full, damaged pages are refused (#182, #216). | `TestPDBAudioMetadataNormalization`, `TestPDBOrphanPlaylistEntries` (`pkg/rekordbox/regression_test.go`); `TestDeviceSQLStringsStartingWithByte03`, `TestPDBPagesAndInvalidData` (`pkg/rekordbox/rekordbox_test.go`) |
| The OneLibrary (`exportLibrary.db`, SQLCipher 4) reads without touching the stick: byte-exact decrypt, tamper and size bounds, soft-deleted rows skipped, the full rekordbox 7 schema with cues; ANLZ files merge into one analysis and ignore disabled slots, pads I and J and overflowing times (#252). | `TestSQLCipher4ByteExactAgainstCSQLCipher`, `TestSQLCipher4TamperAndBounds`, `TestOneLibraryFullSchemaCuesAndRecordHash` (`pkg/rekordbox/sqlcipher_test.go`); `TestOneLibrarySkipsSoftDeletedRekordboxRows` (`pkg/rekordbox/bh2_a4_test.go`); `TestAnalysisMergeAndBounds` (`pkg/rekordbox/rekordbox_test.go`); `TestAnalysisSkipsPadsIPAndDisabledCueSlots` (`pkg/rekordbox/order4_test.go`); `TestCueOverflow252` (`pkg/rekordbox/cue_overflow252_test.go`) |
| The SQLCipher key is derived once per salt and kept for later reads, and the keys are kept in the deck's data directory so a power-on does not derive them again. | `TestPort2ReadSourcesCachesOneLibraryKeysBySalt` (`pkg/bridge/port2_keycache_test.go`); `TestKeyCacheSharesDerivedKeyAcrossCacheInstances` (`pkg/rekordbox/sqlcipher_keys_test.go`); `TestExportKeysAreKeptInTheDecksDataDirectory` (`pkg/bridge/mount_test.go`) |
| The export is found under an upper-case or a hidden `.PIONEER` path; an `export.pdb` the deck generated itself for CDJ link is not a source. | `TestPort2UppercaseAndGeneratedExports` (`pkg/bridge/port2_mount_misc_test.go`); `TestPort1StagedAndLocalLibrariesRetainLocalRowsAndMergeHiddenExport` (`pkg/bridge/port1_mount_test.go`) |
| When one of two libraries cannot be read, it is left out and the other is shown, nothing is removed, the next insert reads again, and the unreadable one does not hold the import back; when none can be read the window returns the error, builds no library, and the job was asked for first (#53). | `TestPort1BrokenOneLibraryLeavesDeviceRowsAndRetries`, `TestPort1UnreadableOneLibraryStillCompletesDeviceImport` (`pkg/bridge/port1_mount_test.go`); `TestMountStartsTheJobBeforeReadingTheExports` (`pkg/bridge/mount_test.go`) |
| A Pro DJ Link library staged on the stick shows under `[Pro DJ Link: <name>]`; a stick with only staged libraries converts them, a repeated mount is Bypassed, a restaged export converts again, and the stick's own rows and Engine's own row stay (#59, #252). | `TestViewTreeEdgeCases` (`pkg/bridge/view_test.go`); `TestPort1StagedOnlyStickConvertsRepeatsAndRestages`, `TestPort1StagedAndLocalLibrariesRetainLocalRowsAndMergeHiddenExport` (`pkg/bridge/port1_mount_test.go`) |
| Staged libraries share one budget per drive, unusable copies are removed, long-unseen players give way only when room is needed, nothing is touched through a symlink, and a deck of ours is never copied (#211). | `TestStagedLibrariesShareOneBudgetOfTheDrive`, `TestStagingRemovesWhatNoImportCanUse`, `TestStagingDropsLongUnseenPlayersForRoom`, `TestStagingRemovesNothingThroughASymlink`, `TestStagingNeverCopiesADeckOfOurs` (`internal/cli/rbbridge_stage_test.go`) |
| The Pro DJ Link flags and the setting migration work, the retired `--watch` exits at once, the journal gets a device and not every packet (#51), and a track loaded over Link has the same performance data as the same track converted from USB. | `TestRBBridgeProlinkFlags`, `TestProlinkSettingOnMigration`, `TestRBBridgeRetiredWatchExitsAtOnce`, `TestRBBridgeLogsDevicesNotPackets` (`internal/cli/rbbridge_test.go`); `TestPort2RekordboxLinkAnalysisMatchesBridge` (`pkg/webui/port2_rekordbox_analysis_test.go`) |

### 2.2 Mount: work proportional to the change, IDs stable, the window's time rules

| Requirement | Tests |
|---|---|
| A window of the hook that cannot end 3 s before the kill leaves the work to the job and gives Engine the stick; the estimate is within 15 % below and 35 % above the replica's measured steps, and a OneLibrary of 20,000 or 50,000 tracks, new or re-exported, is left to the job without being read. | `TestMountLeavesWhatDoesNotFitToTheJob`, `TestWorkCostAgainstTheReplica` (`pkg/bridge/next_test.go`) |
| What the job publishes as a next library is, after the one window that swaps it in, the library the windows would have written in place (rows under the same IDs, lists, data, snapshot); the job never writes `m.db` and the window does no row work. A new next library has its `Track` rows together. | `TestNextLibrarySwappedInIsTheLibraryTheWindowsWrite` (`pkg/bridge/next_test.go`); `TestMirrorLibraryLaysTrackRowsTogether` (`pkg/enginedb/mirror_test.go`) |
| The upkeep at a window's end is stopped at its budget (0.5 s in the insert of an unchanged stick, 1 s elsewhere), and a window leaves a list waveform to the job that staged the track with its row. | `TestMountUpkeepIsStoppedAtItsBudget`, `TestMountLeavesTheListWaveformToTheJobThatStagedTheRow` (`pkg/bridge/mount_test.go`) |
| A window that cannot look for the deck's rows 3 s before the kill does not swap; the job takes them and the next window swaps. With nothing to publish the job makes no next library and asks for no remount. | `TestSwapWaitsForTheJobWhenTheDecksRowsCannotBeLookedForInTime`, `TestNextLibrarySwappedInIsTheLibraryTheWindowsWrite` (`pkg/bridge/next_test.go`) |
| A load alone is no deck edit: Engine's own figure in the main cue and the main cue as imported (both fields, flag clear, as the real Engine writes them; and the last field only) leaves a row the bridge's, whether a window or the UI hook wrote it; an adjusted main cue is the deck's. | `TestMountTakesRowsTheDeckHookFilledAsTheBridgesOwn`, `TestMountStagedDataAndDeckEdits` (`pkg/bridge/mount_test.go`) |
| A stick without room for a next library: row updates one window cannot write are written in place, the batches that fit each window; with room they are left to the job. | `TestRowUpdatesGoOnInPlaceOnAStickWithoutRoomForANextLibrary` (`pkg/bridge/mount_test.go`) |
| The remount helper takes the deck's rows right before the remount, so that a window without time for the look still swaps; it looks at the decks again after the pass; what it took is read on from by the window and by the job. | `TestRowsTakenBeforeTheRemountLetAWindowWithoutTimeSwap`, `TestRowsTakenBeforeTheRemountAreReadOnFrom` (`pkg/bridge/next_test.go`), `TestRemountTakesTheDecksRowsBeforeTheRemove` (`internal/cli/rbbridge_remount_test.go`) |
| A stage that fills `/data` is emptied into the next library and the job goes on: every track is staged and one remount publishes all of them. The job stops writing list waveforms when their folder takes no more files. | `TestFillGoesOnPastAFullStageThroughTheNextLibrary` (`pkg/bridge/next_test.go`); `TestFillStopsWritingListWaveformsWhenTheirFolderIsFull` (`pkg/bridge/fill_test.go`) |
| The first insert of a never-seen export builds rows and lists from the export alone (rows as Engine expects them, lists in Engine's chain order), rebuilds Engine's indexes, saves the snapshot, asks for the job, logs one timed line per step, and leaves only `m.db` and the snapshot in `Database2` (#182, #301). | `TestMountFirstInsert` (`pkg/bridge/mount_test.go`) |
| An export unchanged since a complete import costs a stat and one line: the exports are not opened, `m.db` stays byte for byte, no job starts, nothing changes under `PIONEER`, the audio or `Database2`, and the CLI run skips the volume. | `TestMountUnchangedIsStatOnly` (`pkg/bridge/mount_test.go`); `TestRBBridgeUnchangedExportSkipsTheVolume` (`internal/cli/rbbridge_test.go`); `TestPort5UnchangedWindowLeavesStickAndDatabaseFilesUntouched` (`pkg/bridge/port5_misc_test.go`) |
| A re-export that changes one list writes that list and nothing else, the new list is there at that mount, the plan line says one list (#48, #253), and the insert does not make Engine's trigger walk every list of the library (replica, 2026-11-24). | `TestMountPlaylistOnlyReexportWritesOnlyThatList`, `TestMountAddsListsWithoutEnginesWalkOfEveryList` (`pkg/bridge/mount_test.go`) |
| A row that already holds the export's fields is not written; rows whose export fields changed are written after the rows and lists are committed, 100 to a transaction, and a window with no time for them leaves them pending. | `TestMountLeavesRowsThatHoldTheExportsFieldsUnwritten`, `TestMountUpdatesRowsInBatchesAfterTheLists` (`pkg/bridge/mount_test.go`) |
| A re-export that only renumbers source IDs changes no row hash and no list hash and writes nothing to `m.db`; the snapshot takes the new source IDs. | `TestViewHashesIgnoreSourceIDsAndFollowContent` (`pkg/bridge/view_test.go`); `TestMountRenumberedReexportWritesNoRowAndRefreshesRefs` (`pkg/bridge/mount_test.go`) |
| Engine IDs, the library UUID and the `OverviewData` files stay through renumbering; a renamed or swapped file keeps its Engine ID and deck edits with no ghost row (#4); the row's path has the file's on-disk spelling (#6). | `TestPort1RenumberedSourcesKeepIDsUUIDAndRGB`, `TestPort1MovedAndSwappedPathsRetainEngineIDs` (`pkg/bridge/port1_mount_test.go`); `TestPort2OnDiskSpellingReachesThePublishedRow` (`pkg/bridge/port2_mount_misc_test.go`) |
| The window takes its deadline from the hook's `ENGINEX_HOOK_REMAINING_MS`: a re-export with a new list shows it at that mount and starts the job, a run given almost no time ends well before the kill, a window with no time for new rows adds none, commits what it did, and the next window adds them (#233, #264). | `TestMountPathAsTheHookRunsIt` (`internal/cli/rbbridge_test.go`); `TestMountOutOfTimeLeavesRowsPendingAndFinishesLater` (`pkg/bridge/mount_test.go`) |
| A window works in the order rows, lists, staged performance data (priority tracks first), covers, and stops at its budget. | **no test** |
| A track the export no longer has loses its row and list entries once the job has verified the stamp; a track whose file is not on the stick has no row and gets none again; with many files unfindable by name (a volume without UTF-8 names) nothing is removed and the job returns `ErrFilesUnreadable`; a row with a NULL track path does not break a re-export. | `TestPort1DeletedTracksAreRemovedAfterVerifiedFill`, `TestPort1UnreadableTrackNamesRefuseRemovalUntilRestored` (`pkg/bridge/port1_mount_test.go`); `TestPort2MissingTrackIsRemovedAndDuplicateSourceIsJoined`, `TestMountLeavesATrackWithoutItsFileWithoutARow` (`pkg/bridge/port2_mount_misc_test.go`); `TestPort5NullTrackPathDoesNotBreakReexport` (`pkg/bridge/port5_misc_test.go`) |
| A window never writes a library it must not: one another process has open (`ErrLibraryInUse`), a symlinked `Engine Library`, a hard-linked `m.db`, a read-only library (also the HFS+ view); a cancelled first build leaves no `Engine Library` folder and a finished one passes `integrity_check` and `foreign_key_check` (#53, #301). | `TestPort1OpenLibraryRefusesConcurrentProcess`, `TestPort1CancelledFirstBuildLeavesNoEngineLibrary` (`pkg/bridge/port1_mount_test.go`); `TestPort2ProtectedAndReadOnlyLibrariesAreNotWritten` (`pkg/bridge/port2_mount_misc_test.go`) |
| A row's tempo never takes rekordbox's 500 BPM clamp: it gets the grid's tempo, or 0 without a grid (CF-049); the staged half is in 2.4. | **no test** |
| Staged data is applied once, when `Rules` and the row hash match; what the deck wrote afterwards stays when the export changes the track (each field decided alone: hot cue slot, saved loop, grid and BPM, grid lock, rating), and a row nobody touched is replaced (#328). | `TestMountStagedDataAndDeckEdits` (`pkg/bridge/mount_test.go`); `TestPort1ReexportKeepsDeckPerformanceFieldsFieldByField` (`pkg/bridge/port1_mount_test.go`) |
| What Engine wrote while the job ran (a play, entries of the prepare list, the played indicator) survives the publishing window (#301). | `TestPort1PublishingWindowCarriesEngineWrites` (`pkg/bridge/port1_mount_test.go`) |
| Edits queued while Engine had the library open (`enginex-pending.json`: a hot cue, genre, comment, label) are applied at the next mount (#314). | `TestPort2QueuedHotCueAppliesAtMount` (`pkg/bridge/port2_mount_misc_test.go`); `TestApplyPendingTrackMetadata` (`pkg/enginedb/pending_metadata_test.go`) |

### 2.3 Take-over of an older build's library, and Engine's own rows and lists (#252)

| Requirement | Tests |
|---|---|
| What Engine made itself is the same after a next library is swapped in: a track of its own with its cue, a list the DJ made, a prepared and a played track, and the history file's entries (by row ID and library UUID), whether made before the job copied the library or while the next library waited. The snapshot owns none of Engine's rows. | `TestSwapKeepsWhatEngineMadeItself`, `TestNextLibraryKeepsWhatTheDeckWroteBeforeTheSwap` (`pkg/bridge/next_test.go`); `TestMirrorLibraryTakesEveryRowOnceAndThenWhatEngineWrote`, `TestMirrorLibraryRefusesARowBothAdded` (`pkg/enginedb/mirror_test.go`) |
| No track gets another track's data across a swap (the damage of 2026-11-23): with every rekordbox ID naming another file, five new tracks, a track Engine added and a row the deck wrote, every row holds the data built for its own file under the row ID it had. | `TestSwapNeverGivesATrackAnotherTracksData` (`pkg/bridge/next_test.go`) |
| A row the deck's UI hook filled from the stage (`filled.jsonl`) is the bridge's own write: newer staged data replaces it; Engine's rewrite of the main cue "as imported" at a load is not a deck edit; an adjusted main cue is. An index line of the stage names the track's row. | `TestMountTakesRowsTheDeckHookFilledAsTheBridgesOwn` (`pkg/bridge/mount_test.go`); `TestStageLineNamesTheRowAndFilledReadsTheHooksRecord` (`pkg/rbstate/rbstate_test.go`) |
| A library of an older build (`.rb-bridge-state.db`, no snapshot) is taken over once, in a window: paths, Engine IDs, list IDs and written digests come from the old state (also when its analysis records are malformed), every track is marked for the job, the old state file is removed after the snapshot is saved, the next mount is Bypassed, and nothing the deck wrote is lost (played marks, `dateAdded`, cues, loops, active-loop mask, list tree and order). | `TestPort1LegacyTakeoverUsesOwnedPathsWithMalformedAnalysis`, `TestPort1LegacyTakeoverPreservesDeckEdits` (`pkg/bridge/port1_mount_test.go`) |
| The take-over of a large library (about 5,900 rows) finishes over several windows, without writing rows that already hold the export's fields (replica, 2026-11-25). | `TestMountLeavesRowsThatHoldTheExportsFieldsUnwritten`, `TestMountUpdatesRowsInBatchesAfterTheLists` (`pkg/bridge/mount_test.go`) |
| A change of the offset or grid rule (`rbstate.Rules`) makes the next insert read the affected tracks again, once, unless the DJ edited the row (#178, #233, #333). | **no test** |
| A deck running the previous build (5.3.0.5-r1) that meets a stick last written by v2 does not damage it. | **no test** |
| Engine's empty skeleton `m.db` is not a library: the export is converted over it, and a track the deck had loaded and the library UUID are kept. | `TestPort1SkeletonLibraryIsReplacedByExport` (`pkg/bridge/port1_mount_test.go`) |
| A row Engine made for a file the export also names is not overwritten or adopted (the title stays, the snapshot marks it native), and the job never stages a native row (#252). | `TestPort2NativeTrackConflictRemainsNative` (`pkg/bridge/port2_mount_misc_test.go`); `TestFillPublishesOnlyWhatAWindowWouldApply` (`pkg/bridge/fill_test.go`) |
| A track Engine analysed itself, or a row that holds an import made under an older offset, gets rekordbox's grid, cues and loops (at this import's positions), unless the DJ edited them (#233, #253). | `TestPort2EngineAnalysisIsReplacedWhileDeckCueIsKept` (`pkg/bridge/port2_mount_misc_test.go`); `TestRealignKeptMovesTheImportsPositionsAndKeepsTheDecks` (`pkg/enginedb/realign_test.go`) |
| Lists the DJ made survive every rebuild: one under an imported folder moves to the root when the folder is renamed, removed or rebuilt, with its tracks, sibling order and nested folders; a root name clash gets a suffix; a list Engine exported is never adopted; none is lost when the bridge's state is lost (#252, BH2-A4-3). | `TestPort1NativeChildPlaylistSurvivesImportedFolderRemoval` (`pkg/bridge/port1_mount_test.go`); `TestPort5NativeSiblingChainSurvivesFolderRebuild`, `TestPort5NestedNativeHierarchySurvivesFolderRename`, `TestPort5NativeChildParentChangesPreserveData`, `TestPort5NativeSiblingRelativeOrderSurvivesRebuild`, `TestPort5ExportedNativeChildIsNeverAdopted`, `TestPort5NativeChildSurvivesLostBridgeState` (`pkg/bridge/port5_playlist_test.go`) |
| A write that fails part-way (a trigger aborting the reparent, the folder delete or the list write) leaves `m.db` and the snapshot as they were. | `TestPort2FailedMountRollsBackLiveLibrary` (`pkg/bridge/port2_mount_misc_test.go`); `TestPort5PlaylistRebuildRollsBackNativeChildren` (`pkg/bridge/port5_playlist_test.go`) |
| When Engine recreates the imported lists under new IDs, the next window reuses them and makes no duplicate lists. | `TestPort2RebuiltPlaylistsAreReusedWithoutDuplicates` (`pkg/bridge/port2_mount_misc_test.go`) |

### 2.4 The background job: order, priority, pacing, degraded tracks (#53), covers, list waveforms

| Requirement | Tests |
|---|---|
| The job stages every track of the export (facts, row hash, audio and ANLZ signatures, beats, cues, overview, detail files), verifies the stamp, and writes nothing on the stick but the mini waveforms of finished tracks: `m.db`, the snapshot and the exports stay byte for byte. | `TestFillStagesEveryTrackAndWritesNothingElseOnTheStick` (`pkg/bridge/fill_test.go`) |
| A track the stage or the snapshot already has for its files is not built again, and once the stamp is verified a built track is not even stat'ed; a changed file builds that track alone; a degraded track is not read again while its files are unchanged; a file that comes back without a re-export is found by the next job. | `TestFillSkipsWhatIsStagedOrBuilt`, `TestFillFindsAMissingFileThatCameBack` (`pkg/bridge/fill_test.go`); `TestStatTrackSignatureMatchesTheBuild` (`pkg/enginedb/trackbuild_test.go`) |
| Priority tracks go first, newest request first, then every other track by path; an unknown path does not hold the job up; the acks survive a restarted queue; the queue coalesces identical requests, keeps one 0600 record per volume, and follows the deck-activity file; the deck and the browser enqueue through a loopback-only endpoint. | `TestFillServesThePriorityQueueFirst` (`pkg/bridge/fill_test.go`); `TestPriorityQueueCoalescePermissionsAndActivity` (`pkg/bridge/priority_test.go`); `TestRekordboxPriorityFromDecksAndBrowse` (`pkg/webui/rekordbox_priority_test.go`) |
| With no deck playing the job reads at full speed; while one plays, reads are at most 64 KiB with a pause after each read that went to the stick (none after a page-cache read), and no readahead reads past the pause. | `TestFillYieldsToAPlayingDeck`, `TestPacerPausesOnlyAfterTheStick` (`pkg/bridge/fill_test.go`); `TestBuildTrackPacesItsReadsAndStopsCleanly` (`pkg/enginedb/trackbuild_test.go`); `TestBuildTrackPacedReadsLeaveTheRestOfTheFileUnread` (`pkg/enginedb/trackbuild_linux_test.go`) |
| A stop ends the job at once and keeps what it staged; the next job goes on from there; the progress count never goes down and reaches the total (#301, #328); with `/data` at the stage's floor the job stops, reports what it staged, and does not fail. | `TestFillStopsCleanlyAndGoesOnLater`, `TestFillStopsAtAFullStageAndPublishesWhatItHas` (`pkg/bridge/fill_test.go`) |
| The job never runs beside a window (it waits while a window holds the volume), and a job on a stick no window left a snapshot on ends quietly, with no error for the browser and no remount. | `TestFillWaitsForTheWindow` (`pkg/bridge/fill_test.go`); `TestImportJobWithoutASnapshotEndsQuietly` (`internal/cli/rbbridge_import_test.go`) |
| A track the volume refuses (a symlink, also in a directory the job cannot list; traversal, NUL, a directory, a name two files share) is logged and left unstaged, and the job stages the others and still verifies the stamp (#252). | `TestFillGoesOnPastATrackItCannotRead` (`pkg/bridge/fill_test.go`); `TestStatTrackUnreadableDirectoryCannotHideSymlink`, `TestBuildTrackRefusesUnsafePaths` (`pkg/enginedb/trackbuild_test.go`) |
| One unusable file degrades only its track, with the reason (a bad audio header, an over-large cover, a malformed or truncated ANLZ section); the other tracks are built. A missing audio file is a record that says so; one that cannot be read now is no record (#53, #253). | `TestBuildTrackDegradesOnlyTheTrackWithTheBadFile`, `TestBuildTrackDegradesOnMalformedOrTruncatedAnalysis`, `TestBuildTrackMissingAndUnreadableAudio` (`pkg/enginedb/trackbuild_test.go`) |
| The job asks for the remount only for records a window would apply, and never twice for the same set, so a window that takes nothing cannot cause a remount loop; records the window refuses are dropped from the stage. | `TestFillPublishesOnlyWhatAWindowWouldApply` (`pkg/bridge/fill_test.go`) |
| The job runs as its own systemd unit at idle priority, with the name the hook uses, waits for the visible mount, and a finished job releases the global lock so a second stick's job runs (#301). | `TestImportUnitMatchesTheHook`, `TestStartImportRunsAnIdleSystemdUnit`, `TestImportJobWaitsForTheVisibleMount`, `TestMountPathStartsTheBackgroundImport` (`internal/cli/rbbridge_import_test.go`); `TestIssue301CompletedImportReleasesGlobalLock` (`internal/cli/rbbridge_review_b2_test.go`) |
| The remount waits until Engine has closed the stick's files, no deck or Engine Link peer holds a track of it, and a quiet time has passed; it leaves an ejected stick alone, keeps the real failure, and finds the device under an HFS+ view (#301). | `TestIssue301RemountRemovesAndAddsThePartition`, `TestIssue301RemountWaitsWhileADeckHoldsATrackFromTheStick`, `TestIssue301RemountLeavesAnEjectedStick`, `TestIssue301RemountFailureKeepsAnError`, `TestIssue301RemountFindsTheDeviceUnderAView` (`internal/cli/rbbridge_remount_test.go`); `TestIssue301RemountKeepsALibraryAPeerLoadedFrom`, `TestIssue301RemountAsksEngineLinkPeers` (`internal/cli/rbbridge_review_b2_test.go`); `TestIssue301WaitsThenRemountsAutomatically` (`internal/cli/rbbridge_quiet_test.go`) |
| One start offset for a file however it reaches the deck: cues, beats and loops shift by 1105 samples (MP3 with an Info/LAME tag), 1152 (Xing only) or 0 (headerless, WAV), clamped at 0, the file header's rate winning over a wrong export rate (#178); AAC keeps the export's timing when the `moov` is cut, and an HE-AAC file shifts by its 2048 priming samples. | `TestBuildTrackOffsetRule4AcrossAudioClasses`, `TestBuildTrackAACKeepsTheExportTiming` (`pkg/enginedb/trackbuild_test.go`); `TestEncodePerformanceMP3StartOffsetAndClamping` (`pkg/enginedb/performance_test.go`) |
| The staged tempo ignores rekordbox's 500 BPM clamp (the grid's tempo, or 0 without a grid; a grid whose first beat carries the clamp is dropped), and a WAV with a 32-bit container and 24 valid bits keeps the right sample count (CF-049). | `TestBuildTrackDropsRekordbox500BPMClamp` (`pkg/enginedb/trackbuild_test.go`); `TestPort2Bogus500BPMUsesGridOrZero`, `TestPort2WAVContainerBitsKeepValidSampleCount` (`pkg/enginedb/port2_buildtrack_test.go`) |
| The OneLibrary cue table's cues join the ANLZ cues, a valid RGB overview is staged, rekordbox's 3-band bytes read as low, mid, high, the blobs are as Engine writes them (68-byte `trackData`, 129-byte empty quick cues, opaque cue colours, waveform peaks; GL-013, #101), and with a PWV7 in the `.2EX` the `.EXT` waveform bodies are not read (#253). | `TestBuildTrackStagesCuesFromTheExportAndAnRGBOverview`, `TestBuildTrackLeavesTheEXTWaveformsUnread` (`pkg/enginedb/trackbuild_test.go`); `TestPWV7BandsAreLowMidHighOnRekordboxDemoTracks` (`pkg/enginedb/pwv7_order_test.go`); `TestQCompressAndAbsentCues`, `TestExactContainerSamplesAndLegacyCueColor`, `TestTrackDataWaveformPeaks`, `TestTrackDataPeaksUpgrade` (`pkg/enginedb/performance_test.go`) |
| The export's cover is used first, a cover inside the audio file only when the export names none, and extraction is bounded (ID3v2.2, FLAC and MP4; lying lengths and oversized images are refused without a panic); a staged cover is applied as one `AlbumArt` row with a 20-byte hash and an `Artwork/` file, shared by tracks with the same cover. | `TestBuildTrackTakesTheEmbeddedCoverOnlyWithoutAnExportCover` (`pkg/enginedb/trackbuild_test.go`); `TestExtractEmbeddedArtwork` (`pkg/enginedb/artwork_embedded_test.go`); `TestArtworkReadIsBoundedByTheFileAndTheCap`, `TestArtworkFromID3v22PIC` (`pkg/enginedb/artwork_bounds_test.go`); `TestTruncatedFLACPictureBlockAllocatesOnlyTheFile`, `TestMalformedArtworkLengthsAreRefusedWithoutPanic` (`pkg/enginedb/artwork_lengths_test.go`); `TestPutArtworkRejectsOversizedPNGDimensions` (`pkg/enginedb/bh2_a4_test.go`); `TestPort2StagedCoverIsDeduplicatedAndServed` (`pkg/bridge/port2_mount_misc_test.go`) |
| Covers are applied after staged data, and a cover that cannot be read is not tried again until the export changes the track. | **no test** |
| Every finished track gets its `OverviewData/<uuid>/<id>.rgb`, written atomically by the job, also for a prioritised track before the remount; a valid file is never replaced, an empty one is; the encoder writes PWV6 over PWV7, a PWV4 fallback, 3,099 bytes and a trailer equal to the per-channel maxima. | `TestFillWritesTheMiniWaveformNeverOverAValidOne` (`pkg/bridge/fill_test.go`); `TestEncodeRGBOverviewPWV6AndPWV4FormatAndPruneSafe` (`pkg/enginedb/rgb_overview_generate_test.go`) |
| A track's `.rgb` is its own: the bridge never gives a row's ID to another file (Engine IDs are stable, `Track.id` is `AUTOINCREMENT`, a library is built only where none is), so v2 has no mount-time prune of the stick's files. A track whose audio or analysis changed in place keeps the `.rgb` it has: the job replaces only an empty or invalid one. | **no test** for the changed-in-place case (a known gap, 2.9); the deck's own library is still pruned at boot by `engine-ui-guard` (`--prune-rgb-overviews`, `tests/test_rootfs_overlay.sh`) |
| Deck-speed targets: a whole fill of 2,420 tracks in about 354 s with no deck playing and 1,003 s while one plays; a prioritised track staged in 0.4 to 1.6 s; no underrun of a playing deck's reads. | **no test** |

### 2.5 Cue write-back: gate, identity by path, what is written where, crash safety (#271, #314, #321), the watcher and eject

| Requirement | Tests |
|---|---|
| Write-back writes only when the snapshot has no pending import work (a pending import means no write and one log line), and is allowed again once the job has verified the stamp after a re-export that changed no track file (#321). | `TestWriteBackGateRejectsPendingImport` (`pkg/bridge/writeback_test.go`); `TestPort5PlaylistOnlyReexportVerifiesStampForWriteback`, `TestPort5WritebackGateUsesCurrentExportAfterFill` (`pkg/bridge/port5_misc_test.go`) |
| A snapshot whose stamp no longer matches the exports now on the stick refuses write-back, and so does a missing or older stamp. | **no test** |
| A deck row maps to its rows in the libraries by the snapshot's path identity, never by source ID alone (a source ID that now names another path matches nothing); a reference whose library is unavailable stays pending and is not recorded as synced; an unreadable OneLibrary does not stop the Device Library write (#321). | `TestBuildChangesRequiresSnapshotPathIdentity`, `TestBuildChangesDoesNotConfirmMissingSourceReference` (`pkg/bridge/writeback_test.go`); `TestPort4WriteBackRejectsSnapshotPathMismatch` (`pkg/bridge/port4_writeback_test.go`); `TestWriteBackUsesSnapshotPathAndSkipsTheSecondPass` (`pkg/bridge/writeback_integration_test.go`); `TestP3UnreadableOneLibraryIsExcludedFromWriteBack` (`pkg/bridge/port3_writeback_test.go`) |
| Only rows whose `lastEditTime` moved are read, and a pass over rows Engine only re-analysed reads no export, derives no key and writes no file; an edit made before a pull is found at the next pass; a row Engine rewrote unchanged is read once (#321). | `TestWriteBackAnalysisOnlyPassSkipsExportRead` (`pkg/bridge/writeback_test.go`); `TestPort4WriteBackAnalysisOnlyChangeSkipsExportRead` (`pkg/bridge/port4_writeback_test.go`); `TestP3EditBeforePullIsFoundOnNextPass`, `TestP3SessionReadsRewrittenRowOnce` (`pkg/bridge/port3_session_test.go`) |
| A hot cue set on the deck is written into the track's ANLZ files, the first original of each file is saved under `.enginex-backup/<date>/`, a second pass changes nothing, and the cue survives a later insert (#271). Whether that insert is taken for an unchanged export is not asserted. | `TestP3CueWriteBackUpdatesANLZBackupAndReinsert` (`pkg/bridge/port3_writeback_test.go`); `TestPort4WriteBackPDBCuesMetadataBackupAndConvergence` (`pkg/bridge/port4_writeback_test.go`) |
| Genre, comment and label edited on the deck reach `export.pdb` and the OneLibrary (#314). | `TestP3MetadataWriteBackUpdatesPDB` (`pkg/bridge/port3_writeback_test.go`); `TestP3MetadataWriteBackUpdatesPDBAndOneLibrary` (`pkg/bridge/port3_onelibrary_test.go`) |
| A cue the DJ deleted is written back, a deactivated saved loop is seen, and an edit that sits only in `m.db`'s uncheckpointed WAL is seen; an untouched or merely clamped cue is not reported changed (uncoloured cues stay uncoloured, cues at the clamp keep their time, a legacy MP3 offset drift is not rewritten) (#321). | `TestP3WriteBackRoundTripsCueDeletion`, `TestP3WriteBackDetectsActiveLoopDeactivation`, `TestP3WriteBackReadsUncheckpointedWAL` (`pkg/bridge/port3_writeback_test.go`); `TestP3MergeEngineCuesPreservesUncolouredImportedCues`, `TestP3MergeEngineCuesClampAtZeroPreserved`, `TestP3MergeEngineCuesPreservesLegacyMP3OffsetDrift`, `TestP3TrackRekordboxOffsetRejectsUnreadableAudio` (`pkg/bridge/port3_safety_test.go`) |
| MP3 and AAC start offsets are put back on write: a cue set at 65 ms lands at 65 ms within 1 ms. | **no test** |
| A hot cue reaches the OneLibrary cue table (re-encrypted, `integrity_check` ok) as well as the ANLZ files; pad slots A to H map to hot cues 1 to 8; a macOS stick with a hidden `.PIONEER` is written. | **no test** |
| Each pass logs one line (tracks, read, changed, per-phase times, files, bytes, pass kind). | `TestP3WriteBackLogsOnePassLine` (`pkg/bridge/port3_log_test.go`) |
| The watcher writes a deck edit a quiet time after Engine's last write, once per burst, does not poll an unchanged `m.db`, drops a read that overlaps a write, waits for a running conversion and for an Eject, and cleans cut leftovers when it attaches (#321). | `TestP3WatcherWritesOnceAfterQuietPeriod`, `TestP3WatcherDefersConversionAndFinalPassWritesEjectEdit` (`pkg/bridge/port3_watcher_test.go`); `TestPort4WatcherWaitsForQuietAndDoesNotRepoll`, `TestPort4WatcherDefersConversionAndEjectToTheirPasses`, `TestPort4WatcherDropsOverlappingReadAndWritesLatestEdit`, `TestPort4WatcherCleansCutLeftoversOnAttach` (`pkg/bridge/port4_watch_test.go`) |
| `--writeback-cues --on-eject` hands the pass to stream-sync's final pass, does its own pass when stream-sync does not answer, and gives up on a pass that overruns; the hook runs it at Eject only for a stick with a snapshot. | `TestRBBridgeOnEjectHandsThePassToStreamSync`, `TestRBBridgeOnEjectGivesUpOnAPassThatOverruns` (`internal/cli/rbbridge_writeback_test.go`); `TestRBBridgeWriteBackOnEjectFlag` (`internal/cli/rbbridge_test.go`); `test_eject_runs_cue_writeback`, `test_eject_writeback_overrun` (`tools/edisksd-hook/test_edisksd_hook.c`) |
| The web write-back endpoint refuses GET, a bad drive and a missing drive, skips an unstamped library, and runs the Eject pass only for a request from the deck itself. | `TestHandleRekordboxWriteback`, `TestRekordboxWritebackFinalOnlyFromTheDeck` (`pkg/webui/rekordbox_writeback_test.go`) |
| A stick that hangs at insert holds only itself: another stick's write-back and mount go on (#321). | **no test** |

### 2.6 Crash safety of the window and the snapshot (design section 5)

| Requirement | Tests |
|---|---|
| A cut at any point of the window that swaps a next library in, and the next window on what was on the stick then, end in the library of an undisturbed swap. | `TestSwapCutAtEveryStepEndsInTheSameLibrary` (`pkg/bridge/next_test.go`) |
| A power cut or the hook's kill between any two writes of a window (first insert, a re-export with new lists and tracks, a window that applies staged data) ends in the same library as an undisturbed run, every track once, with no next-snapshot left. | `TestMountCutAtEveryStepEndsInTheSameLibrary` (`pkg/bridge/mount_test.go`) |
| A snapshot that is cut short or has changed bytes is no snapshot; the next-snapshot is invisible to readers until promoted, and a cut one is no claim. | `TestSnapshotRoundTripAndHeader`, `TestSnapshotCutOrChangedIsAbsent`, `TestNextSnapshotIsSeparateUntilPromoted` (`pkg/rbstate/rbstate_test.go`) |
| A file replaced on the stick is whole at every point a pull can separate; the old file is left when it may not be freed, and a leftover or replaced file is freed only when the FAT gives it all its clusters (FAT16, FAT32, short chains; free clusters, wrong lengths and FAT12 are not owned) (#321). | `TestReplaceFileIsWholeAtEveryCut`, `TestReplaceFileLeavesTheOldFileWhenItMayNotBeFreed` (`pkg/rbstate/rbstate_test.go`); `TestIssue321FATChainOwned` (`pkg/bridge/fat_test.go`) |
| One lock per volume: a second holder waits or is refused, and a waiting one takes it when the first lets go. | `TestLockVolumeExcludesAndWaits` (`pkg/rbstate/rbstate_test.go`) |
| A write-back pass cut at every write step leaves no temporary file after the next mount's clean-up, every ANLZ file parses, and the pass after writes the cue; a staged copy cut at any offset is not trusted; a mark line through a symlink never leaves the mount; a pull before a rename, or a failing second rename, leaves every original whole (#321). | `TestPort4CutPassRecoversAndConverges`, `TestPort4StagedCopiesCutAtAnyOffsetAreRejected`, `TestPort4CutMarkCleanupStaysInsideMount` (`pkg/bridge/port4_safety_test.go`); `TestP3PullBeforeRenameLeavesOriginalFilesIntact`, `TestP3SecondRenameFailureLeavesAllExportTargetsIntact` (`pkg/bridge/port3_writeback_test.go`) |
| Write order: the backup directories exist before the mark, and a replaced file is held open until the new entry is synced. | `TestPort4BackupDirectoriesExistBeforeWriteBackMark`, `TestPort4ReplacedFilesStayHeldUntilRenameSync` (`pkg/bridge/port4_safety_test.go`) |
| The stage on `/data`: a record round-trips, a newer `Put` wins, a torn last index line is ignored, names are safe, and the performance file round-trips and refuses cut or oversized input. | `TestStagePutGetDropAndTornIndex`, `TestStageRootInTestsIsPrivateAndNamesAreSafe`, `TestPerfFileRoundTripAndBounds` (`pkg/rbstate/rbstate_test.go`) |
| On a real FAT32 or exFAT volume a stale entry (a file naming freed clusters) is found before a pass creates anything: the pass refuses naming the path, writes nothing, and writes again after a disk check (#321). | **no test** |
| Engine does not move `lastRekordBoxLibraryImportReadCounter` between a cut window and the next one (the assumption stated in design section 5). | **no test** |

### 2.7 Other readers: shadow display, dbhealer, MP3 grid fix, CLI status, the edisksd hook

| Requirement | Tests |
|---|---|
| The webui shadow display finds a bridge track's detail waveform through the snapshot's references to the ANLZ files, and prefers a valid PWV7, then PWV5. | `TestBridgeDetailsFollowTheReferenceToTheANLZFiles` (`pkg/webui/issue253_detail_test.go`) |
| `dbhealer` leaves the rows the bridge manages alone; until a mount has taken a library over it reads the old state file. | `TestHealMountDataOnlyNormalizesMissingNFDPaths`, `TestHealMountDataOnlyLeavesTracksWhenBridgeStateIsUnreadable` (`pkg/dbhealer/normalization_test.go`) |
| The MP3 grid fix and `dbhealer` find the bridge's rows by the snapshot's Engine IDs (`bridgeOwnedTracks`, `bridgeManagedTracks`). | **no test** |
| The MP3 grid fix shifts the grid, cues and loops of an MP3 an earlier EngineX analysed, once, also on an unchanged export, and its cursor survives a hard exit (#246). | `TestPort2MountFixesAnEngineXMP3GridOnAnUnchangedExport` (`pkg/bridge/port2_mount_misc_test.go`); `TestFixMP3GridsExactShiftAndClamping`, `TestFixMP3GridsUntouchedTrackClasses`, `TestFixMP3GridsCursorSurvivesProcessExit` (`pkg/enginedb/mp3grid_test.go`) |
| The USB banner follows the job (importing with done and total, waiting, removed when done), comes back after the hook clears it, belongs to its own drive's job, and keeps a failed job's error visible (#301, #328). | `TestRBBridgeUSBLoadingStatusLifecycle`, `TestIssue328StatusComesBackAfterTheHookClearsIt` (`internal/cli/rbbridge_status_test.go`); `TestImportStatusWaitingStaysUntilDone` (`internal/cli/rbbridge_import_test.go`); `TestIssue301StatusIsOwnedByItsJob`, `TestIssue301LateTransitionKeepsNewerStatus`, `TestIssue301ImportFailureStaysVisible` (`internal/cli/rbbridge_review_b2_test.go`) |
| The hook stops the job under the job's unit name before an Eject, and the Eject's write-back needs `.rb-bridge-snapshot` on the stick. | `test_eject_stops_the_background_import`, `test_eject_runs_cue_writeback` (`tools/edisksd-hook/test_edisksd_hook.c`) |
| A read-only HFS+ stick runs the bridge through the hook's view and its artwork folder is writable. | `test_hfs_mount_runs_bridge`, `test_hfs_view_artwork_is_writable` (`tools/edisksd-hook/test_edisksd_hook.c`) |
| Write-back never runs on a lazy detach, a pulled stick or a read-only view. | **no test** |

### 2.8 Old tests that pinned a mechanism that is gone

These tests were not ported. Each group says why; the behaviour behind a group, where it still exists, has a row in
2.1 to 2.7. Old names are given as written in the removed files.

- **Export choice by file time** (`TestExportSelectionMTimeAndFallback`, `TestExportSelectionFreshness`,
  `TestExportTieKeepsPDBImport`, `TestFreshExportTieKeepsSQLImport`, `TestMalformedLocalExportDoesNotFallBackToStaged`):
  v2 reads both libraries and joins them; there is no preferred export (design 1.4). Replaced by the view tests of 2.1.
- **The old merge and ghost filter** (`TestMergeExportLibraries*`,
  `TestDualExportDropsGhostEmptyRootPlaylistsAbsentFromPDB`):
  `mergeExportLibraries` and `filterGhostEmptyRootPlaylists` are gone. Replaced by
  `TestViewOfTwoLibrariesHasThreeFoldersAndEveryTrackOnce`
  and `TestViewHidesOneLibraryGhostRootLists`.
- **The state database and its marks** (`BridgeState`, `BridgeAnalysis`, `BridgeWritten`, `BridgeSynced`,
  `.rb-bridge-state.db`, `.rb-bridge-stamp`, `.rb-bridge-pending`, `fast:` fingerprints): `TestIssue272*`,
  `TestIssue253StateStaysWithinAPerTrackBudget`, `TestIssue233UninspectedExistingOffsetStaysPending`,
  `TestIssue233Large*`, `TestIssue264*`, `TestLargeLibrary*`, `TestSmallLibraryDeferredMountWritesPendingAndResumes`,
  `TestFailedImportRollsBackBridgeSchema`, `TestEngineSchemaResetKeepsBridgeState`,
  `TestImportedRecordChangedTreatsOpaqueFingerprintAsChanged`,
  `TestInvalidateUnsyncedEditsKeepsWrittenAndDropsEdited`, `TestSingleExportStampUnchanged`, and the `pkg/enginedb`
  compaction and shrink tests (`TestShrinkBridgeAnalysis*`, `TestIssue272Compact*`, `BenchmarkPackAnalysisLargeBatch`).
  The snapshot (`pkg/rbstate`) replaces them (design 1.3, 1.9); the behaviours are the window and write-back rows.
- **Waveform copies in the state database** (`TestIssue253BrokenANLZKeepsItsWaveformRow`,
  `TestIssue253MountReadsNoANLZFileForTheWaveformCopies`, `TestIssue253UpdateLeavesTheWaveformCopiesBehind`,
  `TestIssue253GateIsTheDetailReader`, `TestIssue253GateReadsTheFileAsTheDisplayDoes`): v2 keeps references only and
  the shadow display shows Engine's own overview for a track whose ANLZ later fails (design 1.11 item 3). The build
  half is `TestBuildTrackDegradesOnMalformedOrTruncatedAnalysis`.
- **Quick update, list-first stage, staged library copy, publish directory** (`TestIssue253Cut*`,
  `TestIssue253QuickUpdate*`, `TestIssue253ReexportTooLargeForTheMountPublishesItsListFirst`,
  `TestIssue253FastPublishStartsTheBackgroundUpdate`,
  `TestIssue301Stage*`, `TestIssue301BulkRefusesTheLiveLibrary`, `TestInterruptedFastPublishIsCompleted`,
  `TestVacuumStageMDB*`,
  `TestMountImportPreservesConcurrentEngineWrite`, `TestUpdatePublish*`,
  `TestIssue301UpdateLeavesALibraryEngineChanged`):
  the window writes `m.db` in place, so there is no stage copy to merge, vacuum or publish. The cut points are
  `TestMountCutAtEveryStepEndsInTheSameLibrary`; what Engine wrote meanwhile is
  `TestPort1PublishingWindowCarriesEngineWrites`.
- **Mount-time budgets and deadlines** (`TestConvertAtMountKeepsOverviewDeadlineWhenImportFails`,
  `TestConvertAtMountHandsOffBeforeHookKillWhenPruneConsumesBudget`,
  `TestIssue233ExpiredIncrementalBudgetDoesNotReadEveryNewMP3`,
  `TestIssue264ResumeNeverDeadlocksOnExpiredDeadline`, `TestIssue264SlowReconcileDoesNotExhaustFileReadingBudget`,
  `TestConvertAtMountMP3GridsShareReadBudget`): a window has one deadline and the job has none. Replaced by
  `TestMountOutOfTimeLeavesRowsPendingAndFinishesLater` and `TestMountPathAsTheHookRunsIt`.
- **Overview backfill and zero-maximum completion** (`TestStampBypassCompletesOldOverviewsOnce`,
  `TestOverviewBackfillCutShortGoesOnAtTheNextInsert`, `TestResumedImportCompletesOldOverviews`,
  `TestOverviewProgress*`,
  `TestFillOverviewMaximaGoesOnWhereTheLastMountStopped` and the rest of that family): the backfill and
  `.rb-bridge-overview` are deleted; the UI hook draws a zero-trailer overview at stock height (row 272).
- **Artwork migration** (`TestIssue233ArtworkDeadlineCommitsProgressAndRetries`,
  `TestMigrateArtworkUsesCoveringHashIndex`,
  `TestIssue233StampBypassDoesNotReloadMigratedArtwork`): `MigrateArtwork` is deleted (design 1.9).
- **The verified-`.rgb` cache and the mount-time prune** (`TestRGBVerifiedCacheAndCountMissing`,
  `TestPruneRGBOverviewsRespectsMountBudget`, `TestPublishRemovesRGBOverviewsOfAnotherTrack`): deleted with the cache.
  `TestRenameStagePreservesValidDestinationRGB` and `TestPublishGeneratesAndPreservesRGBOverviews` are replaced by
  `TestFillWritesTheMiniWaveformNeverOverAValidOne`. v2 gives no row's ID to another file, so nothing is pruned at a mount (2.4).
- **Source-ID lookup and stamp text** (`TestResolveRekordboxTrackUsesPathBeforeShiftedID`,
  `TestResolveRekordboxTrackRejectsIDWithDifferentPath`, `TestWriteBackStampReason`): `pdbImportKey` and the "v4" text
  are gone. Replaced by `TestBuildChangesRequiresSnapshotPathIdentity` and `TestWriteBackGateRejectsPendingImport`; the
  stamp-mismatch cases are not ported (2.9).
- **Offset and grid versions carried by the stamp** (`TestOneOffsetRuleCompletedLibraryIsReadAgainOnce`,
  `TestOneOffsetRuleFrozenRowsAreConvertedAgainOnce`, `TestAACGridMigratesAnUnchangedV2Export`, the resync half of
  `TestIssue178MP3OffsetClassesAndExistingLibraryResync`): `rbstate.Rules` in each track's signature replaces the stamp
  prefix. The positions are `TestBuildTrackOffsetRule4AcrossAudioClasses`; no test raises `Rules` (2.9).
- **Fast rows and the fast-base file** (`TestIssue328AStageWithoutItsFastBase*`, `TestIssue328B*`): `BridgeFastBase` and
  `fast-base.db` are deleted. The field rule is `TestPort1ReexportKeepsDeckPerformanceFieldsFieldByField`.
- **Old import internals in `pkg/enginedb`** (`TestIssue301TrustPathsSkipsTheLookup`,
  `TestOrderTracksForPreparePriorityThenPathSorted`,
  `TestLargePDBPreparationProgress253`, `TestPrepare252CaseDifferencesBoundWorkAndMakeProgress`,
  `TestWriteEntitiesPreservesEntityChain`,
  `TestOpenReservesWriterBeforeReading`, the identity benchmarks): `sync.go` and `prepare.go` are replaced by
  `library.go`
  and `trackbuild.go`. Two files that differ only in case are one key now (`TestFoldPath`).
- **The on-stick lock file and the old test harness** (`TestCleanupKeepsALockHeldByAnotherBridge`,
  `TestInChildForTest*`):
  `.rb-bridge.lock` is gone (`TestLockVolumeExcludesAndWaits` covers the lock) and `InChildForTest` was infrastructure.
- **Assertions the port reports dropped on purpose.** Port 1 shrank two matrices (600 tracks and 71 unreadable names to
  100 and 7; the 5/40/60 deletion cases to 20 tracks and 4 files). Port 2 replaced `Unchanged` and stamp assertions
  with `Bypassed` and byte stability. Port 3 dropped the inotify assumptions (v2 polls every 100 ms), the exact global
  key-derivation count, and the `rc3_cues_test.go` analysis-cap re-export (the cap state is gone). Port 4 dropped the
  per-step temp truncation and exFAT directory-entry replay, the loop-device `mkfs`/`fsck` stale-chain tests, the
  `strace` syscall-order parse (the hook now observes the directory state at the mark), the FIFO stall and cross-stick
  scheduling case of
  `issue321_attach_linux_test.go`, and the key-derivation counter. Port 5 dropped the late counter-update
  injection of the rollback test and the old `Sync` result flags.

### 2.9 Requirements without a test

The rows above marked **no test**, by area.

- 2.1: the bridge reads `rb_library_view` from `settings.conf` (`ReadLayout`), and the deck's settings menu offers the
  four choices. Only the web console side of the setting is tested.
- 2.2: the window's work order under a budget (rows, lists, staged data with priority tracks first, covers).
- 2.2: a row's tempo ignores the 500 BPM clamp (only the staged half is tested, in 2.4).
- 2.3: a change of `rbstate.Rules` makes the next insert read the affected tracks again, once.
- 2.3: a deck running 5.3.0.5-r1 meets a stick last written by v2 (to be tried with the `147e37fb` step binary).
- 2.4: covers are applied after staged data, and an unreadable cover is not tried again until the export changes.
- 2.4: a track that was analysed again in rekordbox, or whose audio file was replaced, keeps its old list waveform
  file: the job writes an `.rgb` only where none is valid (`TestFillWritesTheMiniWaveformNeverOverAValidOne` pins
  that). The old mount-time prune removed such a file; it was also the step that ran a mount past the hook's kill.
  Replacing the file when its content differs from the staged one is a small change in the job (one compare of a
  file it reads anyway): a decision for the maintainer.
- 2.4: the deck-speed targets (354 s and 1,003 s fills, 0.4 to 1.6 s for a prioritised track). The gate is the replica
  script of design section 6, not a Go test.
- 2.5: a stamp that no longer matches the exports, or is missing or older, refuses write-back. Only the pending-work
  gate is tested at package level; the web endpoint skips an unstamped library.
- 2.5: MP3 and AAC start offsets are put back on write (65 ms lands at 65 ms). Only the merge rules and the refusal
  for unreadable audio are tested.
- 2.5: a cue reaches the OneLibrary cue table; pad slots A to H; a macOS stick with `.PIONEER`. OneLibrary write-back
  is tested for genre, comment and label only.
- 2.5: a stick that hangs at insert holds only itself.
- 2.6: a stale FAT entry on a real FAT32 or exFAT volume is found before a pass creates anything. Only the ownership
  predicate (`TestIssue321FATChainOwned`) is tested; the loop-device tests were not ported.
- 2.6: Engine does not move the commit counter between a cut window and the next (an assumption, design section 5).
- 2.7: the MP3 grid fix and `dbhealer` find the bridge's rows by the snapshot. The `dbhealer` tests still build the old
  state file, and the MP3 grid tests do not create a snapshot.
- 2.7: write-back never runs on a lazy detach, a pulled stick or a read-only view.

Gaps inside rows that have a test, so the row is not listed above: whether the insert after a write-back is taken for
an unchanged export is not asserted (2.5, hot cue row); a blanked `Information` row put right (GL-010) and the
read-count bounds of #321 (40 untouched tracks read 0, at most 2 MiB) were not ported; an Eject during the final pass
failing visibly within 3 s with every file whole is not tested beyond the CLI's give-up (2.5, eject row).

## 3. The deck's limits

| Limit | Number | Where it comes from |
|---|---|---|
| CPU | The deck is about 6 times slower than the ARM build host for this code. A cached-key read of UNTITLED's OneLibrary by the armhf binary takes 128-136 ms on the build host and 742-830 ms at `cpu.max` 18 % of one core; the deck's eject pass measured 785 ms for that step. | replica calibration; the old estimates assumed 3 times (`update.go:100-110`) |
| Reading the exports | Device Library 279-354 ms, OneLibrary 742-830 ms with the key cached, at deck speed. Deriving a OneLibrary key costs about 4.7 s on the deck, once per power-on (the key cache in `/run` is shared by all processes). | replica; `rb-incident-1123.md` §2.1 |
| Engine's own load | About 75 % of one core while it analyses. The gate runs the playlist-only re-export a second time at half the CPU. | brief |
| The stick | USB 2.0, FAT32 on a spinning disk: 10-15 ms a read, about 11 MB/s. The replica delays every read and write request by 12 ms. | deck journal 11-23; `rbupdate-253` |
| The mount's time | The hook gives the whole pre-mount 20 s and keeps 3 s for the visible mount. `rb-bridge` gets what is left of 17 s after the hook's own steps, as `ENGINEX_HOOK_REMAINING_MS`, and is killed at that instant. Past 20 s Engine shows "USB loading timeout". | `edisksd_hook.c:63`, `:1860`, `:1917` |
| Engine and `m.db` | Engine never sees what another process writes to a library it has open, and writes its own pages over it. So the bridge writes `m.db` only before Engine sees the stick. | handbook §3.13; sandbox 2026-11-17 |
| The remount | Only when no deck holds a track of the stick, no peer deck reports one, Engine is not copying a track, and no audio read was served for 30 s. | `internal/cli/rbbridge_remount.go` (kept) |
| Engine's writes | Each save of a loaded track moves `Track.lastEditTime`; write-back runs 5 s after the last one. | #321 (kept) |

What a window plans with: one clock, the hook's deadline. A window of the hook ends at least 3 s before the kill
(`spare`): before each step that grows with the library (reading the exports, the first library, rows and lists, row
updates, taking the deck's rows into a next library) it compares `work.cost()` with the time left and leaves what
does not fit to the job. The costs are the replica's, per byte of export, per track, per list entry and per commit
(`TestWorkCostAgainstTheReplica`). Inside a step the last commit starts 3.75 s before the kill (`commitReserve`).
Further batches of row updates in an insert also stop 4 s after the window began (`insertBudget`), so the DJ gets the
stick. The upkeep at a window's end (queued edits, the MP3 grid fix) runs under its own deadline, half a second in
the insert of an unchanged stick and a second elsewhere, since the grid fix reads every row of a library it has not
stamped.

## 4. Scenarios

| Scenario | In the window | In the background | How it reaches Engine |
|---|---|---|---|
| First insert of a never-seen export | Stat; start the job; read the libraries; build `m.db` on the deck's disk from the exports alone (rows, lists, the export's own cue rows); copy it to the stick, one rename; snapshot. | Every track: stat, audio header, ANLZ, into the stage; `.rgb` files for finished tracks; then the remount is asked for. | Rows and lists at the insert when the first library fits the window (KDATA, 5,962 tracks: yes), else with the job's next library and one remount. Cues, grids, waveforms and covers with one remount: written in place when one window can write them all, else in a next library that the window swaps in. On a stick without room for a next library: in place, 512 tracks a transaction, what does not fit at the next window. |
| The same export again, import complete | Stat of the export files and one line of the snapshot. | Nothing (the job is not started). | Nothing changes. |
| The same export again, import not complete | As a re-export with an empty diff: read, diff. An insert applies no staged data. | The job goes on where the stage is. | As above. |
| A re-export that changes only lists | Read; diff; the changed lists are written (a new list: one row and its entries; a changed list: its entries), in one transaction; snapshot. | The job compares every track's files with the snapshot (stat only) and marks the stamp verified. No remount. | The lists are there at the insert. |
| A re-export with new or changed tracks | As above, plus rows for new tracks (the export's fields and cue rows), changed fields for changed rows, moves for files that moved, removal of rows whose track left the export. What does not end 3 s before the kill by the cost model is left to the job. | Staging of the new and changed tracks only, then the remount; rows and lists a window left, in a next library first. | Rows and lists at the insert, or with the next library's remount; their performance data at the remount. |
| A hot cue set on the deck | Nothing. | stream-sync's watcher, 5 s after Engine's last write: the changed rows only; the cue goes into the track's ANLZ files and into every library that lists the track; `NoteWriteBack` records the new export stamps so the next insert is not taken for a re-export. | rekordbox reads the stick. |
| Eject | The hook runs the eject pass (12 s at most) before the unmount, through stream-sync, which has the keys and what it read. The hook stops the job. | - | - |
| Power cut or pull, any step | See section 5. | | |
| Two libraries on one stick (#327) | Both are read (about 1.1 s at deck speed with the key cached). Tracks are joined by path. The root shows three folders (merged, Device Library, OneLibrary), or one view at the root with the setting. | A track is staged once. | A cue edit belongs to the track, whichever folder it was loaded from; write-back writes it to both libraries. |
| Read-only and HFS+ sticks | The hook decides how the stick is mounted (read-write, or read-only with a writable library view on the deck). The window writes where `Engine Library` is writable and does nothing otherwise. | The job reads the stick and writes the stage on the deck; a `.rgb` file needs the library view. | Write-back never runs on a read-only stick. |
| First and second generation | The same Go code. The differences are the hook's: how HFS+ is mounted, and that second-generation decks get the hook from `/data`. | | |
| An export too big for a window, never seen (a first library that cannot be built 3 s before the kill: by the cost model above about 7,000 tracks when the OneLibrary key is not cached, 12,000 when it is) | Stat; start the job; nothing else. Engine gets the stick without an Engine library and shows its own view of the export when the stick has a OneLibrary. | The job reads the exports, builds the library as a next library and asks for the remount (20,000 tracks: 13 s at deck speed, 50,000: 30 s). Then the fill; all data goes into a second next library. | Rows and lists at the first remount, cues, grids and waveforms at the second. Each remount's window is two renames. |
| A re-export too big for a window (reading it and comparing it with the snapshot does not end 3 s before the kill, or its rows and lists do not) | Stat; start the job. The old library stays on show. | The job reads, diffs, copies the library into a next library, writes the change there and asks for the remount. | One remount switches to the new rows and lists; the data of new tracks follows in place when one window can apply it, else in another next library. |
| Staged data one window cannot apply | Nothing. | After the fill the job writes all of it into a next library. | One remount (KDATA: 2 remounts in all instead of 36). |
| The deck writes the library while a next library waits | The window takes the rows Engine wrote, then renames. | The job publishes those tracks' data again. | The DJ's edits are in the library after the swap. |
| A library imported by an older build | Taken over once: row and list IDs, paths and the digests of what the import wrote are read from the old state database into a snapshot; every track is pending. The old state and marks are removed after the first commit. | Every track is staged again under the current rules. A row the deck changed keeps its grid, cues and loops. | Performance data is refreshed at the remount. |

## 5. States and crash safety

A stick's state is what its files say; nothing else is stored.

| State | Recognised by | A window does |
|---|---|---|
| No library | no `m.db`, or Engine's skeleton (no lists, no state of the bridge) | build and install |
| Imported, work pending | snapshot whose `Pending` is not empty | read, diff (usually empty), apply staged data |
| Complete | snapshot with the current stamp and empty `Pending` | stat only |
| Export changed | snapshot with another stamp | read, diff, apply |
| Older build's library | `.rb-bridge-state.db` and no snapshot | adopt, then as "work pending" |
| Next library ready | `.rb-bridge-snapshot.swap` and `.rb-next.db` beside `m.db` | take the rows Engine wrote since, rename both into place, nothing else |

What a cut at each point leaves, and what the next window does (`TestMountCutAtEveryStepEndsInTheSameLibrary` and,
for the swap, `TestSwapCutAtEveryStepEndsInTheSameLibrary` copy the stick at every one of these points and require
the same library afterwards as without the cut):

| Cut | On the stick | Next window |
|---|---|---|
| While the library is built | Nothing: it is built on the deck's disk. | Builds again. |
| Build done, before the install's rename | A next-snapshot and a temp file; no `m.db`. | Removes the temp file, builds again, overwrites the next-snapshot. |
| After the install's rename, before the promote | `m.db` and the next-snapshot. | The library's commit ID equals the next-snapshot's: it becomes the snapshot. |
| In an in-place transaction, before the commit | `m.db` with a hot journal; the next-snapshot. | SQLite rolls the journal back when the window opens the library. The commit ID differs: the next-snapshot is dropped. The diff runs again. |
| After the commit, before the promote | `m.db` committed; the next-snapshot. | Commit IDs are equal: promoted. Nothing is written twice. |
| After the promote, before staged records are dropped | Applied records still in the stage. | They are applied again; the row already holds them, so nothing is written, and they are dropped. |
| Between a commit and its `.rgb` files | Rows without their list waveform file. | The job writes a missing or invalid `.rgb` for every finished track; a valid one is never replaced. |
| While the job writes the stage | A record's files without its index line, or a torn index line. | The record does not exist; the track is staged again. |
| While the job writes the next library | `.rb-next.db`, perhaps with a journal; no swap snapshot. Its own snapshot and next-snapshot are on the deck. | Windows do not look at it. The job's next run settles it by the commit ID as for `m.db`, or makes it again. |
| Next library ready, before the first rename | `m.db`, its snapshot, `.rb-next.db`, the swap snapshot. | Swaps. |
| Between the two renames of a swap | `m.db` is the next library; the old snapshot; the swap snapshot; no `.rb-next.db`. | The library's commit ID is the swap snapshot's: the second rename is done now. A swap snapshot with another commit ID and no `.rb-next.db` is a stray and is removed. |
| While the snapshot is replaced | Old or new snapshot, whole (the replace order of #321: the old file is held open until the directory and the new file are synced). | Either is consistent with a committed `m.db` through the commit ID. |

The commit ID is `Information.lastRekordBoxLibraryImportReadCounter`, which earlier builds also added one to at every
import. One assumption is not proven here: that Engine does not move this counter itself between a window that was cut
before its commit and the next window. If it did, a next-snapshot could be promoted without its commit; the rows it
names would then be missing, which `reconcile` (every window compares the snapshot's rows with the library's IDs and
paths) removes from the snapshot, so the tracks get rows again. Lists are checked the same way.

Rows Engine or the DJ removed are found by the same comparison. A row another part of EngineX gave another spelling
of its path (#322) keeps its row; the snapshot takes the spelling.

## 6. Tests and the gate

- Unit tests: `pkg/rbstate`, `pkg/bridge` (view, plan, mount, fill, write-back), `pkg/enginedb` (`BuildTrack`,
  encoders), `internal/cli`. Section 2 lists each requirement with its test, and the old tests that only pinned a
  mechanism that is gone.
- The command line the hook runs is what the tests of `internal/cli` and the replica run, with the hook's variable
  set. No test may call the package entry with a deadline the hook does not use.
- The gate is the replica (`job-after.sh` in the lane's tools, to be moved under `tools/dev`): the UNTITLED data
  (2,416 tracks in OneLibrary, 2,320 in the Device Library, 7,252 ANLZ files, 8,779 covers) on FAT32 behind a 12 ms
  delay, the audio files with their real sizes behind the same delay, the armhf binary at 18 % of one core, a kill
  at 17 s. It prints one line a step with wall time, exit status, read and write counts, and the window's own step
  lines. Steps: first insert; the same stick again before the job ran; the job and its remount windows until
  nothing is pending; two re-inserts; a one-list re-export with a warm and with a cold key cache and at half the
  CPU; a re-export with 30 new tracks, its job and remount; a re-insert; an eject pass with nothing to write. KDATA
  (5,984 tracks, 6 deck cues) follows for write-back: the 6 cues on the right tracks, nothing else changed.
- Limits of the replica: the delay device serves requests in parallel, so a large write costs one delay, not its
  size at 11 MB/s; each step therefore prints the KiB it wrote. The audio evidence holds the first 512 KiB of each
  file, so about 5 % of the tracks (headers at the end of the file) are built without analysis there.

Real-deck test steps (the PM; read-only checks except the inserts):

1. Push `go:rb-bridge`, `go:stream-sync`, `edisksd` (the hook's file name changed) and `ui` (the setting) to both
   decks; check the digests.
2. Insert UNTITLED (an older build's library): the journal shows `library of an earlier build taken over` and one
   `rb-bridge: mount <step> <ms> ms` line a step; the lists show the three folders within the window.
3. Wait for `fill of … publish=true` and the remount; `mount staged … applied`; load a track: cues, grid and
   waveform are rekordbox's, and Engine does not analyse it.
4. Eject and insert again: `mount stat` only, `Bypassed`.
5. In rekordbox, add one playlist and export; insert: the new list is there; `mount diff` shows one list to write;
   no remount follows.
6. Export with a few new tracks; insert: rows and lists at once; after the remount their waveforms.
7. Set a hot cue on a deck; after 5 s the write-back line; eject; open the stick in rekordbox 7: the cue is on that
   track in both libraries. Then the same with the stick's `exportLibrary.db` put back from the backup, to learn
   which file rekordbox reads the cue from (section 7, question c).
8. Pull the stick during step 3's remount window; insert again: the library is whole, the journal shows either
   `cut short after its commit … taken over` or nothing, and the stick is not read-only.
9. The second deck plays from the stick over Engine Link during steps 3 to 6: no dropout, no "pool is disposed".
10. KDATA on the right deck: the 6 deck cues reach the right tracks, nothing else changes.

## 7. Evidence for the open questions

**a. Is the gap between the two libraries in the file or in our parser?** In the file. An independent reader written
from crate-digger's DeviceSQL description and `rekordbox.ReadPDB` return the same rows for both copies of UNTITLED's
`export.pdb`: 2,320 tracks, 159 lists, 2,709 entries on 11-23; 2,340, 160 and 2,729 on 11-24; the same set of track
paths, no unreadable row, no entry without its track or list. Against OneLibrary (2,416 tracks, 185 lists, 2,845
entries) by case-folded NFC path: 2,316 tracks in both, 4 only in the Device Library, 100 only in OneLibrary, and
none of the 100 has a file of the same name in the Device Library. Neither library is a superset of the other. This
is what #327 reported and what the final decision (join by path) assumes.

**b. Can rekordbox 7 write a stick with no `export.pdb`, or a stale one?** Both libraries of one stick do differ in
practice: UNTITLED as above; KDATA has 46 tracks and 18 lists only in the Device Library and 22 tracks and 11 lists
only in OneLibrary (#327), and its `exportLibrary.db` carries a 1980-01-01 time. Today's UNTITLED export wrote
`export.pdb`, `exportExt.pdb` and `exportLibrary.db` with one time, so one export does write both. A stick with
only OneLibrary is not among the evidence. The design does not depend on the answer: one library is used as it is,
two are joined, and no file time chooses.

**c. Which file does rekordbox 7 read deck cues back from?** Not proven: no test in the repository has put a stick
back into rekordbox (#271's checklist row 266 is still "pending real-deck run"). What the evidence shows:

- rekordbox 7's own exports keep cues in the ANLZ files (`PCOB`, `PCO2`). On KDATA as rekordbox left it, 14 tracks
  had cues in their ANLZ files and OneLibrary's `cue` table had 0 rows; another real rekordbox 7 export in the lab
  has an empty `cue` table too.
- OneLibrary's `content` rows have `hasModified` and `cueUpdateCount` (pyrekordbox documents the second as "cue
  update count"). Two KDATA tracks had `cueUpdateCount = 1` before any EngineX write-back, so a player had reported
  cue changes that way.
- The Device Library holds no cues at all.

So write-back keeps writing all three places a cue can be read from: the ANLZ files, OneLibrary's `cue` table, and
the two counters; the Device Library gets only the metadata it holds. Whether the encrypted writer is needed is
decided by step 7 of the deck test. Its cost is paid only on a pass that has a changed cue to write.
