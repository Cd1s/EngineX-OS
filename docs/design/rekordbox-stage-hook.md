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
`/media/<drive>/Engine Library/Database2/m.db`. A deck load reads the track row by id with the prepared
`SELECT t.id, t.playOrder, t.length, t.bpm, t.year, t.path, … FROM Track AS t WHERE t.id = ?` (binding `t.id` via
`sqlite3_bind_int64`), by path with `SELECT t.id, t.playOrder, t.length, t.bpm, t.year, t.path, … FROM Track AS t …
WHERE t.path = ? [OR t.path = ?]` (binding `t.path` via `sqlite3_bind_text`), and then
`SELECT trackId, trackData, overviewWaveFormData, beatData, quickCues, loops, thirdPartySourceId, activeOnLoadLoops
FROM PerformanceData WHERE trackId = ?` (`sqlite3_bind_int64`). With `isAnalyzed` 0 or no `trackData` Engine queues
`OfflineAnalyzer` (on `5.3.0`, checked at `0xd6b138` before entering analysis queueing at `0xe525ec`).

On the 32-bit builds `5.3.0` and `5.3.0-sclive4`, the hook redirects `sqlite3_bind_int64` (`api[5]`, with
`sqlite3_bind_int` `api[4]` branching into it) and `sqlite3_bind_text` (`api[10]`, prologue `push {lr}; sub sp, sp, #20`
verified alongside the BuildID site table before patching). The drive fill runs inside both redirects (`rbx_bind`
after binding the id; `rbx_bind_text` before binding the text so the path pointer remains valid regardless of SQLite's
destructor) and before Engine steps the statement, so whichever connection reads the row first—by `t.id` or by
`t.path`—sees the filled row atomically.

Seen in the sandbox (real `5.3.0` and `5.3.0-sclive4` Engines, 2026-11-25): when Engine auto-selects a newly mounted
drive it reads every row through the bound Track read (1.5-2.6 s after the mount), so every row staged by then is
filled at the insert, before any load. Browsing a list triggers no fills. A row staged later is filled at its load, on
the path-bound read before Engine's `isAnalyzed` check.

## From the track id to the stage file

1. Drive: `sqlite3_db_filename(db, "main")` starts with `/media/` (a string compare, as today).
2. Anything staged on the deck: a `/data/enginex/rb-stage/<uuid>/index.jsonl` with entries no window has applied
   (cached; looked at again only when a stage file changes). If not: done, no statement and no read of the stick.
3. Library uuid without a statement: the first line of the stick's snapshot (`Database2/.rb-bridge-snapshot`,
   `RBS2 {…"uuid":…}`), read once per connection. No snapshot (not a bridge stick) or no live stage for that uuid:
   done, no statement. Today's guarantee for a drive load (not one statement) stays for every stick with nothing
   staged.
4. The stage's index is kept as a small table of its live keys, read again only when the file changes (it is append
   only: only the new lines are read). The table scanner inspects up to four leading JSON members (`"k"`, optional
   `"id"`, `"sig"`, `"rel"`) so it handles both the current index format and R1's `"id"` field without hiding `"rel"`.
5. On a path-bound read (`sqlite3_bind_text`), the hook folds the bound path and checks the cached stage table under
   its mutex *before* running any SQL: an unstaged track costs 0 SQL statements. Only a staged path (or an id-bound
   read on a staged library) runs one indexed read on Engine's connection (`WHERE t.path = '<escaped>'` via `C_path` or
   `WHERE t.id = <id>`) to obtain `t.id`, `t.path`, `t.pdbImportKey`, the analysis columns' state and
   `Information.uuid`, which must equal the snapshot's (a stale snapshot can only make a miss).
6. Key: `rbstate.FoldPath` of the path without `../`. For an ASCII path that is the lower-case path, which the hook
   computes exactly; the files are `<sha1 hex of key>.bin` (`RBXA1`), `.rgb`, `.jpg`. For a non-ASCII path (Unicode
   case folding and NFC need tables a libc-only hook does not carry) the entry is the one whose `rel` (the volume's
   spelling) equals the row's path. A row whose export spelling differs from the volume's only in non-ASCII case or
   normalisation is not found: Engine analyses it as today.
