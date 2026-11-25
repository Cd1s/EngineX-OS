# rekordbox stick: the UI hook fills a loaded track's empty row from the stage

Design note for `docs/design/rekordbox-bridge-v2.md` §1.11 item 1 (the PM chose this path on 2026-11-25). Where this
note and the code differ, the code is right.

## The problem

After an insert the background job stages every track (§1.5), but the staged data reaches the stick's `m.db` only at
a later publishing window: no other process may write a library Engine has open. A track loaded before that window
has an empty `PerformanceData` row, so Engine analyses it itself and the DJ gets Engine's grid and no rekordbox cues
(#328). Only a write on Engine's own connection, when Engine reads the row, serves that load at once. The UI hook
already does this for Pro DJ Link tracks (`sc_rbx_fill_track`, `tools/ui-mod/rekordbox_analysis.h`) and leaves a
drive's database alone on purpose; this note adds the drive case. Layers 1-3 of "stock first" cannot do it: Engine
has no setting for it, the data is already prepared once (the stage), and a Go process cannot reach Engine's open
connection.

## Where the hook sees a drive load

Engine opens each mounted drive's library as its own SQLite connection whose main database is
`/media/<drive>/Engine Library/Database2/m.db`. A deck load reads the track row with the prepared
`SELECT t.id, t.playOrder, t.length, t.bpm, t.year, t.path, … FROM Track AS t WHERE t.id = ?` (this copies
`isAnalyzed`) and then `SELECT trackId, trackData, overviewWaveFormData, beatData, quickCues, loops,
thirdPartySourceId, activeOnLoadLoops FROM PerformanceData WHERE trackId = ?`; both bind the id with
`sqlite3_bind_int64`. With `isAnalyzed` 0 or no `trackData` Engine analyses the track. The hook already redirects
`sqlite3_bind_int64` on the 32-bit builds `5.3.0` and `5.3.0-sclive4` (sites per BuildID, prologue and SQLite api
table checked before the patch). The drive fill runs inside that redirect, after the bind, on the first of the two
statements, before Engine steps it: Engine then reads the filled row.

Seen in the sandbox (real 5.3.0 and SC LIVE 4 Engines, 2026-11-25): when Engine auto-selects a newly mounted drive it
reads every row through the same bound Track read (1.5-2.6 s after the mount), so every row staged by then is filled at
the insert, before any load. Browsing a list runs neither statement. A row staged later is filled at its load.

## From the track id to the stage file

1. Drive: `sqlite3_db_filename(db, "main")` starts with `/media/` (a string compare, as today).
2. Anything staged on the deck: a `/data/enginex/rb-stage/<uuid>/index.jsonl` with entries no window has applied
   (cached; looked at again only when a stage file changes). If not: done, no statement and no read of the stick.
3. Library uuid without a statement: the first line of the stick's snapshot (`Database2/.rb-bridge-snapshot`,
   `RBS2 {…"uuid":…}`), read once per connection. No snapshot (not a bridge stick) or no live stage for that uuid:
   done, no statement. Today's guarantee for a drive load (not one statement) stays for every stick with nothing
   staged.
4. The stage's index is kept as a small table of its live keys, read again only when the file changes (it is append
   only: only the new lines are read).
5. One read on Engine's connection: the row's path, `pdbImportKey`, the analysis columns' state and
   `Information.uuid`, which must equal the snapshot's (a stale snapshot can only make a miss).
6. Key: `rbstate.FoldPath` of the path without `../`. For an ASCII path that is the lower-case path, which the hook
   computes exactly; the files are `<sha1 hex of key>.bin` (`RBXA1`), `.rgb`, `.jpg`. For a non-ASCII path (Unicode
   case folding and NFC need tables a libc-only hook does not carry) the entry is the one whose `rel` (the volume's
   spelling) equals the row's path. A row whose export spelling differs from the volume's only in non-ASCII case or
   normalisation is not found: Engine analyses it as today.
7. The entry is the key's last index line with no `drop` after it.

## What it writes, and when not

One exec on Engine's connection, the conditions repeated in the `WHERE`:
`UPDATE PerformanceData SET trackData, overviewWaveFormData, beatData, quickCues, loops = the five blobs of the .bin,
activeOnLoadLoops = the line's "active", thirdPartySourceId = NULL WHERE trackId = <id> AND <the row as read>;
UPDATE Track SET isAnalyzed = 1 WHERE id = <id> AND changes() > 0`. The import mark stays off: the staged data holds
the bridge's start offset already, and Engine shifts a marked row again (2292 samples, MP3). `isBeatGridLocked`, the
rating and every other `Track` field stay as they are.

