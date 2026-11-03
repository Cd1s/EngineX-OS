# Engine UI mod

`engine_ui_hook.c` is preloaded into `/usr/Engine/Engine` and swaps files in
Engine's embedded Qt resources for those in
`/data/sc5000-custom/ui/packs/<language>/`. Rootfs and the Engine binary stay
untouched. `build_packs.py` builds the packs from the QML extracted from Engine
5.0.4: language, waveform palette, separate NetEase/Spotify/Deezer sources and
removal of the improper-eject prompt.

    python3 build_packs.py --qrc <extracted qrc> --out out/ui-packs
    # unpack out/ui-packs.tar.gz into /data/sc5000-custom/ui on the deck

`--dev` adds an eval/screenshot harness; it also needs
`QML_XHR_ALLOW_FILE_READ=1` and must never ship.

`icons/` holds the service marks, rendered from Simple Icons (CC0).

Units in the rootfs `/etc`: systemd reads them before the `/etc` overlay mounts,
so a drop-in only in the overlay's upper layer is ignored at boot. On a live
deck, change that layer offline or in an image; bind-mounting `/` and editing it
while running crashed PID 1 once.

## Limits

- Translation covers text literal in QML (`i18n.tsv`). Text from Engine's C++
  stays English. Track, artist and playlist names are never translated because
  they are not literals.
- TIDAL, NetEase, Spotify and Deezer share Engine's one TIDAL client, so one is
  active at a time. TIDAL itself is passed straight to TIDAL's servers. Switching
  to it after using another service may ask for a new TIDAL login.
  Tracks already loaded on a deck keep working after a switch.
- Audio for streaming tracks comes from a Deemix REST bridge on the deck at
  127.0.0.1:6595, then the deck's NetEase login. See `docs/stream-sync.md`.
- Featured, Genres, Moods and TIDAL Rising are hidden, and My Collection too except
  on NetEase, where it opens saved albums. TIDAL itself shows them all. Encoder
  navigation can still land on those rows; the gateway answers them with empty
  listings.
- Encoder navigation of the Source screen reaches only stock rows. The three
  service rows are touch-only.
- Stems use Engine's own sidebar Stems list, which the hook enables on JP07.
  The pack keeps Engine's Performance Pads bar on and switches a deck's pads
  to STEMS when a stemmed track loads.
- The deck overview is stretched only for tracks whose path has a `Contents/`
  folder (a rekordbox USB export). Streams, Stream downloads and Engine Library
  music keep stock height.
- Jog light modes only set `/Engine/Deck<N>/Color` in memory. Measured
  2026-11-03: 30 s of Psy Fast or Psy BPM Sync wrote no more to eMMC than Static
  and changed no settings, profile or database file. BPM sync follows the playing
  deck's tempo (one beat per step, four beats per colour-wheel turn), not its beat
  phase. This changes the earlier half-beat timing; the new pack needs deployment
  before a deck running the previous version changes behaviour.
- Selecting a drive that carries an Engine profile no longer asks "Load drive
  profile to device?"; the prompt's "No" is taken and the deck keeps its settings.

## Recovery

- Three Engine starts that do not survive 90 s create
  `/data/sc5000-custom/ui/safe-mode` and the stock UI returns.
- To remove the mod: delete
  `/etc/systemd/system/engine.service.d/60-ui-hook.conf`, run
  `systemctl daemon-reload` and restart `engine.service`.

## Remote Stems runtime adapter

All backend offsets below are ELF virtual addresses in the ARMv7 Engine 5.0.4
build with GNU build ID `af073865a3318d807a8303f716152eac923a4c6f`. This executable contains the
player-model branches; these are not independently validated hardware builds. The official SHA-256 is
`e1eb0e2b858880016daa3ad14538426204f8e698d855a274aebc924a5f9a5caa`;
the previously unlocked image is
`342b611bd71fd09d9fda8013f19dccfba47ee4f0f772a959058bdd19d74d5413`.
Constructor boundaries and model-name PC references were recovered from ARM
code and `.ARM.exidx`: JP07 `0xf6586c`, JP08 `0xf663f8`, JP11 `0xf665e0`, JP13
`0xf6709c`, JP14 `0xf67dcc`, JP20 `0xf67fc8`; PRIME 4 has a separate constructor
at `0xf64124`. The JP20 constructor names **SC LIVE 2**, not PRIME 4. No model
constructor is modified by the new Bluetooth/HFS+/processor settings.

An unknown build ID is left untouched. Each edit then accepts original bytes,
already patched bytes, or the explicitly supported old processor branch.
Unknown bytes are skipped; the processor and gateway groups are each checked
as a unit. A memory-protection failure aborts startup. Code mappings return to
RX and their instruction caches are flushed; the Engine file is never changed.

| Site | Behavior |
| --- | --- |
| `0x842644` | Bind only the native StemsPreRenderedPlayback object (`0x29bc83c`) to the native true byte (`0x258e185`), then use the normal enabled-feature registration at `0x8429b0`. Other features replay the displaced load/comparison and resume at `0x84264c`. |
| `0x6d3e00` | Preserve ShowStemsList eligibility across the extra device gate. |
| `0x85ef9c`, `0x85e5f8` | Keep the remote processor path and handshake adapter. |
| `0xf6303c` | After model initialization, set config bytes `+0xdb`, `+0x24c`, `+0x24e`, `+0x276` to 1 (Stems integration, Bluetooth, Bluetooth assignment, remote processor) and `+0x289` to 0 (disableHfsPlus). Replay the two original literal loads and resume at `0xf63044`. |
| `0x1897e70`, `0x1897dcc`, `0x1678644` | Filesystem admission and native hfsplus selection. |
| `0x58af34`, `0x167295c`, `0x16728c0` | Keep the existing read-only library-overlay admission behavior. |
| `0x336740`, `0x336758`, `0x33652c`, `0x1db8e90` | Local Tidal gateway URLs and MP3 extension. |

