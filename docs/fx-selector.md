# PX effects in the native FX selector

This branch targets Engine 5.3.0 armhf, BuildID `be537e908121e76164ddbbf9730d472ba0b8f9d4`, on JP07 and JC11. The JP07 rootfs reports `hasDJFx=false` and `hasNHFx=false`; it has no native FX selector or PX menu. Every board where the PX hook is installed bypasses legacy master Color FX immediately, before its first selector request. The API removes old `pioneer_fx=` lines before reporting selector availability and refuses to save them again. Other builds/boards retain their legacy preference. AZ04 compiles the original master DSP source in `pioneer_fx_legacy.h` and retains its existing master Color FX behavior.

The selector retains the 26 stock entries and appends Spiral (PX), Pitch Echo (PX), Slip Roll (PX), Dub Echo (PX), and Space (PX). Flanger is the carrier because its process entry is already guarded by an exact build/prefix match, it has native Parameter/Mix controls, and its instances belong to individual FX units. Choosing the original Flanger or another original entry clears the route before native selection.

## Controls and identity

One controller per unit lives in EmbeddedMain. Several rendered DJFx widgets can refer to the same unit; they share that controller. The physical encoder adapter consumes the original decoded JogOutput magnitude, forwards Scripted Plus/Minus/Accept actions, and falls back to the original target when no ready controller exists. These are nonpersistent Planck properties created with createProperty, so the bridge works across QML contexts without relying on a shared JavaScript singleton.

A POST to the existing UI API writes an atomic transient command with a 750 ms expiry. A separate FX mutex rejects overlapping requests without blocking saved settings; cancelled/timed-out unread requests are removed. The heartbeat lives in `/run/enginex-fx-selector.ready` (tmpfs). The C polling worker publishes the route and a matching acknowledgement before QML writes the carrier Type. QML then sends two distinct float Mix challenges, separated by 40 ms, and restores the displayed Mix. Their unit bits are disjoint; both must reach the same native owner in the same command generation. Consumed challenges never reach stock DSP. A normal Mix event completes activation. Missing, reordered, interrupted or ambiguous bindings retain stock output.

The event handler owns a ChannelDJFx. While processing that live event, the hook validates its vtable and looks for the uniquely named Flanger in that owner's bounded EffectCollection. The process hook subsequently compares the actual effect instance pointer and one of the two owner-embedded processor buffer-array addresses; it does not scan the heap, infer ownership from call order, or dereference cached owners on other render threads. The ChannelDJFx destructor invalidates routes and deferred native-Mix restoration before freeing its handlers. Native Type change/switch events also invalidate the binding.

Selector delay banks use explicit DIRTY/PREPARING/READY/LIVE ownership. The ordinary polling worker clears and pre-faults only DIRTY banks; the audio callback atomically acquires a READY bank and releases the old bank after its 256-frame crossfade. It never clears a delay buffer. A rapid selection retains the current mode until a spare is ready (up to one 80 ms poll); without an initial prepared bank, it renders the original carrier. Off/on and enable events invalidate the audio epoch, so a resumed instance starts with fresh delay/roll state. Smoothers flush tiny recursive values and Wet=0 bypasses only after the smoothed Wet drops below 1e-6.

The custom DSP owns Wet/Dry. After recording the physical Mix value, its stock carrier processor receives unity Mix, avoiding a second wet/dry blend. Returning to stock restores the recorded Mix on the original DSP thread, including when selecting the already active stock Flanger produces no Type event. DSP state is fixed storage per unit/instance, with nonblocking atomic ownership, two banks and a 256-frame wet crossfade. Wet=0 preserves every input sample bit. The audio path allocates nothing, locks nothing and calls no libm functions.

The independent Rate event is **not captured yet**. Parameter currently maps time, character and feedback together. The UI's Time/Rate control retains its native display but does not independently change the PX delay. This limitation is intentional and must be communicated to testers.

A timeout can still race a command already being processed: expiry and removal prevent unread stale requests, but do not prove cancellation after the worker has begun applying a reused route. A complete two-phase commit is deferred. Also, the native destructor hook clears cached handlers, but its serialization with a concurrent native Mix restoration still needs hardware/thread evidence; the NULL-owner guard alone does not establish lifetime safety.

## Independently checked armhf details

All addresses below are virtual offsets relative to the Engine PIE base.

