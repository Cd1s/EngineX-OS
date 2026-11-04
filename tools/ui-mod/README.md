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

- Three Engine starts in a row that crash within 15 s create
  `/data/sc5000-custom/ui/safe-mode` and the stock UI returns. A deliberate
  stop or restart (language switch, deploy) resets the count
  (`engine-ui-guard stop`, run from `override.conf`).
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

The current ALSA/update candidate is prepared for coordinated deployment. Its
new hooks have not been loaded on the deck by this task; this task does not
restart Engine.

## DSP guard (`dsp_smooth.h`)

`dsp_smooth.h` is header-only C11 for the audio callback: no allocation, no
locks, shared state only through `__atomic` builtins. Include it from this one
translation unit; a second one would get its own, unconnected state.
`test_dsp_smooth.c` covers it:

    gcc -std=c11 -O2 -Wall -Wextra -Werror tools/ui-mod/test_dsp_smooth.c -o /tmp/test_dsp_smooth && /tmp/test_dsp_smooth

The hook places 19 trampolines into Engine's closed DSP and controller code,
all deployed and verified on the deck:

| Entry | Guard | Issues |
|---|---|---|
| `ChannelDJFx` TurnOff `0xd917b8`, TurnOn `0xd917e4`, EffectSwitch `0xd91894`, EffectChange `0xd918c0`, EnabledChanged `0xd918ec` | `sc_dsp_fx_mark_transition`: bump the generation, arm a 128-frame ramp | H145 H163 H164 |
| `ChannelDJFx` Mix `0xd91810`, Parameter `0xd91868` | Events carrying NaN/Inf are dropped, not rewritten to 0 | H154 |
| `Effects::AutoGate::process` `0xe3a598` | `sc_dsp_autogate_pre_process`: phase at `+0x1e8` cleared once per transition | LA-020 H153 |
| FX common mix tail `0x15fa188` | `sc_dsp_fx_common_post_process`: NaN/Inf output replaced by dry or silence, dry→wet ramp while armed | H154 H145 H163 H164 |
| `PlaybackModifier::Nudge` `0x14fff54` | Non-finite values dropped | CF011 (input side) |
| `TimeStretchingModifier::PlaybackTempoChange` `0x153312c` | Only positive finite tempo values pass | H154 RA003 RA004 LA-033 |
| `SweepFx::DubEcho` `0xe3d0a8`, `SweepFx::Wash` `0xe3d908` | `sc_dsp_sweep_fx_post_process`: output NaN/Inf scrub and `[-8.0, 8.0]` clamp | H154 H172 |
| `TimeStretchingModifier::Jump` `0x1532fa0` | Non-finite jump target values dropped | H149 R012 |
| `RampDownModifier::ChangeRampTime` `0x1500950`, `MotorizedPlatterScratchControl::RampdownTimeChange` `0xd0b5bc` | Non-finite or negative ramp-down times dropped | LA-036 CF014 LC030 |
| `SamplerControl` StopImmediately `0xc5b2e0`, Stop `0xc5b30c`, Play `0xc5b338` | `sc_dsp_sampler_mark_event`: atomic voice generation tracking | N004 |

`ElastiqueFader` (`0x1534398`) is not hooked: `r1` is the channel count and the
stock code already steps per frame (`step = 1/(rate × 0.001)`), so
`sc_dsp_fader_smooth_block` stays unused. Verified on SC5000 on 2026-11-04 with
hook SHA-256 `e4f30a85…32ee9ba`: all 19 trampolines installed, ALSA
`card2/pcm0p/sub0` `RUNNING`, no XRUN, `NRestarts=0`. That shows the hooks load
and keep audio running; it does not show each pop is gone by ear.

## ALSA clock and official updater boundary

The official image contradicts the original RA002 assumption: `0x165bcd0`
loads type **2 (MONOTONIC_RAW)** and `0x165bcd8` calls the ALSA timestamp setter.
The second setup at `0x167cdf8` does not explicitly choose a type. Engine's timer
helpers at `0x167bad4` and `0x167bc1c` use clockid **4 (CLOCK_MONOTONIC_RAW)**.
This supports preserving RAW; it is not an end-to-end proof of the Sync Time bug.

The two ALSA interposers only upgrade GETTIMEOFDAY/type 0 to RAW/type 2 inside
Engine. Existing MONOTONIC/type 1 and RAW/type 2 pass through unchanged. Before
sw_params is committed, its type is queried through the real ALSA getter. An
unavailable/failed query preserves the original apply behavior; a failed required
type change returns the real error without committing the configuration. Other
processes retain normal ALSA behavior. No timestamp mode, sample rate, PCM data,
DSP operator, start/stop threshold or silence parameter is changed.

Both native paths explicitly set silence_threshold=0 (`0x165bcc0`, `0x167cde4`).
No silence-size/boundary override is added: filling a ring buffer differently
without establishing its ownership/drain semantics could truncate valid audio.
The ALSA docs describe [silence filling](https://www.alsa-project.org/alsa-doc/alsa-lib/pcm_2pcm_8c.html)
and the kernel distinguishes [the clock domains](https://cdn.kernel.org/doc/html/latest/sound/designs/timestamping.html).
The unit test checks type preservation, call ordering, failure handling and
unchanged sentinel buffer parameters; it cannot establish zero pops or XRUNs.

UpdateServerUrl defaults to `https://autoupdate.airmusictech.com/PrimeUpdates.xml`
(the string reference at `0x386f6c` precedes the property assignment at `0x386f94`).
The Engine-only getaddrinfo guard rejects that exact hostname, case-insensitively
and allowing a final DNS dot. It does not block the inmusicbrands.com apex,
connman, GitHub, or `device.inmusicbrands.com`: the latter also appears in OAuth
refresh (`0xf089b0`) and software-unlock (`0x1a84b94`) code. GitHub OTA UI and its
server remain separate work; vendor XML requests are not redirected to GitHub
JSON. Proxy-side DNS, numeric/cached endpoints and other processes are outside
this resolver boundary.

The historical closed-core/MCU checklist was checked for a library boundary,
not counted as fixed. StopTime maps to `/Client/Preferences/RampDownTime` and
`Config.audio.rampDownTime`; forcing it to zero could abruptly cut audio.
KeyLock (`/Engine/DeckN/Track/KeyLock`) and InstantDouble
(`/Engine/DeckN/InstantDouble/Sync`) expose controls, not a verified DSP repair
interface. FX, AutoGate, sweep FX, tempo, jump, ramp-down and sampler entries
are guarded (see DSP guard above). Reverse reverb uses a fixed 0.55 parameter at
`0xdc0cd4` and ignores KeyLock by design; `LoopModifier` (`0x1522f14`) already
compensates the end-current remainder. The panel ADC and motor torque loop run
on separate MCUs; only their host-side events are guarded. A guard keeps bad
values out; it does not prove a symptom is gone by ear.

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
