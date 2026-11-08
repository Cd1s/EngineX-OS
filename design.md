# Display-only shadow decks

Implementation base: `origin/agent/merge-latest-20261109` (`de0f64d`), rebased
from deployed `origin/wip/laptop-line-20261109` (`f8de9e0`). First design was
published before implementation; this revision incorporates the JP/ID hardware
investigations and PM corrections. No deployment is part of this branch.

## Ownership and synchronization

Role 13 plays only Engine decks 1/3; role 24 plays only 2/4. By default the
other local Engine slots stay empty. Display code never calls Librarian load,
Speed, seek/position, cue/loop setters, play, Sync, RequestMaster or unload.
Even locally accessible copies are not loaded. Existing audio routing remains.

**Engine Link shares Network Master, not each deck's DeckIsMaster.** The old
comments saying it “shares master natively” were incomplete. The PM/ID hardware
investigation found a receiver shadow Deck1 becoming local master, so pressing
SYNC on real Deck2 synchronized against that local copy. Empty shadow slots
remove that competing loaded track. Native Engine Link remains responsible for
cross-machine network tempo/master negotiation; this branch does not implement
a new audio synchronization protocol or write synthetic shadow master state.
Source MASTER/SYNC badges are display data only.

Code evidence: `scShadowPost` reads `/Engine/Sync/Network/MasterStatus` separately
from `/Engine/DeckN/DeckIsMaster`; the legacy control path guards some peer
RequestMaster/Sync operations. `/RemoteState` registration was already disabled
because it blocked Librarian on disconnect. Do not restore it. Required physical
acceptance is that real 1/3 and real 2/4 can follow the intended remote real deck,
including 1↔4 and 3↔2, handover while playing/stopped, and no local empty slot
becoming tempo leader. A sandbox or the old code comments cannot establish this.

## Source data and immutable ownership

Read the loaded track's volume `Engine Library/Database2/m.db`, or `stm.db` for
streaming downloads, using SQLite `mode=ro`, `query_only(1)`, bounded SELECTs and
short busy timeouts. Related metadata uses a read transaction. No healer,
importer, analyzer DB writer, journal-mode change, or library update is invoked.
The source rechecks loaded key/path after reading to reject track switches.

Do **not** filter Track.isAnalyzed. In actual 5.3.0 stm.db it remains zero even
after complete analysis. Readiness means trackData, overviewWaveFormData and
beatData contain nonempty qCompress data. This also fixes the legacy `/perf`
readiness check. Missing analysis is retryable, not an invitation to decode on
the receiver or generate another beatgrid.

Measured formats, confirmed by `runs/20261109-sc5000-grid/README.md`:

| Data | Format and handling |
| --- | --- |
| trackData | qCompress; 44/68 bytes; BE sample rate/count, three peak doubles at 20..44. Display Track.key, whose enum can differ from trackData. |
| overview | qCompress; 3099 bytes = BE u64/u64/f64 header + 1024 L/M/H triples + terminal triple. |
| beatData | qCompress; BE f64/f64/u8 header and BE marker counts; each marker LE f64/i64/u32/u32. Use adjustedMarkers, interpolating sparse source markers without deriving BPM/grid on receiver. |
| quickCues | qCompress; BE counts and sample positions, UTF-8 labels, ARGB. Live source-posted cues override saved markers, including deletion. |
| loops | **Not compressed**; raw header/labels and LE sample positions/flags with ARGB. Render only. |
| artwork | AlbumArt BLOB if present, else 20-byte hash -> Engine Library/Artwork/base64.RawURLEncoding(hash).jpg; validate file size/image dimensions. |

The investigation measured complete single-track reads/decompression in
7.75–8.87 ms on JP07, including a 74.2 MB / 5981-track USB library. Engine used
TRUNCATE rollback journal and held no persistent idle POSIX lock. Those are
observations from the supplied report, not a universal guarantee against future
busy writers; queries still time out and retry safely.

## Actual detailed waveform availability

