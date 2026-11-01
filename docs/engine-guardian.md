# Engine Guardian (Engine OS 5.0.4 / schema 3.0.2)

`engine-guardian` uses the pure Go `modernc.org/sqlite` driver. It does not require
CGO or a device-side SQLite executable. Firmware findings are documented in
`re-report/FIRMWARE-BUGS-AND-DEBLOAT.md` and `re-report/ENGINE-OS-KNOWN-BUGS.md`;
binary layouts come from section 7 of `re-report/ENGINE-5.0.4-ANALYSIS.md`.

## Build and use

```sh
go test ./... -v
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o bin/engine-guardian-armv7 ./cmd/engine-guardian
bash -n rootfs-overlay/usr/local/bin/sc5000-debloat

engine-guardian --heal-mount /media/USB
engine-guardian --heal-mount /media/USB --max-sessions 50
engine-guardian --status --heal-mount /media/USB
engine-guardian --watch --mount-root /media --interval 10s --quiet-for 5s
```

One-shot commands emit a JSON array and return nonzero for a failed library.
Watch mode emits one JSON object per attempt. `--status` always opens databases
read-only and does not repair anything. `--watch --heal-mount DIR` watches a
specific volume. Discovery checks at most two levels under `--mount-root`, never
walks audio directories, and retries failures. SIGINT/SIGTERM stops polling.
Changes to m.db, hm.db and their WAL/journal sidecars reset the quiet period;
unchanged libraries are not healed again. Quiet time reduces contention but is
not a guarantee that Engine has stopped writing: SQLite transactions and a 3s
busy timeout arbitrate concurrent access. For an initial large-library repair,
run the one-shot command while Engine is stopped.

## Repair behavior

- A quick integrity check and an explicit 3.0.2 schema check precede writes.
  Unknown schema versions produce an error; databases are never recreated.
  Each m.db/hm.db repair, trigger/index installation and cache refresh commits
  together; both databases receive the full repair suite. JSON repair counters
  combine both libraries.
  hm.db pruning is a separate transaction, followed by VACUUM only after deletion.
- Chains preserve surviving topology. Heads are visited by ascending ID;
  disconnected cycles start at their lowest ID. All rows survive. Playlist
  parent cycles/dangling parents are rooted at 0. Root title collisions gain a
  deterministic `[recovered ID-N]` suffix. NULL tails become 0. Existing sibling
  uniqueness constraints stay enabled; temporary NULL links are transactional.
- `MaterializedPlaylistPath` stores paths, parent/next pointers and positions.
  `PlaylistPath` transparently reads it while clean. O(1) dirty triggers detect
  edits; an ordered live traversal serves current paths until the next refresh (the original GROUP_CONCAT order is also unstable).
  The original vendor view is retained as `GuardianOriginalPlaylistPath` for
  inspection. Cache positions use breadth-first depth and linked sibling order.
- qCompress parsing checks the declared length, zlib checksum, trailing data and
  a 32 MiB decode budget. Invalid overview blobs receive a 3099-byte, three-channel
  placeholder with 1024 logical bins and one extra bin. This is not a waveform
  derived from the audio. `GuardianReanalysis(trackId,field,reason)` records
  fields needing actual device-side analysis; no WaveformBuilder is invoked by
  this standalone tool. `reanalysis_pending` counts queued fields. Unknown
  track/beat formats are preserved and queued. Cues and beatData are never
  overwritten, and isAnalyzed's undocumented bit mask is never guessed.
- Loop slots use the documented uncompressed mixed-endian format. The adjusted
  beatgrid takes precedence, with piecewise interpolation across tempo changes.
  There is no stored beat-count field: only loops starting within about 1 ms of
  a beat boundary, and whose durations fit a 1/128-beat grid within that tolerance,
  are reanchored. All labels, colors, flags and unset slots survive. Unknown
  layouts and free loops are preserved. Playback-time phase accumulation inside
  the closed-source Engine executable cannot be changed by this database tool.
- Artwork larger than 1024 pixels on either axis or 1 MiB is converted to JPEG
  within 512x512, maintaining aspect ratio. SHA-1 hashes are updated in AlbumArt
  and referencing Track rows. Corrupt images and images exceeding the 64-megapixel
  decode budget are preserved with warnings. External Artwork files are not
  rewritten; the database blob remains the fallback source. Dangling albumArtId
  references become NULL, preserving albumArtSourceHash for external artwork.
- History supports listId/historylistId references and recognized session date
  columns, with descending ID as a fallback/tiebreaker. It retains 50 sessions
  by default. Tracks referenced by any independent trackId table, including
  playlists/preparation lists, survive with their PerformanceData. Unsupported
  history structures fail before deletions. The supplied reports lack the full
  hm.db DDL, so tests cover explicit supported fixtures rather than claim a
  verification against an unavailable real history database.
- Read-only media is inspected without repair. Pending WAL/journal data requires
  recovery on writable media first; immutable reads never silently ignore it.
  Tuning sets synchronous=NORMAL, mmap_size=128 MiB and temp_store=MEMORY without
  forcing journal_mode=WAL. No journal tuning is attempted on read-only media.

## Overlay integration

Copy the overlay into a **writable staging root**, install the ARM binary as
`usr/local/bin/engine-guardian`, then run:

```sh
sh rootfs-overlay/usr/local/bin/sc5000-debloat --root /path/to/staging-root
```

This patches the original engine/prerequisite scripts, fixes `shift1`, disables
explicit automatic `az01-update --erase-data` calls, installs the cleanup link,
limits core dumps and volatile journal size, and adds telemetry hosts entries.
It preserves existing hosts entries and does not erase downloads or stems.
Persistent downloaded music should live under `/media/sc5000-sd-cache/Downloads`;
the vendor's remaining encryption/storage behavior is not replaced here.

The overlay enables both systemd units through relative wants links. The engine
drop-in runs runtime tuning before startup, and `sc5000-debloat.service` repeats
it after the vendor prerequisites. Runtime mode never rewrites the rootfs.
Audio IRQs use CPU 0/FIFO 99, GPU CPU 1, USB/network CPU 2, and guardian runs with
idle CPU/I/O scheduling on CPU 3. Missing hardware utilities are tolerated;
gigabit negotiation failure falls back to 100 Mbps. Wi-Fi power saving is
turned off on service execution. Networking reconnections may require repeating
runtime tuning; network manager ownership is not changed.

Hosts entries block known inmusicbrands.com/PostHog endpoints. The staged
`dnsmasq.d` file additionally blocks subdomains **only on images actually using
dnsmasq**; installing this file does not change the resolver or start dnsmasq.
Disabling core dumps avoids the vendor crash-upload path; it does not replace
all Reporter call sites in the proprietary binary.

Tests execute the offline patcher twice against a fake root and execute its
runtime block against redirected fake proc files and mocked hardware commands,
including Ethernet fallback and audio scheduling. No hardware/firmware tree is
accessed. Actual SC5000 hardware timing and UI playback need device validation.
