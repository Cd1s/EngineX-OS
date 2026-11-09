# SC LIVE 4 AUX through channels 3/4 (#171)

New, unreleased; independent review and SC LIVE 4 hardware validation are pending.
The implementation enables the native software Line-In channels and the native
**Control Center → Mixer Menu → Channels → Input → Line In** entry. It adds no
second set of AUX buttons. Channel gain, EQ, Sweep FX, fader, crossfader, meters
and PFL/CUE remain in Engine's existing channel graph.

## Verified target and corrections to the analysis

The official SCLIVE4 5.3.0 image used for this work has SHA-256
`bf5489d19d7ebd932b7408ab3e51e080648f557c52b78fe2c515929dc611ce7e`.
Its Engine is **47,265,736 bytes**, GNU BuildID
`b946c3b8d99d32998c8197d5dc905a6bddaa5d2f`, SHA-256
`68cb478d120d2793d5883876abe6eba49a3ca78b80abf041466113f93e1f0d58`.
This is the existing `5.3.0-sclive4` entry in the hook. The supplied analysis
instead names BuildID `498878f45a5daeb42cd7a2fb0372fcbe9647bc6f`; its code
addresses, stack buffers and several property/config names do not apply here.
The image was in the lab's `fw-5.3.0` directory, rather than `fw-5.3.0-gen2`.

| Item | Verified value in the supported Engine |
| --- | --- |
| `audio.hasSoftwareLineIn` | Config `+0x26`, unchanged from the analysis |
| `ui.hasMixerMenu` | Config `+0x4d1`, **not `+0x4c9`**; property registration at `0x15a612c` uses UI base `+9` |
| getConfig trampoline | Existing site `0x159f4a8`; adds the two flags only after all AUX prerequisites pass |
| Mixer input processing | **`0xcbd96c`**, prefix `f4 42 6d e1 f8 60 cd e1` |
| AuxMic2 property callback | **`0xdba454`**, prefix `f8 41 6d e1 ec c2 9f e5` |
| AuxMic2 constructor | **`0x1ba13c0`**, prefix `f4 42 6d e1 00 40 a0 e1`; publish native `+0x88` after construction |
| Callback selector/ON properties | Object `+0x78` / `+0x74`; cached selector `+0x88`, ON `+0x84` |
| Native selector values | `InputSelect.index`: `0 = Mic2`, `1 = Aux`; `unnormalized` is not this enum's API |
| Native Line switches | **`/Engine/Mixer/Channel3/Line`**, **`/Engine/Mixer/Channel4/Line`**, not `/LineIn` |
| Mixer channels / Line bit | Pointer vector `+0x40`, count `+0x18`; each channel's bit `+0x7a` (native store at `0xd09050`) |
| Mic2 / AUX / LineIn3 / LineIn4 objects | Mixer `+0x9c / +0xa0 / +0xa4 / +0xa8` |
| Corresponding input pointer pairs | MixerBuffers `+0xb8 / +0xc0 / +0xc8 / +0xd0`; frame count `+0x140` |
| Native output pointer pair | Channel `+0x2c` → context `+8` → buffers `+0x0c` |

These code sites are in the first ELF LOAD segment, where VA equals file offset.
For example, disassemble `0xcbd96c..0xcbd9a0`, `0xdba454..0xdba4a4`,
`0xd09028..0xd09054` and `0x15a611c..0x15a617c` with an ARM objdump.
The proposed two instructions at `0xcbcef8` would overwrite unrelated code in
this Engine; they are deliberately not used.

## Routing and isolation

The AUX edits are a separate all-or-nothing site group. They require the exact
existing BuildID, the exact **JP21** product code, all three verified prologues and
all existing config/Stems prerequisites. JP20, JP21X, JC11S and the other boards
keep their previous configuration and receive none of the AUX edits. An unknown
BuildID or a mismatched prerequisite leaves the feature disabled.

At getConfig's existing common return, the JP21 trampoline sets software Line-In
and Mixer Menu. The native channels, native control properties and native UI
controllers are then constructed by Engine.

For each audio block, the routing guard reads channel 3/4's native Line bits.
When both are off, it calls the displaced native function with unchanged inputs.
When either is on, it saves the Mic2/AUX/Line-In input pairs, points both Line-In
sources at the **pre-gate AUX capture pair only when InputSelect is Aux**, and
supplies silence only to the legacy **AUX** processor, only in AUX mode. This
prevents an extra dry AUX feed to Master, Booth and headphone Master side.
**MIC2 and Mic1 keep their native inputs, ON gates and output routes.** In Mic2,
unknown, invalid or in-progress selector states, the Line-In sources receive
silence; both legacy input pairs keep their native values. The guard restores
every saved input pointer on return.

