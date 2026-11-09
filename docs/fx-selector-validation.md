# FX selector R5b validation — 2026-11-09

Follow-up to `03d9404`; branch `agent/fx-selector-20261109`. Baseline master DSP: `dfdef97e04621e1fc7fc8f4e2f0fc607a00b316d`.
All compilation/tests/sandboxes ran on Oracle-Batam-arm. Native armhf execution; no qemu or deck connection. The shared laboratory remained read only.

| Check | Result |
| --- | --- |
| gofmt -l . / go vet ./... | PASS |
| go test -race ./... | PASS |
| Python az01-packer discovery | 136 tests, OK, 1 skipped |
| Rootfs overlay | 18 tests, PASS; mocked storage/hardware |
| test_pioneer_selector / test_dsp_smooth | plain, ASan+UBSan, TSan PASS; UBSan recovery disabled |
| Native armhf Flanger/control regressions | PASS |
| Master golden | AZ04 and armhf, fast/off; five modes × 20 seconds per scope |
| Hook rebuild | Two armhf builds byte-identical; AZ04 compilation PASS |
| Site JavaScript syntax | PASS |
| JC11 rootfs | PASS; selector, two bindings, parameter/Wet, migration |
| JP07 rootfs | PASS; no PX install/bypass/menu hiding/migration |
| JC11 rootfs with read-only /run | PASS; readiness preflight fails back to legacy |

Both committed armhf hook copies have SHA256:

```text
d5970f5227e9d65c9150d14596845a495ef00364a871645e6468ef3f2ca1087d
```

Paths: `rootfs-overlay/etc/sc5000/libengine-ui-hook.so` and `rootfs-overlay/usr/local/lib/sc5000/libengine-ui-hook.so`.

## Review coverage

| Review | Fix / regression |
| --- | --- |
| R1 | A mapped override with no READY bank returns untouched dry input without calling Flanger. `test_double_reset_dry` calls the actual hook through two resets with no prepare, verifies sample-exact dry and unchanged original-call count, then resumes after worker preparation. `test_double_reset_no_prepare` repeats the lifecycle in the sanitizer matrix |
| R2 | Install/master bypass/menu hiding/migration restricted to verified JC11 5.3.0 (`hasDJFx=true`). JP07 has neither selector flag and keeps the original global menu, master DSP and preference. `test_px_product_scope`, `TestFXSelectorJP07PreservesLegacy`, JP07 rootfs probe and master golden |
| R3 | Poison index is `SC_PFX_RING_SIZE - 1000`, within the actual 48000-sample ring. ASan+UBSan uses `-fno-sanitize-recover=undefined`; a UBSan finding now fails the gate |
| R4 | Dedicated FX lock queues ordinary overlaps for at most 250 ms, with context cancellation. `TestFXSelectorQueuesOverlappingUnits` forces a 100 ms overlap and verifies both units receive matching successful ACKs; the stalled-hook test still bounds waiting and leaves settings unlocked |
| R5 | Migration writes 0644 and Syncs before atomic rename, matching saveUI; permission/content assertions in `TestFXSelectorMigratesLegacySettings`. Migration remains a one-time side effect of status discovery on a supported selector |
| R6 | ENABLE does not increment the reset epoch (`test_px_off_on_reset`). Readiness preflight fails back to the original master/menu when /run cannot be written (`test_px_ready_preflight`, read-only-rootfs probe). Heartbeat replacement is atomic (`test_atomic_ready`). Live hardware event semantics remain unverified |
| Non-PX armhf golden | Master uses the frozen dfdef97 DSP on both architectures, separately namespaced from selector DSP. `test_master_golden` checks JP07, JP08, 5.0.4 and sclive4 scopes, each mode for 20 seconds under fast/off, against hashes generated from the actual dfdef97 hook source |

## Golden provenance

The armhf baseline was separately compiled from `git archive dfdef97 tools/ui-mod` with the same deterministic callback fixture. Both FP-contraction flags produced these exact float-word FNV-1a hashes:

```text
Spiral      2b14589a7cbaec75
Pitch Echo  92b5494acccf3730
Slip Roll   b560e4dd037650cc
Dub Echo    2c70597c4940e052
Space       c96acf49bea7c17f
```

The golden is a C callback oracle for the shared hook path and product/build policy. It does not run JP08/5.0.4/sclive4 firmware images. `pioneer_fx_legacy.h` remains byte-identical to dfdef97; `pioneer_master.h` namespaces that frozen source without editing its arithmetic. Existing separate AZ04 fast/off expected hashes remain pinned.

## Sandbox excerpts

```text
JC11: hasDJFx=true hasNHFx=false
engine-ui-hook: PX product=JC11 master-bypass=1 installed
FXPROBE appended-list|PASS
FXNATIVE unit=1 finite+param+wet0|PASS
FXNATIVE unit=2 finite+param+wet0|PASS
FXNATIVE stock-mix-restored|PASS
FXPROBE done|PASS
FXCONFIG legacy-master-bypass-and-migration|PASS

JP07: hasDJFx=false hasNHFx=false
FXCONFIG master-bypassed=false selector=false
FXCONFIG JP07-legacy-preserved|PASS

JC11 with read-only /run: hasDJFx=true hasNHFx=false
FXCONFIG master-bypassed=false selector=false
FXCONFIG JC11-legacy-preserved|PASS
```

All three sandbox Engine runs ended with the planned 40-second timeout (exit 124), after their probes passed.

The JC11 probe uses a real PRIME4 5.3.0 rootfs, production hook, generated UI pack and production Go API in private mount/network/PID namespaces. It tests native knob context, appended list, exact Planck challenge bits, two distinct real ChannelDJFx collections, parameter/Wet-zero output, and native Mix restoration.

There is no ALSA device. A separate offline driver replays DSP event fixtures into captured real handlers, rather than exercising the live device event queue. Therefore physical unit/channel routing, end-to-end knob latency, event ordering/bit preservation and destructor-versus-restoration serialization are not established. Independent Rate is still not captured; Parameter also maps time.

PRIME 4 verification must cover rapid off/on, ENABLE/reassignment and echo tails, overlapping unit selection, rapid mode changes, return to stock Flanger with the prior Mix position, writable /run, and teardown. Steps and the previously checked Engine offsets remain in [fx-selector.md](fx-selector.md).
