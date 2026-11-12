# Design Note: Denon -> Pioneer CDJ Export Validator & Wire-Format Fixes (#63)

## 1. Problem

`pkg/prolink` (`SyncEngineToPioneerExport`, `DBServer`, `NFSServer`, `Exporter`) translates an Engine DJ library (`Engine Library/Database2/m.db`) and downloaded streaming tracks into a Pioneer DeviceSQL database (`PIONEER/rekordbox/export.pdb`) plus `PIONEER/USBANLZ/.../ANLZ0000.{DAT,EXT,2EX}` analysis files so Pioneer CDJs on the LAN can browse and play media plugged into an EngineX deck (`prolink_export`, default `Off`).

While our own `rekordbox.ReadPDB` and `rekordbox.ParseAnalysis` readers accepted the generated files, real Pioneer players (`CDJ-3000` firmware 3.20, `CDJ-2000NXS2`) and Rekordbox enforce stricter DeviceSQL page-chain, track-row, ANLZ cue/section, and MP3 timing rules:

1. **`export.pdb` DeviceSQL page structure**: `buildPDB` started each table directly at a data page (`page_flags = 0x24`), set `empty_candidate = 0`, and terminated `last_page.next_page = 0`. Real Rekordbox DeviceSQL tables open with an index page (`page_flags = 0x64`, `u16le(0x20) = 0x1fff`, `u16le(0x22) = 0x1fff`) and link `last_page.next_page` to a dedicated blank `empty_candidate` page (`< next_unused_page`).
2. **`export.pdb` track row fields & artwork foreign keys**:
   - `string[7]` (`autoload_hot_cues`) was empty `""` instead of `"ON"`, preventing CDJs from auto-loading Hot Cues A–H when loading a track.
   - `file_type` (`b[90..92]`) was `0` instead of Rekordbox's audio format code (`1` MP3, `4` M4A/AAC, `5` FLAC, `11` WAV, `12` AIFF).
   - `artwork_id` (`b[28..32]`) reused `t.ID` instead of deduplicated `1..N` artwork IDs and wrote unverified `ArtworkPath` references on deferred background jobs when `AlbumArt` lacked valid image data.
3. **`#178` reverse-direction MP3 start-offset alignment (`Engine -> rekordbox`)**:
   - `internal/audioheader.ReadMP3` establishes `StartOffsetSamples = Rekordbox sample - Engine sample` (`1152`/`576` samples for a leading `Xing`/`Info` frame plus `encoder_delay + 529` samples when a `LAME`/`Lavf`/`Lavc` extension is present; `0` for `VBRI`, headerless MP3, WAV, FLAC, AIFF).
   - `rb-bridge` (`Rekordbox -> Engine`) subtracts `StartOffsetSamples`, but `SyncEngineToPioneerExport` (`Engine -> Rekordbox`) exported raw Engine sample positions without adding `StartOffsetSamples` back, shifting MP3 beatgrids, Hot Cues, Memory Cues, and Loops `26–52 ms` early on Pioneer players.
4. **MP3 audio frame count & `PVBR` seek table**:
   - `ANLZ0000.DAT` lacked `PVBR` (`1620` bytes) with `u2 = audio_frames * samples_per_frame` where `audio_frames` skips leading `ID3v2` tag(s), skips a leading `Xing`/`Info` frame, and excludes trailing `ID3v1` / non-MPEG bytes.
5. **`ANLZ` cue list chaining & companion sections**:
   - `cueSections` chained `order_first` / `order_last` on Hot Cues (`kind == 1`) instead of setting `0xffff` (only Memory Cues `kind == 0` are chained, with `memory_count = len - 1`), omitted `PCP2` beat-loop `LoopNumerator` / `LoopDenominator`, omitted `.EXT` `PQT2`, and omitted `.DAT` monochrome overview previews (`PWAV` 400-column + `PWV2` 100-column).

## 2. Stock Capability Check

