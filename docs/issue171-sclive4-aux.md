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
existing BuildID, the exact **JP21** product code, both verified prologues and
all existing config/Stems prerequisites. JP20, JP21X, JC11S and the other boards
keep their previous configuration and receive neither AUX edit. An unknown
BuildID or a mismatched prerequisite leaves the feature disabled.

At getConfig's existing common return, the JP21 trampoline sets software Line-In
and Mixer Menu. The native channels, native control properties and native UI
controllers are then constructed by Engine.

For each audio block, the routing guard reads channel 3/4's native Line bits.
When both are off, it calls the displaced native function with unchanged inputs.
When either is on, it saves the Mic2/AUX/Line-In input pairs, points both Line-In
sources at the **pre-gate AUX capture pair only when InputSelect is Aux**, and
supplies silence to the legacy Mic2 and AUX processors. All direct consumers
(Master, Booth and headphone Master side) consequently receive silence from
those two legacy inputs. Mic1 is unchanged. In Mic2, unknown or invalid selector
states, the Line-In sources also receive silence. The guard restores every saved
input pointer on return.

Silence comes from Engine's own correctly sized output buffers, cleared before
use; no fixed-size production scratch buffer is needed. The audio guard performs
no allocation, locking, file/network I/O or Planck writes. The selector callback
revokes AUX before a native selector transition and publishes its value after
that transition. ON events do not revoke the source. ON state itself is retained,
so closing both Line switches restores the normal native direct-input behavior.
The native AUX attenuation/ducking stage is bypassed by the pre-gate Line-In
source; the physical input LEVEL and the destination channel controls apply.

The original Input menu presents channels in physical order **3, 1, 2, 4**.
On an empty channel its native `currentInput` can remain `Engine` even while
`Line` is true, and choosing `Engine` then leaves Line enabled. For JP21 channels
3/4 the shipped QML derives the selected label from the real `Line` property and
clears that property on `Engine`, so an empty channel can exit AUX immediately.
The native labels and Input heading use the existing trilingual dictionary.

## Tests and limits of the evidence

`tools/ui-mod/test_sclive_aux.c` runs as native armhf against the real regular
5.3.0 and SCLIVE4 Engines. It verifies stock bytes, unknown BuildID refusal,
all-or-nothing config/site refusal, non-JP21 full config byte equality, both real
A32 displaced prologues (including the callback's PC-relative literal), the
3/4/both/off routing matrix, Aux/Mic2/unknown/invalid selector states, raw-capture
immutability, exact input-pointer restoration and uninterrupted ON events.

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
AUX source pointers, silent legacy inputs, Mic2 denial and unchanged off-mode
inputs. With no real ALSA device setup, the fixture returns before the native DSP
body. This is a source/control integration test, **not a full audio rendering or
hardware-routing test**. A physical SC LIVE 4 is still required for audible CUE,
EQ/Sweep response, clipping, feedback, switching transients and XRUN checks.

The en / zh-Hans / zh-Hant sandbox runs all passed. Each observed 32 off-mode
blocks, 24 channel-3 AUX blocks, 12 channel-4 AUX blocks, 12 both-channel blocks,
18 blocks with ON enabled and a Mic2 denial block, with **zero routing errors**.
Both translated Input headings and selected Line In labels passed.

The native `test_sclive_aux`, existing Stems/Mixstream/AZ04 hook tests, full Go
format/vet/race suite, Python suites (148 packer/UI cases, one protective `/data exists on this host`
skip; nine Stems cases), 18 overlay checks and SC LIVE 4 plus official JC11S bootstrap /
install / uninstall cases passed. The bootstrap script accepts
`BOOTSTRAP_TMP_PARENT` to keep its sandbox in the assigned run.

Both tracked armhf hook copies were rebuilt twice and matched SHA-256
`4c6cb6d4e2748755b6b6a3eddf47fb3d3783e2028f3451eded6228d194be07e7`.
An AArch64 hook build and native AZ04 tests also passed. Engine hook source
SHA-256: `02138aa1284a7ab830532e9f227bf4163c18f0b9fafa779b8149466ea380d1f6`.

## SC LIVE 4 hardware acceptance (English)

1. Start at low Master, Booth and MIC2/AUX LEVEL settings. Keep channels 3/4
   faders down; use headphones first and keep speakers disconnected or very low.
2. Connect a quiet line source to AUX, set the rear selector to **AUX**, and leave
   **MIC2/AUX ON off**. Select **Line In** for channel 3 in the native Mixer Menu.
3. Press channel 3 CUE and turn the headphones fully toward CUE. Confirm AUX is
   audible with the channel fader down while Master/Booth remain silent.
4. Raise the fader slowly. Check channel gain, all three EQ bands, Sweep FX,
   crossfader assignment and meters. Repeat on channel 4, using one channel at a time.
5. With Line In active, toggle MIC2/AUX ON. Confirm there is no doubled dry feed,
   approximately +6 dB jump or extra Booth/headphone Master feed.
6. At low levels, switch the rear selector to MIC2. Confirm channel 3/4 Line In
   receives no microphone signal and no feedback/howling occurs. Switch back to AUX.
7. Turn MIC2/AUX ON off, select **Engine** for both channels, then test the normal
   direct AUX/MIC2 behavior separately at low level. Restart and repeat to check
   initial selector state. Stop if any routing, feedback or level check fails.
