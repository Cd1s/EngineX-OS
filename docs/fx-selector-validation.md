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
| Site JavaScript syntax | PASS |
| armhf hook, two builds | Byte-identical |
| AZ04 hook compilation | PASS; existing third-argument ABI/body retained |

Both committed armhf copies have SHA256:

```text
0f068e987f66945608f66d30076cf81caacf9f83411995a68d53265b2dee5d6d
```

Paths: `rootfs-overlay/etc/sc5000/libengine-ui-hook.so` and `rootfs-overlay/usr/local/lib/sc5000/libengine-ui-hook.so`. ELF32 ARM EABI5, stripped; BuildID `10324fe3df12c8a252a7a5ae395d39eec5d80fb1`.

## JC11 sandbox excerpt

Real PRIME4 5.3.0 rootfs, production hook, generated UI pack and production Go API handlers, in private mount/network/PID namespaces. The real private knob component is instantiated using original assignment modules and MIDI configuration fixtures.

```text
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
```

The wire check compares the exact float bits stored by Planck against the acknowledged command. The native driver then replays ABI fixtures into two distinct real ChannelDJFx handler/collection pairs and feeds real carrier pointers with their owner-embedded buffer-array addresses through the installed process hook. Output is finite and nonzero, changes with Parameter, and is bit-identical to input at Wet=0. Selecting stock Flanger restores native Mix.

There is no ALSA device in this sandbox. The fixture unit numbers are assigned by the offline driver; this does not prove the live audio-device event queue, physical unit/channel assignment, end-to-end gain, latency, or listening quality. The bounded Engine run ends via the 40-second timeout (124), so graceful native destructor teardown is not claimed as a runtime result. Destructor unregister logic is covered by the C unit test, and its displaced instructions/offsets were checked statically.

Independent Time/Rate is not wired: Parameter currently also controls time. PRIME 4 hardware verification remains necessary; the steps and verified offset table are in [fx-selector.md](fx-selector.md).

Read-only architecture consultation used paseo-committee: 严IN (Sonnet), agent `815450da-81cc-4b4a-9bd7-71bed88bef11`, verified the owner/collection/ABI chain; 程IN (GPT61), agent `c1592103-ed34-43b8-9003-bdf567262f87`, identified the native Scripted-action registration and encoder bridge. Both are archived. This consultation does not replace the PM's planned independent code review.
