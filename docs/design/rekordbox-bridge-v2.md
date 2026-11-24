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
m.db rows the deck changed ──WriteBack──▶ ANLZ + every library holding the track; then NoteWriteBack ──▶ snapshot
```

Three stores, each with one writer:

| Store | Where | Writer | Readers |
|---|---|---|---|
| Snapshot `.rb-bridge-snapshot` | on the stick, beside `m.db` | the mount path (and `NoteWriteBack`, under the volume lock) | mount path, fill job, write-back, shadow display, dbhealer, MP3 grid fix, the hook (existence only) |
| Stage `<library uuid>/` | on the deck, `/data/enginex/rb-stage/` | the fill job | the mount path |
| Write-back state `.rb-bridge-synced` | on the stick, beside `m.db` | write-back | write-back |

Rules that hold everywhere:

1. The live `m.db` is written only inside a window, in place, in short transactions. It is never copied, replaced by
   a copy or merged with a copy. The one exception is a library the stick does not have yet (no `m.db`, or Engine's
   empty skeleton): that one is built aside and renamed into place.
2. Everything the bridge writes to `m.db` is idempotent and keyed by path. After each commit the snapshot is saved. A
   cut between the two repeats the same writes at the next window.
3. The background job never writes `m.db` or the snapshot. It writes the stage in `/data`, and `.rgb` files for
   tracks it has finished (they are files Engine reads on demand; today's `mirrorPrioritizedRGB` does the same).
4. A window does work in this order and stops at its budget: rows, lists, staged performance data (priority tracks
   first), artwork. What is left stays pending for the next window.
5. A row the deck has changed is never overwritten. "Changed" means the row no longer holds what the bridge last
   wrote (`Track.Written`), decided per row when staged data is applied, with the field rule of #328.
6. Identity is the key. A source ID is used only together with its library (`SourceRef`), and only to follow a file
   that moved.

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
	SyncedName   = ".rb-bridge-synced"   // write-back's own state, same directory
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

File format of the snapshot: text, one record per line.

```
RBS1 {header as JSON}
L {list as JSON}            one per list
T {track as JSON}           one per track
E <records> <crc32 of all bytes above>
```

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
```

On disk: `<root>/<uuid>/index.jsonl` (append only, last line of a key wins, a torn last line is ignored) and one
`<sha1 of key>.bin` per track. The `.bin` file is the `RBXA1` layout the UI hook already parses
(`tools/ui-mod/rekordbox_analysis.h`): the magic, then `trackData`, `overviewWaveFormData`, `beatData`, `quickCues`,
`loops`, each a little-endian uint32 length and its bytes, then one byte for `Complete`. The other fields are in the
index line; `RGB` and `Art` are files beside it (`.rgb`, `.jpg`). `Put` refuses when `/data` has less than 256 MiB
free. A stage not touched for 30 days is removed by `OpenStage` of another library.

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
  further. The lane keeps its own "what the stick's files hold" state in `.rb-bridge-synced` (its format is the
  lane's; today's `BridgeSynced` content).
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
| `internal/cli/rbbridge_stage.go` (299) | deleted with the stage; `rbbridge_import.go`, `rbbridge_remount.go`, `rbbridge_status.go` kept |

Marks and files that disappear from the stick: `.rb-bridge-state.db` (and its journal and `.compact`),
`.rb-bridge-stamp`, `.rb-bridge-pending`, `.rb-bridge-reread`, `.rb-bridge-import/` with `BASE`, `BASE.counts`,
`BASE.times`, `KIND`, `RESUME`, `QUICK`, `fast-base.db`, `.bulk`, `.rb-bridge-publish/READY`, `m.db.before-import`,
`.rb-bridge-overview`, `.rb-bridge-rgb-verified`, `.rb-bridge.lock`. What remains: `.rb-bridge-snapshot`,
`.rb-bridge-synced`, and write-back's recovery marks.

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

What a window plans with: one clock, the hook's deadline. Work the window owes (rows, lists) stops 2.5 s before the
kill (`commitReserve`: the commit under way, the snapshot, the exit). Optional work (staged data, covers) also stops
4 s after the window began when the window is an insert (`insertBudget`), so the DJ gets the stick; the window of a
remount the job asked for has nobody waiting and uses all its time.

## 4. Scenarios

| Scenario | In the window | In the background | How it reaches Engine |
|---|---|---|---|
| First insert of a never-seen export | Stat; start the job; read the libraries; build `m.db` on the deck's disk from the exports alone (rows, lists, the export's own cue rows); copy it to the stick, one rename; snapshot. | Every track: stat, audio header, ANLZ, into the stage; `.rgb` files for finished tracks; then the remount is asked for. | Rows and lists at the insert. Cues, grids and waveforms at the remount's window, a batch of 64 tracks a transaction, the tracks a deck asked for first; what does not fit waits for the next window. Covers after that. |
| The same export again, import complete | Stat of the export files and one line of the snapshot. | Nothing (the job is not started). | Nothing changes. |
| The same export again, import not complete | As a re-export with an empty diff: read, diff, apply what is staged within 4 s. | The job goes on where the stage is. | As above. |
| A re-export that changes only lists | Read; diff; the changed lists are written (a new list: one row and its entries; a changed list: its entries), in one transaction; snapshot. | The job compares every track's files with the snapshot (stat only) and marks the stamp verified. No remount. | The lists are there at the insert. |
| A re-export with new or changed tracks | As above, plus rows for new tracks (the export's fields and cue rows), changed fields for changed rows, moves for files that moved, removal of rows whose track left the export. Rows are added while the window has time; the rest is pending and the lists that hold them are completed later. | Staging of the new and changed tracks only, then the remount. | Rows and lists at the insert, their performance data at the remount. |
| A hot cue set on the deck | Nothing. | stream-sync's watcher, 5 s after Engine's last write: the changed rows only; the cue goes into the track's ANLZ files and into every library that lists the track; `NoteWriteBack` records the new export stamps so the next insert is not taken for a re-export. | rekordbox reads the stick. |
| Eject | The hook runs the eject pass (12 s at most) before the unmount, through stream-sync, which has the keys and what it read. The hook stops the job. | - | - |
| Power cut or pull, any step | See section 5. | | |
| Two libraries on one stick (#327) | Both are read (about 1.1 s at deck speed with the key cached). Tracks are joined by path. The root shows three folders (merged, Device Library, OneLibrary), or one view at the root with the setting. | A track is staged once. | A cue edit belongs to the track, whichever folder it was loaded from; write-back writes it to both libraries. |
| Read-only and HFS+ sticks | The hook decides how the stick is mounted (read-write, or read-only with a writable library view on the deck). The window writes where `Engine Library` is writable and does nothing otherwise. | The job reads the stick and writes the stage on the deck; a `.rgb` file needs the library view. | Write-back never runs on a read-only stick. |
| First and second generation | The same Go code. The differences are the hook's: how HFS+ is mounted, and that second-generation decks get the hook from `/data`. | | |
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

What a cut at each point leaves, and what the next window does (`TestMountCutAtEveryStepEndsInTheSameLibrary`
copies the stick at every one of these points and requires the same library afterwards as without the cut):

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