The old `0x8429b0` Stems patch was sufficient for JP07 but missed the early exit
for configurations set to False. The new single interception runs before that
split. It changes no configuration strings or secure-configuration flag.
`0x85f000` is no longer needed: the shared getConfig hook includes its processor
capability write. The pre-existing JP07 `0xf659b8` capability patch remains.
Bluetooth admission does not add missing Bluetooth hardware or drivers, and
filesystem admission does not make a physically read-only volume writable.

## SQLite, telemetry and worker guards

Engine embeds SQLite 3.45.3, source ID
`2024-04-15 13:34:05 8653b758870e6ef0c98d46b3ace27849054af85da891eb121e9aaa537f1e8355`,
matching the [official release](https://www.sqlite.org/releaselog/3_45_3.html).
The internal `sqlite3_api_routines` table at ELF VA `0x29980a4` (file offset
`0x29970a4`) and the ARM code establish these entries:

| Function | ELF VA | API table slot |
| --- | --- | --- |
| sqlite3_open | `0x1cba858` | 70 |
| sqlite3_open_v2 | `0x1cba874` | 135 |
| sqlite3_exec | `0x1c91f74` | 55 |
| openDatabase (shared internal implementation) | `0x1cba04c` | — |

Both public open entries use eight-byte, checked ARM redirects. Their copied
prologues contain no PC-relative instructions. The original functions still
receive their filename/output pointer/flags/VFS arguments. Telemetry filenames
containing `posthog.db` or `events.db` become `:memory:`. Successful ordinary
opens receive `synchronous=NORMAL`, `temp_store=MEMORY`, `cache_size=-8000` through
the internal exec function. Failed opens preserve their original result and do
not run PRAGMA. `:memory:` connections do not run the tuning SQL. `open16` is not
intercepted.

`open`, `open64`, `openat`, and `openat64` additionally map analytics sidecars
and CrashReportDB files into a private `/tmp/sc5000-engine-volatile-<uid>/`
tree, preserving absolute path identity and handling relative dirfds. `/tmp`
must be tmpfs for this fallback to avoid storage writes. This is an open fallback,
not an overlay for mkdir/stat/rename/unlink or other processes.

Crashpad exec/spawn requests fail with the normal ENOENT result; they never
claim success or invent a child PID. Other exec/spawn calls pass through.
The listed analysis/cloud/Sentry/freeze workers receive SCHED_IDLE, nice 19,
idle I/O, and the last online CPU outside CPU0. Pixmap/FileInfo workers receive
nice 10 and CPUs 1 through the last online CPU. The mask is built from the online
CPU count rather than Engine's inherited CPU1-only startup affinity. Both pthread
naming and Qt's prctl/__prctl_time64 naming apply these guards; later
pthread_setschedparam calls cannot reset idle workers to SCHED_OTHER. Naming
another pthread targets that thread, never blindly applies nice/ioprio to its
caller. Subsequent affinity requests retain these guards. CPU0-only systems
cannot satisfy the CPU guard.
AudioOut/AudioRenderer names are excluded. Freeze/Sentry threads are deprioritized,
not terminated; independent OfflineAnalyzer processes are outside is_engine().

NORMAL still permits journal writes/fsync. The 8000 KiB cache is per connection,
not a total RAM bound, and later application PRAGMAs can override it. Telemetry
`:memory:` databases are connection-local. Unit tests do not prove zero Engine
retries, zero flash writes by all processes, or fixes for CF003/N001/N002/N006.

## Verification and deployment status

Build only on IN in `/root/build/sc5000-hook-universal-20261104`. The ARM tests
link the official SQLite 3.45.3 amalgamation, kept only in that remote directory:

    arm-linux-gnueabihf-gcc -O2 -DSQLITE_THREADSAFE=1 -c deps/sqlite3.c -o deps/sqlite3.o
    arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror -Ideps -o out/test_stems_feature tools/ui-mod/test_stems_feature.c deps/sqlite3.o -ldl -lpthread -lm
    qemu-arm -L /usr/arm-linux-gnueabihf out/test_stems_feature /root/build/sc5000-lab/5.0.4/tree/usr/Engine/Engine /root/build/sc5000-lab/out/Engine.unlocked17

The tests execute installed ARM SQLite redirects and original-function
trampolines, read back all three real PRAGMAs, open/query a real database, check
read-only/error/memory cases, exercise the Stems/config paths, classify every
edit on both binary images, and check actual worker scheduling/affinity/nice/I/O
and caller isolation. Six synthetic config patterns exercise the common layout;
they are not six hardware acceptance tests. Ghidra's full import timed out;
entry identification above uses the API table, source ID and objdump evidence.

This universal backend build is prepared for coordinated deployment. Its new
hooks have not been loaded on the deck and this task does not restart Engine.

The client additionally requires the separately provisioned private
`/etc/sc5000-stems/token` and the LAN backend. Installing the client and seeing
Engine load the hook does not establish that a track can be separated. Full
acceptance requires a real short track, successful `.stems` output, and playback
of each stem. Do not use Engine's "Remove Stems Support" button: the stock
removal path targets the processor path returned by this adapter.

Before deployment, back up the existing hook, `engine-ui-guard`, and Engine
service drop-ins to agent-backup. To roll back, stop Engine, restore those exact
files, unmount `/usr/local/bin` after stopping processes using the overlay,
remove the newly installed helper, and restart Engine. Keep user audio,
databases, and credentials intact.