| Item | Verified detail |
| --- | --- |
| MIX dispatcher / target | `0xe6a0e8` / `0xe9437c`; float at event `+0x0c` |
| Parameter dispatcher / target | `0xe6a140` / `0xe942c4`; float at `+0x20`, effect name at `+8` |
| Rate dispatcher / target | `0xe6a114` / `0xe94320`; unsigned index at `+0x24`; observed statically, not hooked |
| Change / Switch targets | `0xe8f36c` / `0xe8f1f4` |
| Handler ABI | bound object `+0x0c`, member function `+0x10`, adjustment `+0x14` |
| Owner | ChannelDJFx vptr `0x2b21eec`, EffectCollection pointer at `+0x28` |
| EffectCollection | Effect-pointer vector at `[collection+4, collection+8)` |
| Effect name | pointer at `effect+0x28`, length at `+0x2c`; confirmed by the native lookup/memcmp flow |
| Flanger process | `0xf1ab24`, vtable slot at `0x2b2182c` |
| Process ABI correction | `(self, float *const *buffers, int channels, uint32_t frames)`, in place; the third argument is **not an output pointer** |
| Active processor | owner `+0x290` selects embedded Blend `+0x30` or Space `+0x160`; it is not a Flanger pointer |
| Processor render | `0xe6ab9c` / `0xe6acc4` call `[processor+0x18]` with buffer array `processor+0xf8` and channel count 2 |
| Destructor | `0xe99000`; displaced PC-relative loads are relocated explicitly; deleting destructor `0xe99ba8` calls it |

The earlier RE report mixed file offsets and virtual addresses in some Flanger-vtable references; the RW LOAD segment has a 0x1000 VA/file-offset difference. JP07 and JC11 use the same verified binary. No AZ04 offsets were inferred from these values.

## Reproducible validation

All compilation, testing and sandbox work for this task runs on Oracle-Batam-arm through remote-build, under `/root/build/sc5000-f4-20261109`. The shared `/root/build/sc5000-lab` remains read only. No deck connection is used.

- `bash tools/ui-mod/test_fx_selector.sh`: existing DSP regressions and the new control/mapping matrix, each plain, ASan/UBSan and TSan. The new suite covers all five modes, time/parameter/wet values, finite bounded output, literal wet bypass, mode crossfades, ownership contention, challenge ordering, unbind/destruction and concurrent control/render activity.
- `bash tools/az01-packer/test_fx_selector_sandbox.sh`: private JC11 PRIME4 5.3.0 image, unshare mount/network/PID namespaces, real rootfs and native armhf Engine; no qemu. The UI uses the production Go UI handlers, production hook and generated packs. A private import directory supplies the original assignment modules to the UI engine for the real knob-component probe.
- The sandbox C driver is explicitly **offline**: there is no ALSA device. It captures real native handlers/owners and distinct collections, replays event ABI fixtures, and feeds real Flanger instance pointers through the installed production process hook. It proves instance binding and finite/parameter-sensitive output, not real audio-device queue timing, physical routing or latency. It is built separately and is never part of the production overlay.
- `bash tools/ui-mod/validate_fx_selector.sh` runs the required whole-repository checks: gofmt, go vet, go test -race, az01-packer Python discovery, rootfs overlay test, both DSP sanitizer matrices, native armhf legacy Flanger scope tests, AZ04 master-output golden hashes under both FP contraction policies, and two identical hook builds. Final results and hashes are recorded in the task report/commit message.

## PRIME 4 tester steps

1. Install the PM's matching test image (suite, UI pack and hook from this branch) and reboot. Use a JC11 PRIME 4; AZ04 PRIME 4 G2 is outside this feature.
2. Rotate through the full original list and all five `(PX)` entries. Confirm the selected name persists on the main FX panel; test touch selection and fast physical rotation/presses.
3. Assign FX1 only to channel 1 and FX2 only to channel 2. Choose different PX effects simultaneously and confirm each affects only its assigned channel.
4. Sweep Wet/Dry from zero to full, including a sudden 0.8→0 jump. Verify a smooth fade to sample-exact dry sound at settled zero. The consumed binding handshake should not cause a brief volume duck. Echo modes use dry/wet interpolation: at full Wet, their initial delay interval contains little output until the first echo arrives. Change Parameter and confirm time/character/feedback change. Independent Time/Rate support is pending.
5. Switch quickly among the five PX modes and listen for clicks, clipping or dropouts. Select every stock effect, including stock Flanger, and verify native sound and the previous Wet/Dry position return.
6. Reassign units/channels, toggle FX off/on, reopen views and reboot. Report incorrect labels, cross-unit changes, missing controls or audio glitches with the selected modes/knob positions.

Rollback for review is the source/artifacts at `dfdef97`; nothing has been deployed to hardware. The remote build and sandbox directories are disposable and removed after artifact/report retrieval.