It writes nothing when:

- the row has `trackData`, `beatData` or an overview: it is filled already (by Engine, a window or the hook);
- the row's cues are not the insert's. The insert leaves either the export's cue rows with the import mark 1, or no
  cues, loops or mark at all. Engine clears the mark at its first load and a DJ edit needs a load, so any other state
  is the deck's: it is left alone;
- `pdbImportKey` is 0 or NULL: an Engine-made row the bridge never writes (#252);
- the stage entry is incomplete: no index line, dropped, `perf` false, `deg` or `missing` set, `sig.v` (Rules) not
  the hook's `5`, the `.bin` missing, cut, over 8 MiB, of another magic, with lengths that do not add up, `Complete`
  not 1, or no `trackData` or `beatData`;
- the database's uuid is not the stage's; the feature `rb-bridge` is off.

## The record, so that a filled row stays rekordbox's

1. The row itself: it holds the staged blobs byte for byte, so `enginedb.WrittenDigest` of the row equals the staged
   record's `wr`, until the first load: Engine rewrites the main-cue tail of `quickCues` at every load (sandbox:
   9920.0 → −1105.0, flag 0; the hot-cue entries stay byte for byte). The stage entry is not dropped by the hook. The
   next window applies that entry: for an unloaded row `ApplyPerformance` takes its first case ("the row holds the
   staged data already"); for a loaded one it takes the export-only case (`mergeFast`), which in the sandbox (an export
   without cue rows) still ended `Written = wr`, `PerfApplied`. With an export that has cue rows that case compares
   the row with the export's cue rows, which do not carry the bridge's start offset: see R2.
2. A journal beside the index, `<stage>/<uuid>/filled.jsonl`, one line per fill:
   `{"id":<Track.id>,"k":"<key>","wr":"<the line's wr>","v":<sig.v>,"w":"<sig.w>","at":<unix seconds>}`. It covers the
   cases where the window does not see the entry the hook wrote (re-staged, dropped as stale, a row the next library
   takes again from the live one). Request to the bridge and write-back owners: a row whose `WrittenDigest` equals
   a staged `wr` or a journal `wr` is the bridge's write (`PerfPrev.Written`), not a deck edit.
3. Cue write-back before that window: the fill moves `lastEditTime` (Engine's trigger), so the watcher compares the
   row with the ANLZ files. The cues are the ANLZ's own, so nothing is written; the proof runs write-back on a filled
   row.

## Cost

- Audio thread: nothing (the bind runs on Engine's library threads; no lock is taken on the audio path).
- Browse and load of Engine's own library, Link and streaming rows, and of a drive with nothing staged: a compare and
  a cached look at the stage on `/data`; no statement. The stick's snapshot line is read once per connection, and only
  while the deck holds a stage.
- On a drive with staged entries: one indexed read per bind of the two statements; the fill only for an empty row
  the stage holds: one `.bin` read from `/data` (about 13 KB) and one UPDATE: 5.5-16.7 ms a row on the build host's
  native armhf (load request to filled row 0.006-0.014 s). Engine's insert read fills every row staged and not yet
  applied by then, one after the other on Engine's library thread: bounded by what a window left in the stage.
- Play: nothing.

## Known limit (first-generation 5.3.0)

On JP07 (5.3.0) a row filled at its load (staged after Engine's insert read) was analysed again by Engine in 4 of 11
sandbox loads, 1.0-1.7 s after the load, with a loaded deck and with an empty one; Engine's grid, overview and track
data then replace the staged ones (hot cues and loops stay). Every hooked read after the fill saw the filled row, so
Engine decides from state taken before the fill: a race in its load path. Rows filled at the insert read (most rows)
were never analysed again, and SC LIVE 4's Engine never re-analysed one. Not handled here.

## 64-bit units

PRIME 4 G2 and SYSTEM ONE run the AZ04 Engine. The bind redirect is A32 code and has no AArch64 site, so they get
nothing new: a track loaded before the publishing window is analysed by Engine as today, and the staged data reaches
it at the next window.

## Requests to the stage owner (PM relays)

- R1: `"id"` (the row's `Track.id` from the snapshot) in each index line: the hook can then skip a row that is not
  staged without a statement, and find a non-ASCII row whose export spelling differs from the volume's.
- R2: a hook-filled row is the bridge's write, never a deck edit: in the window, the next library's mirror and
  write-back, match it by `filled.jsonl` (`id`, `wr`) and compare it with the staged data without the main-cue tail
  Engine rewrites at every load.
