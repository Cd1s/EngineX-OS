# FX selector validation — 2026-11-09

Base: `dfdef97e04621e1fc7fc8f4e2f0fc607a00b316d`.
Branch: `agent/fx-selector-20261109`.
Build/test host: the build host, native aarch64 host with armhf compatibility; no qemu and no deck connection.

| Check | Result |
| --- | --- |
| Kernel-native armhf execution | Parent inspected the live child via /proc/PID/exe: the executable itself is ELF32 ARM, not an emulator |
| gofmt -l . | No files |
| go vet ./... | PASS |
| go test -race ./... | PASS |
| Python az01-packer discovery | 136 tests, OK, 1 skipped |
| UI source/behavior subset after final encoder changes | 34 tests, OK |
| test_rootfs_overlay.sh | 18 tests, PASS; mocked storage/hardware |
| test_pioneer_selector | plain + ASan/UBSan + TSan PASS |
| test_dsp_smooth | plain + ASan/UBSan + TSan PASS |
| Native armhf non-PX Flanger | PASS; opaque argument 0/1/2/4/0x12345678, bit-exact original body and makeup |
| Site JavaScript syntax | PASS |
| armhf hook, two builds | Byte-identical |
| AZ04 hook compilation and master golden | PASS; frozen dfdef97 DSP source; all five modes × 20 seconds, both `-ffp-contract=fast` and `off` |

Both committed armhf copies have SHA256:

```text
51c99fdf4dd10c91a95eb50a7d4866a48d5f2df7ef1c15384fc1985ac1e1d8b6
```

Paths: `rootfs-overlay/etc/sc5000/libengine-ui-hook.so` and `rootfs-overlay/usr/local/lib/sc5000/libengine-ui-hook.so`. ELF32 ARM EABI5, stripped; BuildID `72f0707a8ebefb8b8950e54eb0aa92aad3672b00`.

## R5 regressions

Follow-up to review of `9e66cba`, all builds/tests on the build host:

| Review | Fix and regression |
| --- | --- |
| M1 | `sc_pfx_activate` clears pending restore before native override; `test_arm_arm` arms/off/arms without rendering between selections |
| M2 | Wait for smoothed Wet <1e-6 before literal bypass; `test_wet_zero_fade` checks every adjacent sample during each mode's 0.8→0 fade (<0.025), then bit-exact dry |
| M3 | Unverified armhf scope retains the opaque argument/body; `test_flanger_scope`. AZ04 compiles original DSP source; `test_master_golden` checks exact float-word hashes against dfdef97 under both FP policies |
| M4 | Install-time master bypass on every PX-enabled board; API migration preserves unrelated settings; `test_px_master_bypass`, `TestFXSelectorMigratesLegacySettings`, both rootfs probes |
| L1 | Worker-only preparation with atomic bank ownership; `test_worker_banks` checks no unprepared render, no hidden callback clear, deferred rapid change and fresh reset; concurrent routes run under TSan |
| L2 | Flush tiny smoother states; `test_smoother_denormals` |
| L3 | NULL carrier/current-name guards; `test_px_null_owner`. Destructor-versus-restoration lifetime serialization remains unproven |
| L4 | Off/on/enable events increment reset epoch; `test_px_off_on_reset` plus fresh-buffer assertion in `test_worker_banks` |
| L5 | Heartbeat moved to `/run/enginex-fx-selector.ready`; real API readiness exercised by both sandboxes |
| L6 | Dedicated try-lock, 750 ms command expiry and unread-request cleanup; `TestFXSelectorTimeoutDoesNotBlockSettings`. Cancellation after worker application starts remains deferred |
| I1 | Tester documentation distinguishes consumed-handshake ducking from normal wet-only delay startup |
| I2 | Live event ordering, channel assignment, toggles and teardown remain PRIME 4 hardware checks; no deck was contacted |

The JP07 rootfs probe reports `hasDJFx=false`, `hasNHFx=false`: it has no native FX selector. Its supported hook still immediately bypasses old master FX; the legacy menu is hidden and the old key is removed, as requested by R5. JC11 provides the native selector.

## JP07 config probe

```text
engine-ui-hook: PX product=JP07 master-bypass=1 installed
FXCONFIG product=JP07 hasDJFx=false hasNHFx=false
FXCONFIG master-bypassed=true selector=false
FXCONFIG legacy-master-bypass-and-migration|PASS
```

## JC11 sandbox excerpt

Real PRIME4 5.3.0 rootfs, production hook, generated UI pack and production Go API handlers, in private mount/network/PID namespaces. The real private knob component is instantiated using original assignment modules and MIDI configuration fixtures.

```text
FXCONFIG product=JC11 hasDJFx=true hasNHFx=false
FXPROBE legacy-master-menu-hidden|PASS
FXPROBE appended-list|PASS
FXPROBE native-knob-component|PASS
FXPROBE three-plus-one-minus|PASS
FXPROBE unit-bus-isolation|PASS
FXPROBE continuous-zero-no-movement|PASS
FXPROBE continuous-five-step-burst-clamps|PASS
FXPROBE unit1-Mix-wire-bits|PASS
FXPROBE unit1-carrier|PASS
FXPROBE unit1-px-label|PASS
FXNATIVE distinct-collections|true
engine-ui-hook: PX unit=1 mode=1 ... bound
FXNATIVE unit=1 finite+param+wet0|PASS
engine-ui-hook: PX unit=2 mode=2 ... bound
FXNATIVE unit=2 finite+param+wet0|PASS
FXPROBE unit2-Mix-wire-bits|PASS
FXPROBE unit2-carrier|PASS
FXNATIVE stock-mix-restored|PASS
FXNATIVE offline-driver|PASS
FXPROBE stock-flanger-selectable|PASS
FXPROBE done|PASS
FXCONFIG legacy-master-bypass-and-migration|PASS
```

The wire check compares the exact float bits stored by Planck against the acknowledged command. The native driver then replays ABI fixtures into two distinct real ChannelDJFx handler/collection pairs and feeds real carrier pointers with their owner-embedded buffer-array addresses through the installed process hook. Output is finite and nonzero, changes with Parameter, and is bit-identical to input at Wet=0. Selecting stock Flanger restores native Mix.

There is no ALSA device in this sandbox. The fixture unit numbers are assigned by the offline driver; this does not prove the live audio-device event queue, physical unit/channel assignment, end-to-end gain, latency, or listening quality. The bounded Engine run ends via the 40-second timeout (124), so graceful native destructor teardown is not claimed as a runtime result. Destructor unregister logic is covered by the C unit test, and its displaced instructions/offsets were checked statically.

Independent Time/Rate is not wired: Parameter currently also controls time. PRIME 4 hardware verification remains necessary; the steps and verified offset table are in [fx-selector.md](fx-selector.md).

Read-only architecture consultation used paseo-committee: 严IN (Sonnet), agent `815450da-81cc-4b4a-9bd7-71bed88bef11`, verified the owner/collection/ABI chain; 程IN (GPT61), agent `c1592103-ed34-43b8-9003-bdf567262f87`, identified the native Scripted-action registration and encoder bridge. Both are archived. This consultation does not replace the PM's planned independent code review.
