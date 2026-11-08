# Display-only shadow decks

Base: deployed `origin/wip/laptop-line-20261109` (`f8de9e0`). Integration base
`origin/agent/merge-latest-20261109` was not present at the initial remote probe;
check again before publishing and rebase when available. No deployment is part
of this change.

## Ownership and synchronization

On role 13, only Engine decks 1/3 play; on role 24, only 2/4 play. New default:
the other slots remain empty. Shadow rendering never invokes Librarian load,
Speed, position/seek, cue/loop setters, play, Sync or RequestMaster. Existing
native audio routing is retained.

Cross-Engine SYNC/MASTER already uses **stock Engine Link**. Evidence:
`shadow.go:shadowCompose` clears peer Master with the explicit Engine Link
comment; `build_packs.py:scShadowApply` gates RequestMaster and Sync with
`!e.link`, and `scShadowPost` reads `/Engine/Sync/Network/MasterStatus`.
`/RemoteState` subscriptions were already disabled because registering them
blocked Librarian when a peer disappeared. Display-only does not reintroduce
them. This is code evidence, not a claim that real-hardware phase behavior
has been measured with empty shadow slots. PM must verify native Link across
both real playing role decks, especially 1↔4 and 3↔2.

## Static data

Read the *source* library once analysis is committed: the track's own volume
`Engine Library/Database2/m.db`, or `stm.db` for streaming downloads. SQLite
connections use `mode=ro`, `query_only(1)` and a short busy timeout; no healer,
import, analyzer, pragma journal change or database write runs. Match the loaded
track path, require `isAnalyzed=1`, and bound every blob before decompression.