Stock Engine OS imports Rekordbox USB sticks locally (`rb-bridge` / stock importer) and links Denon units over Engine Link, but provides no Pro DJ Link `dbserver`, `NFSv2` export server, or `export.pdb` / `ANLZ` generator. Existing `pkg/prolink` (`prolink_export`, default `Off`) is EngineX's dedicated opt-in implementation.

## 3. Options Considered

1. **Validate exports using only `rekordbox.ReadPDB` and `rekordbox.ParseAnalysis`**:
   - Insufficient: `ReadPDB` intentionally tolerates missing index pages, `empty_candidate = 0`, empty `autoload_hot_cues`, and `file_type = 0` so EngineX can read third-party sticks leniently. Writers must be held to strict CDJ wire rules, while readers stay lenient.
2. **Pull in external GPL Rust crates or shell out to a separate checker binary**:
   - Unnecessary and adds cross-compilation bloat: a clean-room Go validator in `pkg/prolink` runs in-process at the end of `SyncEngineToPioneerExport`, in `/api/prolink/diag`, and across unit/integration tests with zero external dependencies.
3. **Chosen — Independent strict Go validator (`pkg/prolink/export_check.go`) + wire-format & `#178` reverse-offset fixes in `internal/audioheader` and `pkg/prolink`**:
   - Clean-room MIT Go implementation verifying DeviceSQL page chains, row string offsets, foreign-key/file existence, `.DAT`/`.EXT`/`.2EX` section invariants, MP3 frame counts, and `#178` round-trip timing.

## 4. Chosen Design

