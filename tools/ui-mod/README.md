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
The named analysis/cloud/Sentry workers use SCHED_OTHER, nice 10 and
best-effort I/O priority 7. Analysis Queue and other potential lock holders
must make progress under ordinary CPU and I/O load; SCHED_IDLE/idle I/O
cannot guarantee that. Freeze Checker and transient JobController names
are left alone. Named helpers share all online CPUs except EMain/SceneGraph
(1/2), instead of the old single last CPU. The perf-tune sweeper uses the
same helper mask. Pixmap/FileInfo workers retain nice 10 and the helper mask.

Both pthread naming and Qt prctl/__prctl_time64 naming apply these guards;
later pthread_setschedparam calls cannot put named background workers back
into SCHED_IDLE. Naming another pthread targets that thread, never its caller.
Standalone OfflineAnalyzer also starts with SCHED_OTHER/nice 10/BE7, and
Engine cannot promote it to nice -5 or demote it into SCHED_IDLE afterward.
RT audio names/policies are not changed. This is starvation prevention,
not proof of the reported main-thread freeze's cause. See the R2 report.

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
| `Effects::Flanger::process` `0xe3a118` (5.3.0 `0xf1ab24`; shared by Flanger, FlangerPositive, FlangerNegative) | `sc_dsp_makeup_apply`: the stock code mixes 0.7 x dry in place, so the block's RMS is brought back to the dry input's over ~100 ms, unity to +6 dB, ramped per block | #54 |
| `TimeStretchingModifier::Jump` `0x1532fa0` | Non-finite jump target values dropped | H149 R012 |
| `RampDownModifier::ChangeRampTime` `0x1500950`, `MotorizedPlatterScratchControl::RampdownTimeChange` `0xd0b5bc` | Non-finite or negative ramp-down times dropped | LA-036 CF014 LC030 |
| `SamplerControl` StopImmediately `0xc5b2e0`, Stop `0xc5b30c`, Play `0xc5b338` | `sc_dsp_sampler_mark_event`: atomic voice generation tracking | N004 |

`ElastiqueFader` (`0x1534398`) is not hooked: `r1` is the channel count and the
stock code already steps per frame (`step = 1/(rate × 0.001)`), so
`sc_dsp_fader_smooth_block` stays unused. Verified on SC5000 on 2026-11-04 with
hook SHA-256 `e4f30a85…32ee9ba`: all 19 trampolines installed, ALSA
`card2/pcm0p/sub0` `RUNNING`, no XRUN, `NRestarts=0`. That shows the hooks load
and keep audio running; it does not show each pop is gone by ear.

## Master soft limiter (`master_limiter.h`, #58)

Off by default; Settings → "Master Soft Clipper" or `POST /v1/sc5000/ui?masterLimiter=1`
writes `master_limiter=1` to `settings.conf`, which the hook polls once a second
from a SCHED_OTHER thread. It is the 21st DSP site: airHost's device callback
(5.0.4 `0x1462a6c`, 5.3.0 `0xc77934`, 7 arguments, the last three on the stack),
which zeroes and fills Engine's planar float outputs before the resampler and
the `S32_LE` conversion clip them. After the original returns, each stereo pair
of outputs gets a linked limiter. Samples up to -1.5 dBFS are bit-exact; above
that a rational tanh curve bends peaks into a -0.1 dBFS ceiling, and an
instant-attack peak envelope (~100 ms release, no lookahead) lowers the gain
when the input is hotter than the curve's range. The two builds' prologues
differ in the second word and are checked per build with the rest of the group.
Tests: `test_master_limiter()` in `test_dsp_smooth.c`, and the ARM mock in
`test_stems_feature.c`.

## Bluetooth speaker output (`bt_out=`, 5.3.0.5)

Every 5.3.0 Engine already contains a complete A2DP source: a BluezQt `/MediaEndpoint/Source/Sbc`, an `sbc_encode`
thread and `BluetoothSignalSender::audio(this, float *const in[2], int frames)`, switched on by
`Config.audio.hasBluetoothOutput` (`prepare_config` sets it, byte 3 of the audio block) and fed silence (a player
feeds it its deck 1 pair). We add nothing but a feed. Engine calls the sender inside the master callback, on its
thread, before the callback's output exists; so `hook_master_output` keeps the pair `bt_out=<n>` selects (outputs
`2n`, `2n+1`; default 0, which is Master or a player's Layer A, not "off"; a pair the output does not have is pair 0,
and `lim_poll` never stores a negative one) after the callback, and `hook_bt_send`, in the next callback, takes everything kept and hands
it to the sender in calls of at most 512 frames. What waits is a FIFO of stream-rate frames, so one block's length need
not match the next one's, and a block goes once. Calls without outputs (a player makes them too) add and take nothing.
Sender entry per build: Gen-1 5.3.0 `0xf56034`, Gen-2 armhf `0xf629b0`, AZ04 `0x102eb20` (the table's `bt_send`;
5.0.4 has none).