Reuse `enginedb.QUncompress`. Parse trackData sample rate/count; use Track.key
for displayed key (trackData's enum can differ). Overview is BE u64 logical
count, BE u64 count, BE f64 samples/bin, then three amplitude bytes/bin plus
the terminal triple. beatData has its header and **two** grids of 24-byte
markers; choose the adjusted grid and interpolate its integer beats, including
sparse/negative markers. Never derive a substitute grid from receiver BPM.
Parse quickCues' BE sample offsets/ARGB and loops' LE sample offsets/flags.
Read artwork by AlbumArt hash/BLOB with a strict size bound. QML-posted live
cues override saved cues for unsaved edits.

Important verified schema limitation: this repository's measured 5.0.4/5.3
PerformanceData schema has **no highResolutionWaveFormData column**. A
1024-bin overview is not high-resolution. Native source high-resolution
storage/availability is being independently checked in
`runs/20261109-sc5000-grid/`; any overview-only response must say `detail:false`.
Do not decode the song or silently run an analyzer to manufacture detail.
If a library actually supplies the optional high-resolution column, consume
it read-only with the same validated waveform envelope. Report actual detail
availability, and keep this limitation visible in acceptance evidence.

## Transport and cache

The existing UDP state push and 250 ms HTTP fallback keep position + sample
timestamp, pitch, play/CUE, master/sync, BeatNumber and source identity. Static
wave/grid/cue/cover data must never enter the UDP packet or 120 ms state poll.

Add `GET /peer/decks/{n}/display?key=K`: a versioned bounded JSON object with
source metadata, exact beat positions in milliseconds, cue/loop markers,
base64 three-band bytes, optional high-resolution bands and cover bytes.
The receiver fetches asynchronously once per source identity/deck/key, retries
not-yet-analyzed tracks at a bounded cadence, discards stale completions, and
keeps a bounded in-memory cache. HTTP key mismatch is 409; missing analysis is
404. A legacy peer without the endpoint remains metadata-only, without audio
fallback in new mode. No audio files or .perf sidecars are created.

Typical overview: 3072 raw band bytes (4096 base64); 1,000 expanded beats about
35–55 KB JSON; cue/loops <4 KB; cover <=1 MB; optional detailed bands <=6 MB.
Limit whole response to 12 MB. At 100 Mb/s, 100 KB takes about 8 ms wire time,
compared with copying an entire song. Caches retain only currently referenced
tracks and a small bounded working set; failed requests have negative caching.

Local endpoints: `/v1/sc5000/shadow/display?deck=N&key=K` returns display
metadata and PNG URLs; `/v1/sc5000/shadow/waveform.png?deck=N&key=K` and an
overview selector return cached PNGs; a cover endpoint serves cached cover.
Keys and fresh peer/deck ownership are checked before every response, including
cached responses. Responses use no-store to avoid stale track images.

## Rendering and budgets

Suite renders three-band amplitude columns **once per track**, into <=2048 px
wide textures; never create a texture wider than the device GPU limit. QML
Image items translate/scale within clipped stock waveform/overview positions.
No per-frame Canvas, HTTP PNG request or audio decode. Position advances from
the latest timestamp at 1+pitch/100 only while playing; pause/jump replaces the
sample, and a stale peer freezes/clears its view. Grid lines and cues are QML
items using source millisecond offsets; instantiate only visible grid lines.
Overview, playhead, cover, metadata, BPM/key, bars.beats cue countdown and
MASTER/SYNC are display data, not Engine deck properties.

Targets, pending real-board measurement: <=2 ms GUI work/frame for two shadow
strips, <=3 MB GPU memory per pair of overview textures; steady Go shadow work
<2% of one core, transient PNG generation <100 ms per overview on SC5000.
Detailed textures require bounded tiles and a separately measured memory
budget. Removing receiver audio download, OfflineAnalyzer (~18 s observed),
two decoders/time stretchers and frequent Engine property writes should remove
those costs entirely; total CPU percentage saved cannot be stated honestly
without before/after real-board sampling. No such number is fabricated.

## Dependency migration

| Feature | New mode |
| --- | --- |
| Load Track popup | Continue offering local role decks only; never route load into a shadow slot. |
| Remote shadow Stems | Display-only, no local rendering/import/mute/solo; operate source player. Local role Stems unchanged. |
| Cue synchronization | Source markers in display model; never write receiver Engine cues. Live posted cues update markers. |
| #151 Cue countdown | Source grid and source/live cue positions; disable local-deck countdown on shadow slots. |
| #49 unload/goodbye | Peer empty/expiry removes display/asset refs; no local unload/master handover action. |
| Engine Link SYNC/MASTER | Native Link handles real role decks; shadow badges only reflect source state. |
| CDJ shadow | Display source metadata/state; no synthetic Engine sync/master. Detailed CDJ asset support is separate; never fetch full audio in new mode. Legacy retains previous compatibility. |
| Local/shared source files | Even if receiver can resolve the file, no receiver Engine load. |
| Debug telemetry | Retain bounded opt-in ring/log, add display cache/fetch status; no audio paths needed in display decisions. |

## Old paths and rollback

New default bypasses shadowFetchPeer/CDJ, shadow-v3 audio copying, savePerf,
EXPF replay and scShadowApply's load/Speed/seek/cue/control path. Retain them
behind explicit `shadowLegacyEngine` / `shadow_legacy_engine=1` for rollback;
do not delete users' existing cache files during migration. The setting is
persistent, defaults false, and the UI must require a planned Engine restart
when changing modes so previously loaded legacy slots cannot remain active.
No live restart, unload or setting change on a real deck is authorized here.
PM may revert the branch/build, or enable legacy and restart during deployment.

## Validation and remaining physical checks

All tests/builds run only on Oracle-Batam-arm in the dedicated task directory.
Cover bounded/malformed qCompress and PerformanceData, adjusted sparse grids,
cue/loop endian rules, PNG pixels/dimensions, no-audio fetch on the default
receive path, key changes/expiry/retry, legacy behavior, QML generation and
existing pack tests. Run gofmt, vet, race tests and pack unittests. If a private
Engine sandbox can be created from read-only lab materials, probe generated
QML there using virtio-gpu/eglfs; never reuse or mutate another agent's sandbox.

PM acceptance: both role layouts, real Link SYNC/MASTER and phase, long play
without periodic stalls, stop/CUE/seek/hotcue/loops/pitch, source grid agreement,
streaming vs file analysis readiness, unplug/replug/goodbye, countdown, no
receiver shadow files/OfflineAnalyzer/loaded tracks, three-band appearance,
texture/GPU/CPU budgets, optional high-resolution availability, and planned
restart rollback. Sandbox success cannot replace these measurements.