7. The entry is the key's last index line with no `drop` after it.

## What it writes, and when not

One atomic `SAVEPOINT` exec on Engine's connection, the conditions repeated in the `WHERE`:
`SAVEPOINT sc_rbs; UPDATE PerformanceData SET trackData, overviewWaveFormData, beatData, quickCues, loops = the five
blobs of the .bin, activeOnLoadLoops = the line's "active", thirdPartySourceId = NULL WHERE trackId = <id> AND <the row
as read> AND (SELECT uuid FROM Information) = '<uuid>'; UPDATE Track SET isAnalyzed = 1 WHERE id = <id> AND
changes() > 0; SELECT changes(); RELEASE sc_rbs`. Any error executes `ROLLBACK TO sc_rbs` then `RELEASE sc_rbs`
(falling back to `ROLLBACK` on any non-zero `RELEASE` error) so no partial write (`PerformanceData` filled with
`isAnalyzed = 0`) is ever committed and the connection stays in clean autocommit mode while preserving any outer
`BEGIN`. Both the state read and the atomic write retry `SQLITE_BUSY` (masking extended result codes with `rc & 0xff`)
within a 200 ms `CLOCK_MONOTONIC` wall-clock deadline (5 ms `nanosleep` between attempts, avoiding multiplication of
`PRAGMA busy_timeout`) so concurrent connections on the same load wait for an in-flight fill to commit rather than
reading `isAnalyzed = 0`. The import mark
stays off: the staged data holds the bridge's start offset already, and Engine shifts a marked row again (2292 samples,
MP3). `isBeatGridLocked`, the rating and every other `Track` field stay as they are.

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

- Play / audio thread: nothing (both bind redirects run on Engine's library threads; no lock or I/O is taken on the
  audio path; `test_rt_budget.c` and `test_rt_callback.c` pass with 0 misses on arm64 and armhf).
- Browse and load of Engine's own library, Link and streaming rows, and of a drive with nothing staged: a statement
  string check and a cached look at the stage on `/data`; 0 SQL statements. The stick's snapshot line is read once per
  connection, and only while the deck holds a stage.
- On a drive with staged entries:
  - Path-bound reads (`sqlite3_bind_text`) for an unstaged track: one O(1) hash lookup in the cached stage table, 0 SQL
    statements.
  - Id-bound reads (`sqlite3_bind_int64`) or staged path-bound reads: one indexed read (`Track.id` or unique `C_path`);
    the fill runs only for an untouched row the stage holds: one `.bin` read from `/data` (about 13 KB) and one atomic
    `SAVEPOINT` write: 2.9-13.5 ms a row on native armhf (`late2` runs: 3.1-6.6 ms on JP07, 2.9-13.5 ms on SC LIVE 4;
    0-7 ms before the load request settles, 0.28-0.56 s request to loaded). Engine's insert read fills every row staged and not yet applied by then, one after the other on
    Engine's library thread: bounded by what a window left in the stage.

## Race closure (first-generation 5.3.0, round 2)

In round 1 (id-bound redirect only), a row staged after Engine's insert read was re-analysed by `OfflineAnalyzer` in 4
of 11 sandbox loads on JP07 (`5.3.0`), 1.0-1.7 s after load. Tracing the live `5.3.0` Engine showed that the deck load
path reads `Track` by path (`WHERE t.path = ? OR t.path = ?` via `sqlite3_bind_text`) across multiple SQLite
connections before checking `isAnalyzed` (`0xd6b138`) and queueing `OfflineAnalyzer` (`0xe525ec`), bypassing the
`sqlite3_bind_int64`-only hook and racing with an in-flight fill.

Adding the `sqlite3_bind_text` redirect on `WHERE t.path = ? [OR t.path = ?]`, wrapping `UPDATE PerformanceData` +
`UPDATE Track` in `SAVEPOINT sc_rbs`, and retrying `SQLITE_BUSY` across concurrent connections closed the race:
0 re-analyses in 20 of 20 load-time fills on JP07 (`5.3.0`) and 0 of 20 on SC LIVE 4 (`5.3.0-sclive4`) in the sandbox,
plus full `engine` / `window` / `base` regressions passing on both targets.

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