Engine 5.3 does **not** persist scrolling waveform detail in PerformanceData
or OverviewData/*.rgb. Both disk sources only contain the 1024-bin overview.
The native WaveformBuilderJob decodes on deck load, builds 105 blocks/s into
ITrackData heights/colors, and frees those buffers on unload. A scaled overview
must never be labelled “high resolution”. The optional-column assumption in
the first draft has therefore been removed.

Persisted Rekordbox detail is available from rb-bridge's separate, read-only
`.rb-bridge-state.db` / BridgeAnalysis.sourceJSON: PWV7 is three bands at
150 entries/s; PWV5 is a two-byte RGB/height representation at 150 entries/s.
Reuse the existing Rekordbox conversion and keep source Engine grid/cues.

For other supported files, a **source-only** streaming envelope worker reads
FLAC/MP3/WAV without retaining full PCM, creates three-band 105 Hz envelope
columns with RBJ filters and source peaks, and caches by d.Key. It never runs
BPM/key/grid detection or writes Engine data. One worker runs at a time on a
locked, niced Linux thread, pinned to the last allowed CPU, with 5 ms work /
15 ms yielded-time throttling and cancellation when the track is unloaded or
the suite stops. It has explicit duration/bin/memory/time bounds. The worker
thread exits while locked so its priority cannot leak into the Go thread pool.

Overview becomes usable immediately; `detailPending:true` causes slow metadata
refresh until detail is ready. Unsupported/cancelled/failed detail retains a
clearly labelled overview fallback, not a fabricated detailed waveform.
Generated detail is marked `source-envelope`; it is not claimed to be the
original in-memory Engine buffer or pixel-identical to it. The Hybrid Mode
interface investigation is separately tracked in `re-findings.md`; an unproven
native interface is not required by or silently enabled in this branch.

## Protocol, freshness and caching

Realtime state remains on existing UDP push plus HTTP fallback: position with
sample timestamp, pitch, play/CUE, sync/master, BeatNumber and source identity.
Static data never enters UDP or the 120 ms state response. A new read-only
`GET /peer/decks/{n}/display?key=K` returns versioned JSON, source metadata,
exact millisecond grid/cue/loop data, base64 band bytes and optional cover.
Key mismatch is 409; not-ready data is 404. No audio fallback, audio copy,
shadow-v3 directory or EXPF sidecar is created by the new receive path.

The receiver fetches asynchronously, rejects redirects and oversized payloads,
backs off failures, preserves usable overview during detail retries, and bounds
its cache to four entries. Display keys include source identity and original
source deck, so the same song from another source cannot reuse the wrong grid.
Mapped receiver slots retain original source deck numbers for asset requests.
The dual-deck slot collision bug is fixed: only the same peer **and same original
deck** can reuse an occupied slot. A sibling never overwrites the first track.

A 2 s gap now marks a sample `stale` and freezes extrapolation while retaining
track/assets. The sender also preserves its last state while its GUI stalls,
instead of publishing a false empty list. Only explicit goodbye, a confirmed
empty/ejected track, or an explicit layout/role change clears the display.
The old 2 s timeout + 350 ms unload behavior remains only in the opt-in legacy
renderer. Missing packets alone do not cause a track reload or display teardown.

Local endpoints provide metadata, cached PNGs and cached artwork under
`/v1/sc5000/shadow/`. Role, key and current source ownership are checked on every
request, including cached responses. Cached assets remain available for stale
but retained tracks. Explicit goodbye makes them inaccessible. Responses are
no-store; a state version header lets QML skip parsing unchanged state bodies.

Typical static sizes: 3 KB raw overview / 4 KB base64, 35–55 KB for 1000 expanded
beats, <4 KB cues/loops, and 135 KB PWV7 detail for a five-minute track. Cover is
limited to 1 MB and the whole response to 12 MB. At 100 Mb/s, 100 KB is about
8 ms wire time. Decoder input remains entirely on the source.

## QML rendering and allocation budget

Suite renders PNG amplitude columns once per track/tile, preserving peaks when
reducing resolution. Images use three-band colours. Overview is 1024×64;
detail uses <=2048×128 tiles, at most 65536 columns/32 tiles. Only visible tiles
load into QML. Image items translate/scale inside clipped stock waveform and
overview positions; there is no Canvas or per-frame PNG rendering/request.
Grid and cue positions use source milliseconds, with only visible beat lines
instantiated. Source metadata, cover, loop/cue markers, playhead, Bars.Beats cue
countdown and MASTER/SYNC badges cover the otherwise-empty Engine UI.

40 ms sampling reuses role lists, translator references, two sample buffers,
post objects/cue pools and POST/poll XHR. Receiver samples use two reusable
buffers; live cue comparisons avoid per-frame JSON.stringify/filter/map.
JSON serialization/parsing still occurs when actual network state changes;
that unavoidable transport work is not described as allocation-free. A state
version header avoids parsing unchanged poll replies. Non-shadow scDicts,
scCueTick and scCall remain owned by the other implementation line.

Targets requiring real-board measurement: <=2 ms GUI work per update for two
shadow strips, <3 MB GPU textures for the visible pair, <2% of one core steady
Go shadow work, and <100 ms overview PNG construction. Source envelope work is
transient and throttled toward <=25% of one core. Receiver audio copy,
OfflineAnalyzer (~18 s previously observed), duplicate decoders/time stretchers
and repeated Engine property writes disappear; total CPU percentage savings
must be measured by PM rather than invented from these component changes.

## Dependencies, removed paths and rollback

| Feature/path | Display-only behavior |
| --- | --- |
| Load Track popup / swipe load | Only local role decks selectable; no shadow slot load. |
| Shadow Stems / import / mute / solo | No local rendering/import/control for a shadow slot; operate source player. Local role functionality is retained. |
| Cue synchronization | Live/saved source markers in display model; no receiver Engine cue setters. |
| #151 cue countdown | Uses source adjusted grid and source cue milliseconds, not receiver BPM or empty deck properties. |
| #49 unload/goodbye | Clear only confirmed display state, with no synthetic unload/master handover. |
| Cross-machine SYNC/MASTER | Native Link negotiates network master; real local deck ownership must be physically validated. Shadow flags only paint badges. |
| Shared local file | Still not loaded into receiver Engine. |
| CDJ shadow | New mode is metadata-only for CDJs; no full-audio fetch or synthetic Engine sync/master. Legacy preserves the former CDJ compatibility path. |
| Debug | Bounded opt-in telemetry includes display cache/fetch and source-envelope status. |
| shadowFetchPeer/CDJ, shadow-v3 copies, savePerf, EXPF replay | Bypassed in default receive path; retained behind explicit legacy mode. |
| scShadowApply/Speed/Seek/Cues/Beat/Link/Empty | Early return in display mode. No native deck control or per-frame analyzer work. |

Persistent rollback setting: `shadowLegacyEngine` / `shadow_legacy_engine=1`,
default false. The settings row explains that switching requires a planned
Engine restart to clear any slots from the former legacy mode. Existing audio
caches are retained; discovery no longer deletes old cache directories. PM can
roll back the build or enable legacy and restart during deployment. No live
restart, unload, setting/database change or flash is performed by this task.

## Validation and physical acceptance

All compilation/tests run only in this task's Oracle-Batam-arm build directory.
Coverage includes bounded/malformed PerformanceData, source adjusted sparse
markers, cue/loop endian rules, PNG pixels/tiles, no audio/perf fetch on default
receive, read-only m.db/stm.db with isAnalyzed=0, role/source identity, dual-deck
collisions, jitter/freeze versus explicit bye, QML forbidden Engine access,
buffer/XHR reuse, pack generation and existing UI tests. Final evidence is in
report.md; run gofmt, vet, full pkg race tests and az01-packer unittests. Attempt
a private native Engine QML sandbox using read-only lab materials and virtio-gpu;
never mutate or reuse another agent's sandbox.

PM acceptance: both real role layouts, correct native cross-machine tempo/phase
and master handovers, long play without periodic stalls, pause/CUE/seek/hotcue/
loops/pitch, matching source grid, streaming vs file readiness, visible detailed
versus fallback labels, source job CPU/memory, >2 s sender stalls without clear,
explicit eject/bye and reconnection, source replacement with identical song,
countdown, no receiver loaded shadow tracks/files/OfflineAnalyzer, GPU budget,
and planned-restart rollback. Sandbox success cannot replace these checks.