Silence comes from Engine's own correctly sized output buffers, cleared before
use; no fixed-size production scratch buffer is needed. Explicit volatile stores
avoid a first-block `memset@plt` resolver/loader lock. The audio guard performs
no allocation, locking, file/network I/O or Planck writes. The constructor hook
actively reads and publishes the completed native selector cache at `+0x88`,
even if restoration emitted no property callback. The initial value is the native
Mic2 default (0), rather than an unknown (-1) latch. The selector callback revokes
AUX before a native selector transition and publishes its value afterward.
ON events do not revoke the source. ON state itself is retained,
so closing both Line switches restores the normal native direct-input behavior.
The native AUX attenuation/ducking stage is bypassed by the pre-gate Line-In
source; destination channel controls apply. Verify the physical MIC2/AUX LEVEL
response during hardware acceptance.
Both channels receive the **same** AUX source. Raising both faders can sum it;
use one channel at a time unless that doubling is intentional.
Input selection changes routing at a block boundary without an added gain ramp.
Turn direct AUX ON off and lower the channel fader before changing sources; clicks
and the physical selector's analog/control latency remain hardware acceptance items.
The frame count and matching output-buffer capacities are native Mixer invariants;
the guard does not invent a second capacity bound.

The original Input menu presents channels in physical order **3, 1, 2, 4**.
On an empty channel its native `currentInput` can remain `Engine` even while
`Line` is true, and choosing `Engine` then leaves Line enabled. For JP21 channels
3/4 the shipped QML derives the selected label from the real `Line` property and
clears that property on `Engine`, so an empty channel can exit AUX immediately.
The native labels and Input heading use the existing trilingual dictionary.
On JP21 the visible choices are **Engine** on channels 1/2, and **Engine / Line In**
on channels 3/4. Unverified Bluetooth/Computer choices are hidden. The QML keeps
an explicit visible-to-native index map, including icons and translated labels;
other boards retain their full native list.

Enabling the native capabilities also exposes the Control Center **Mixer Menu
(Beta)** tile. The native `!hasMixerMenu` Preferences gates hide Mic-to-Booth,
Mic-to-headphones, Mic FX, fader curve and Computer Mode Decks entries there;
their controls must be checked in the native Mixer Menu (Channels, Crossfader,
Microphone) during hardware acceptance. `GlobalAssignmentConfig.hasSoftwareLineIn`
also allows the Load key to load a track on a Line In channel. Check the selected
source, label and audible output after Load.

## Tests and limits of the evidence