Nothing runs unless a speaker listens. The `sbc_encode` interposer counts the encoder's calls; a master callback that
sees `BT_IDLE` (64) callbacks go by without one treats the speaker as gone: it drops what waits and Engine's own feed
goes to the sender as it always did. A build without the sender hook never captures.

The sender does not resample and sends the data at the rate it arrives, while the SBC stream has the rate BlueZ
negotiated with the speaker: Engine picks 44.1 kHz whenever the speaker offers it (48 kHz only if that is all it
offers). The hook reads the stream rate from the `sbc_t` Engine passes to `sbc_encode` (interposed; a frequency libsbc
does not have is not a heartbeat, so the rate is never 0 while a speaker listens) and **measures the master callback's
own rate**: its frames over the monotonic clock in half-second windows, snapped to 44.1 / 48 / 88.2 / 96 kHz, and a new
value has to come twice in a row, so one stalled callback changes nothing. It runs on every callback, listening or not
(one clock read), so the first speaker is heard from its first block; until the first answer (a second after Engine
starts) nothing is sent, not a guess: a 96 kHz callback taken for 48 kHz doubled the supply and left a second of queue. It must not trust the ALSA device: on a real SC5000 the callback is 225 frames 213
times a second, which is a 48 kHz render, while ALSA is opened at 96 kHz; reading 96 kHz made the first version halve
48 kHz data and send half of what the encoder needs (about 22 kHz of audio into a 44.1 kHz stream: the choppy sound
the first deck test heard). It halves a render of 88.2 kHz or more with an 11-tap half-band, runs a 29-tap low-pass (Kaiser 4.0, 20.5 kHz at 48 kHz: flat
to 18 kHz, -23 dB at 22.05 kHz, -58 dB from 23 kHz) when the stream rate is below that, and a Catmull-Rom interpolation
brings the result to the stream rate, the position carried across blocks. Without the low-pass a 23 kHz tone came out
at 21.1 kHz with 27 % of its level from a 48 kHz render and 15 % from 96 kHz; now it is under 0.1 %. Without the
resampling a 48 kHz feed on a 44.1 kHz stream runs 8.8 % ahead and fills the speaker's buffer.

The sender converts float to s16 with a wrap, and the filters overshoot a full-scale signal (1.08 and 1.15 on a square
wave), so every frame is saturated to +-1 as the last step (a NaN is silence). The filters are FIR: a NaN or an
overshoot in the input leaves them within 43 frames, and nothing needs saturating before them. A new source
(pair), render rate or stream rate restarts the filters with their history set to the new block's first frame and fades
the next 128 frames in from the last frame produced, so a switch or a speaker that connects does not click.

Which pair carries what is per model: players pair 0 and 1 are their two decks, a PRIME 4's 10 channels are Main,
Booth, Headphones (Cue), Zone and a spare, PRIME 2 and Mixstream Pro Main and Headphones; `pkg/webui/bluetooth.go`
names only these and rewrites a saved pair its model has no name for. A single channel of an all-in-one is not on any
output (the mixer's per-channel buffers are not visible to a hook site): it reaches the speaker through Headphones (Cue).
Tests: `test_bt_out.c` (no Engine needed, on a simulated clock: every render rate measured within 1.2 s with the clock
reading off by 300 us, a stalled window ignored, `deck` = 225-frame callbacks at 48 kHz must make 44100 stream frames a
second; its `clip`, `blocks`, `rate` and `source` tests also build against the first version of the feature with
`-DBT_OUT_OLD` (6e62523) and `deck` against the ALSA-probe version with `-DBT_OUT_FB` (fb33354), where they fail;
`test_bt_out realtime`, opt-in and a quarter of a minute, runs the callback and a speaker's encoder on two threads on the wall
clock at 48 / 96 / 44.1 kHz: the rate is found by itself, the encoder does not starve, the queue stays at a few callbacks); sandbox runs with a fake A2DP speaker are in
`docs/REGRESSION-CHECKLIST.md` row 185.

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