1. **`internal/audioheader.CountMP3AudioFrames` / `OpenMP3FrameCount`**:
   - Reuses `skipID3Seeker` and `decodeMPEGHeader` to scan MPEG Layer III frames after leading `ID3v2` tags, skipping a leading `Xing`/`Info` frame (matching Rekordbox's frame counter) and stopping cleanly at trailing `ID3v1` (`TAG`) or padding. Returns `(frames, samplesPerFrame, MP3Info, err)`.
2. **`#178` Reverse-Direction Offset in `pkg/prolink/engine_bridge.go`**:
   - In `readEngineAnalysis`, inspect MP3 files via `audioheader.OpenMP3` (and frame count for `PVBR`). Convert `StartOffsetSamples` into `rate`-scaled Engine samples (`offsetSamples = float64(mp3Info.StartOffsetSamples) * rate / float64(mp3Info.SampleRate)` when rates ever differ, or exact `float64(mp3Info.StartOffsetSamples)` when equal).
   - Shift `engineBeats`, `engineQuickCues`, and `engineLoops` by `+offsetSamples` when converting Engine sample positions to Rekordbox milliseconds, keeping BPM untouched and preserving exact `Rekordbox -> Engine -> Rekordbox` round-trip symmetry (`±1 ms` integer rounding).
3. **`export.pdb` & `ANLZ` Fixes in `pkg/prolink/engine_bridge.go`**:
   - `buildPDB`: emit a proper index page (`0x64`, `0x1fff`, `0x1fff`) at `first` and a dedicated blank `empty_candidate` page linked from `last_page.next_page` for every table; deduplicate `Artwork` table rows (`1..N`); set `string[7] = "ON"` (`autoload_hot_cues`) and `file_type` (`b[90..92]`) from file extension; verify artwork existence before recording `ArtworkPath`.
   - `writeANLZ` / `cueSections`: un-chain Hot Cues (`order_first = 0xffff`, `order_last = 0xffff`), set `PCOB` `memory_count = uint16(len-1)` on non-empty memory cue lists, compute `PCP2` `LoopNumerator`/`LoopDenominator` on integer-beat loops, emit `PVBR` + `PWAV` (`400` cols) + `PWV2` (`100` cols) in `.DAT` (when applicable), and emit `PQT2` in `.EXT` while preserving `.EXT` `PWV5` and `.2EX` `PWV7`.
   - Bump `currentStamp` prefix from `"analysis-v3|"` to `"analysis-v4|"` so existing generated exports automatically upgrade on next sync.
4. **Independent Validator (`pkg/prolink/export_check.go`)**:
   - `CheckPDBBytes(file []byte) (*ExportCheckReport, error)`: independently parses DeviceSQL header, strict index/data/empty page chains, non-zero non-overlapping 21-slot track string offsets, `"ON"` hot-cue autoload flag, nonzero known `file_type`, and referential integrity across `Artists`, `Keys`, `Artwork`, `Playlists`, and `PlaylistEntries`.
   - `CheckANLZFiles(dat, ext, twoEx []byte, wantPath string, isMP3 bool) error`: independently checks `PMAI` bounds, `PPTH` UTF-16BE path equality across all three files, `PQTZ` monotonic beats (`1..4`, valid BPM), `PVBR` presence/size on MP3s, `PCOB` hot-cue unchained `0xffff` vs memory-cue `0..n-1` chain, `PCO2`/`PCP2` UTF-16BE alignment and loop bounds, `PQT2` header in `.EXT`, and waveform section geometry (`PWAV`, `PWV2`, `PWV5`, `PWV7`).
   - `CheckExportDir(root string) (*ExportCheckReport, error)`: runs `CheckPDBBytes`, verifies every referenced audio file, artwork JPEG, and `.DAT`/`.EXT`/`.2EX` triple exists inside `root` without symlinks, and validates every completed track's ANLZ triple.

## 5. Risks & Mitigations

- **Real Rekordbox USB preservation**: `SyncEngineToPioneerExport` continues to check `!GeneratedExport(root)` before touching `PIONEER/rekordbox/export.pdb`; a user's own Rekordbox stick is never modified.
- **Engine `m.db` read-only safety**: `m.db` is opened strictly with `mode=ro&_pragma=busy_timeout(500)`.
- **Backward compatibility with `rekordbox.ReadPDB` and `sim_cdj3000_test.go`**: `ReadPDB` already skips non-data pages (`p[27]&0x40 != 0`) and stops at `n == last` before `empty_candidate`, and `ParseAnalysis` stores `PVBR`/`PQT2` in `Unknown` while keeping `.EXT` `Waveforms` (`PWV5`) and `.2EX` `Waveforms` (`PWV7`) unchanged.

## 6. Test Plan

1. **`internal/audioheader` tests**: MP3 frame counting skipping multiple `ID3v2` headers, `ID3v2.4` footers, leading `Xing` and `Info` frames, and trailing `ID3v1` (`TAG`) / junk bytes.
2. **`pkg/prolink` validator negative & positive tests (`export_check_test.go`)**:
   - Prove `CheckPDBBytes` rejects missing index pages (`0x24` at `first`), broken `empty_candidate` links, zero string offsets, missing `autoload_hot_cues = "ON"`, dangling playlist/artwork IDs, and overlapping row heaps.
   - Prove `CheckANLZFiles` rejects chained hot cues in `PCOB`, mismatched `PPTH`, non-monotonic `PQTZ` beats, inverted loops, and missing MP3 `PVBR`.
   - Full `Engine -> Pioneer` export fixture with WAV, FLAC, headerless MP3, VBRI MP3, Xing MP3, and LAME MP3 (`StartOffsetSamples > 0`), nested playlist folders, UTF-16 CJK/emoji titles & cue labels, loops, and SHA-1 artwork, verified by `CheckExportDir` AND `SimCDJ3000` NFS + `DBServer` browse/load!
3. **`#178` round-trip symmetry test**: `Rekordbox -> rb-bridge (Engine m.db) -> SyncEngineToPioneerExport (Pioneer ANLZ)` on LAME/Xing/VBRI/headerless MP3s + WAV, proving beatgrids, Hot Cues, Memory Cues, and Loops round-trip within `±1 ms` while WAV/VBRI/headerless stay at `0` offset.