`tools/ui-mod/test_sclive_aux.c` runs as native armhf against the real regular
5.3.0 and SCLIVE4 Engines. It verifies stock bytes, unknown BuildID refusal,
all-or-nothing config/site refusal, non-JP21 full config byte equality, all three real
A32 displaced prologues (including the callback's PC-relative literal), the
3/4/both/off routing matrix, Aux/Mic2/unknown/invalid/in-progress (-2) selector states, raw-capture
immutability, exact input-pointer restoration, unchanged MIC2, uninterrupted ON
events, short/null graph fallbacks, missing capture and aliased silence pairs.
A constructor test restores AUX without emitting any property event; a second
thread holds a native selector transition while 10,000 audio-side snapshots
verify revocation until publication. Native armhf UBSan is also run.

`tools/az01-packer/sclive-aux-sandbox.sh` unpacks the official image into a private
sandbox and runs the real armhf Engine. The QML probe instantiates the **shipped
native InputSelect component** with the real native channel controllers and
invokes its actual option signal. It checks flags, native Line properties,
channel 3/4 switching, ON, Mic2 and return to Engine, plus the translated heading
and input label in en, zh-Hans and zh-Hant.

The sandbox-only `sclive-aux-hardware-fixture.c` supplies the missing SoC identity
predicate at `0x3a6794`; both its BuildID and prefix are checked. This fixture is
never part of the data bundle or either shipped hook. It captures the real Mixer
allocation by its native vtable, injects distinct synthetic AUX/Mic2 samples and
observes the native entry **after the shipping routing guard**. It verifies the
AUX source pointers, AUX-only dry silence, unchanged MIC2, Mic2 denial to
Line In, and unchanged off-mode inputs. With no real ALSA device setup, the fixture returns before the native DSP
body. This is a source/control integration test, **not a full audio rendering or
hardware-routing test**. A physical SC LIVE 4 is still required for audible CUE,
EQ/Sweep response, clipping, feedback, switching transients and XRUN checks.

## R6: real Engine audio thread and complete DSP

The independent live mode uses `sclive-aux-audio-fixture.c` and a hook compiled
with **`ENGINE_UI_HOOK_AUX_TRACE`**, never either shipped `.so`. The fixture does
not call `0xcbd96c`, invoke its guard, install an audio trap, or skip the DSP body.
It supplies the missing SoC identity and ALSA control-card discovery only, then
redirects the native `hw:0` open to a **real ALSA null PCM** in the private chroot's
`/etc/asound.conf`. The test card constrains real null hw_params to 8 playback /
2 capture channels, S32_LE. Two capture channels are required by JP21's native
`deinterleave32BitMicAndAux` contract; unconstrained null otherwise advertises
2^30-1 channels. All PCM configure/read/write operations use libasound. The
fixture paces writes and injects quiet packed 1 kHz triangle waves at the ALSA
capture boundary. The host's sound configuration and kernel modules are untouched.

The Engine creates and drives its own audio thread. Test counters at the shipping
guard's entry and native return are paired with every ALSA write after priming;
each write must see **exactly one new guard entry and one native return**, on the
same non-main TID. The observed return address is **`0xcabaac`**, following this
native pointer-to-member dispatch:

```text
0xcaba8c: ldr r2, [r3, #12]      ; this adjustment / virtual flag
0xcaba90: ldr r4, [r3, #8]       ; bound member target
0xcaba9c: add r0, r3, r2, asr #1
0xcabaa8: blx r4                 ; enters 0xcbd96c -> production routing guard
0xcabaac: ...                    ; observed LR in every guard entry
```

Thus a scan for direct branches or literal absolute pointers alone is insufficient
for reachability. The existing **`0xcbd96c` site is retained**. The evidence binds
that native caller, non-main TID, block count, complete native returns and real
ALSA writes; it does not infer a live audio callback from a fixture-owned thread.

For the boot case, the TRACE-only construction callback restores the **native enum
data and cached `+0x88` to AUX (1)** before the production constructor hook reads
that cache. It calls no enum setter and emits no selector notification. The QML
probe checks `InputSelect.index == 1`, enables Line In and **never writes the
selector until the later MIC2 test**. The test observes nonzero native Line In
DSP output before that first selector value transition. Native subscriptions may
republish the same starting value; those are not physical/user transitions. The
unit test separately proves publication with **no property callback of any kind**.

The live probe also checks capture/output pointers do not alias and checks the
first two samples of each raw capture channel remain unchanged across the native
DSP call. `capture-alias` counts either a pointer alias or such a sample mutation.
The default offline source/control fixture remains a separate test with its
explicit early-return limitation. Neither fixture is shipped in the data bundle.

Reproduce on Batam, from the build checkout, using a **new** sandbox path:

```sh
X171_LIVE_AUDIO=1 bash tools/az01-packer/sclive-aux-sandbox.sh \
  /root/Oracle-Batam-arm-Paseo/sc5000-a1-20261109/r6-20261110/new-live-run
```

The script builds its trace hook separately and checks all three languages. The
2026-11-10 R6 gate passed both sandbox modes in en / zh-Hans / zh-Hant. Live
reporter snapshots at about 20 seconds (after startup/priming) were:

| Language | Main PID / audio TID | Guard / native returns | Paired blocks | Pair errors | Boot capture / rendered blocks | Capture alias or mutation |
| --- | --- | --- | --- | --- | --- | --- |
| en | 41 / 161 | 1330 / 1330 | 1329 | 0 | 413 / 413 | 0 |
| zh-Hans | 41 / 162 | 1188 / 1188 | 1187 | 0 | 409 / 409 | 0 |
| zh-Hant | 41 / 162 | 1259 / 1259 | 1258 | 0 | 409 / 409 | 0 |

All three had zero main-thread entries, caller `0xcabaac`, 512-frame blocks,
initial native selector 1 and exactly one constructor publication. Paired blocks
exclude the first guard and prefill writes. Each default offline run also passed
`off=32 aux3=24 aux4=12 mic-denied=1 both=12 on=18 errors=0`.

Representative live evidence, truncated:

```text
X171 AUDIO|pid=41|guard=1330|tid=161|main=0|caller=0xcabaac|frames=512|writes=1336|reads=1334|write_tid=161|
X171 AUDIO_PAIR|blocks=1329|errors=0|native-returns=1330|
X171 BOOT_AUX|initial=1|constructors=1|before-transition=413|capture-nonzero=413|render-nonzero=413|capture-alias=0|
X171 AUDIO_DONE|PASS|Engine ALSA thread; one guard/native return per block; full DSP; boot AUX without touching selector|
```

A physical SC LIVE 4 is still required for audible CUE, gain/EQ/Sweep behavior,
analog selector delay, feedback, clipping, switching clicks and XRUN validation;
independent re-review of this follow-up is also pending.

The native `test_sclive_aux`, existing Stems/Mixstream/AZ04 hook tests, full Go
format/vet/race suite, Python suites (148 packer/UI cases, one protective `/data exists on this host`
skip; nine Stems cases), 18 overlay checks and SC LIVE 4 plus official JC11S bootstrap /
install / uninstall cases passed. The bootstrap script accepts
`BOOTSTRAP_TMP_PARENT` to keep its sandbox in the assigned run.

Both tracked armhf hook copies were rebuilt twice and matched SHA-256
`4889bad887c54cf1833e54b0b3531df29e4b79f9f4d418df63516b92bdef82d4`.
The production audio assembly has no PLT calls, allocations, locks, I/O or trace
syscalls. Both test-only trace symbols are absent from its dynamic symbol table.
The AArch64 hook is byte-identical to c47fb54 (SHA-256
`f390adceea5a4e5edfc554c688017526f3041a6248d8628f8497c38e567c9d02`);
native AZ04 tests also passed. Engine hook source
SHA-256: `76ec7f4c74f8d6e0cde1e608010b9d2b3379da9be6a5f883c265a9d38a18c596`.

## SC LIVE 4 hardware acceptance (English)

1. Power off first and set the rear selector to **AUX**. Lower Master, Booth and
   MIC2/AUX LEVEL, keep channel 3/4 faders down, and use headphones first. Stop
   other tracks; keep speakers disconnected or very low.
2. Boot with the selector already at AUX. **Do not move the selector or press ON.**
   Open **Control Center → Mixer Menu (Beta) → Channels → Input**, select channel 3
   **Line In**, enable channel 3 CUE and turn headphones fully toward CUE. Confirm
   AUX is audible immediately with the fader down while Master/Booth remain silent.
3. Raise the fader slowly. Check MIC2/AUX LEVEL, gain, all EQ bands, Sweep FX, crossfader assignment
   and meters. Repeat on channel 4, **one channel at a time**. Both Line In channels
   share one AUX source; raising both faders intentionally sums that source.
4. Toggle MIC2/AUX ON while Line In is active. Confirm no doubled dry AUX feed,
   approximately +6 dB jump, or extra Booth/headphone Master feed. Listen for clicks.
5. At low levels, switch to MIC2 and speak with MIC2/AUX ON enabled. Line In must
   receive no microphone signal; **MIC2 must still use its stock direct route**.
   Check for leakage during switching, feedback/howling and unexpected level jumps.
   Return to AUX. Physical switch/control latency still requires this hardware check.
6. Turn ON off, select **Engine** on both channels, and test normal direct AUX/MIC2
   behavior separately at low level. Verify the Line In label clears even on an
   empty channel. Confirm inputs show only Engine on 1/2 and Engine / Line In on 3/4;
   Bluetooth/Computer must not appear on JP21.
7. Verify the Mixer Menu Beta tile and the settings moved from Control Center
   Preferences: Mic-to-Booth, Mic-to-headphones, Mic FX, fader curve and Computer
   Mode Decks. Check the native Channels/Crossfader/Microphone pages; fail acceptance
   if a previously available setting cannot be reached.
8. With levels low and Line In active, use Load to load a quiet track on that
   channel. Confirm the selected source, label and audible output remain consistent;
   select Engine and verify that the loaded track behaves normally.
9. Restart with AUX already selected. **Leave the selector untouched**, reselect
   Line In and repeat the headphone/fader-down test. Check clicks, clipping and
   XRUNs during normal use. Stop if any routing, feedback, level or settings check fails.
