# Issue #181: pitch fader pickup investigation

Status: **plan only; no playback or setting changes**. Baseline: `73f7b5b`
(Engine OS 5.3.0). The requested change reaches the native translator's
takeover contract, so it is deferred under the task's native-code/risk boundary.

## Requested behavior

The original reporter uses PRIME 2 / PRIME 4 and wants predictable tempo
unlocking without repeatedly moving through the center. The scoped CDJ-style
contract is: after software tempo and the physical fader diverge, retain the
current tempo until the fader reaches or crosses the current target; the first
crossing restores control even if consecutive samples skip over the target.

This is the requested contract, not a verified statement that every Pioneer
model has identical reset and SYNC behavior. Direct takeover, forced center
reset and additional modes in the issue's proposed design are outside scope.

## Stock 5.3.0 evidence

These references are to the stock extracted resource tree, independently read
for this investigation; they are not files added to this repository.

| Resource | Observed responsibility |
| --- | --- |
| `ControlSurfaceModules/ControlSurfaceModules/SpeedSlider.qml:16` | Per-deck assignments; only the active deck or an unloaded deck accepts enabled input. |
| Same file, lines 28–39 | Applies hardware inversion, uses double-precision CC, enables soft takeover, retains soft input while disabled, and targets `/Engine/Deck%1/Speed`. |
| `airAssignments/InputAssignment/ValueOutput.qml:104` | Detached input goes straight to the translator; deferral applies only when the takeover source already matches. |
| Same file, lines 126–166 | Checks editability; disabled/noneditable input calls `setSoftValue`; enabled input calls native `setValue(value, sourceId, 0)`. Initialization has a separate force flag. |
| `airAssignments/InputAssignment/PropertyTarget.qml:51` | Resolves the target's translator. The crossing/latching decision is not implemented here. |
| `Skins/EngineOS/UI/CompositeControls/Deck/DeckItem/SoftTakeoverIndicators.qml:7` | Displays two input booleans; changing these icons cannot unlock the fader. |

The visible `useSoftTakeover` switch already is on. Turning it off sends
`HARDEXTERNAL` input and bypasses pickup; it does not implement crossing pickup.
`centerValue` rounds an input sample around normalized 0.5; it neither exposes
a takeover threshold nor proves a physical center deadband. No crossing-mode
selector was found in the inspected slider, output adapter or preference QML.
This search does not prove that no native configuration exists.

## Transition matrix and remaining uncertainty

| Transition | Stock route and relationship supported by the inspected QML | Still needs native/runtime evidence |
| --- | --- | --- |
| SYNC on | `Sync.qml` requests `Sync`; the hardware assignment still submits soft input to `Speed`. A synced tempo may differ from the physical position. | Follower/LEAD ownership, exact target updates and pickup predicate. |
| SYNC off | Requests `UnSync`; this QML does not reset `Speed` or force fader ownership. | Whether tempo is retained in each sync mode and exactly when ownership changes. |
| Load another track | `TrackSpeed.qml` exposes the existing capability-gated `Track Speed` preference; `DefaultSpeedRange.qml` exposes the default range. | Actual preference entries, resulting speed and ownership for each load policy. Saved per-track offsets are not established by this evidence. |
| Key Lock | `KeyLock.qml` acts on `Track/KeyLock`, key reset or key sync; it does not write `Speed`. | Confirm identical tempo/pickup behavior with Key Lock on/off. |
| Change Pitch Range | `PitchBend.qml` changes `SpeedRange`; speed and range displays read separate properties. | Whether speed is retained, remapped or clamped, and how the normalized pickup target changes. |
| Switch deck/layer | Disabled assignments retain hardware soft values without an enabled speed write. | Native ownership and first-sample behavior when returning to a loaded layer. |

The issue's narrow-window/hysteresis diagnosis is **unconfirmed**. The inspected
QML does not contain that predicate. A generic `Track/ResetTempo` action found
in grid editing is not evidence of a CDJ-style deck tempo-reset button.

## Minimal follow-up design

1. First reproduce stock pickup with timestamped physical samples, effective
   normalized target, ownership, editability, active layer, SYNC and Pitch Range.
   Check for a native crossing option before adding an EngineX option.
2. Locate and verify the native pickup predicate and its callers. Determine
   whether the reported miss is a threshold defect, ownership transition,
   calibration issue or target change. Do not patch guessed offsets or flags.
3. If needed, add one default-off option in the existing EngineX playback
   settings: **Single-Pass Pitch Pickup** / **单次越过接管** / **單次越過接管**,
   with existing Off/On translations. Off must follow the original path.
4. Scope changes to the physical pitch assignment. While detached, accept
   equality or a directional crossing between consecutive physical samples and
   the same current target. A software target moving across a stationary fader
   must not count. Track/layer/range/ownership changes invalidate old samples.
5. Preserve initialization, inactive-layer soft updates, editability and all
   native SYNC/LEAD behavior. Do not force tempo to zero, synthesize hard writes,
   disable SYNC, or change other controllers' soft takeover.
6. Keep the stock pickup indicators bound to the native state. Any hook change
   requires both checked-in hook copies to be rebuilt and verified.

## Verification before implementation can ship

The existing UI regression suite passed all 81 tests. A separate replay of the
stock `ValueOutput` functions passed six routing scenarios: samples skipping a
target, disabled assignment, noneditable target, initialization force flag,
owned-source deferral, and direct input with soft takeover disabled. The replay
mocks the translator; it establishes the QML/native boundary, not native pickup
correctness.

- Replay the actual stock QML input adapter to establish call routing; this
  cannot validate the native latch predicate or hardware behavior.
- Test equality, slow approach, a fast sample jump across the target in both
  directions, center jitter, reversal before reaching the target, and a target
  outside the selected range. No tempo change before pickup; one latch per
  crossing; later fader movement controls tempo normally.
- Repeat after SYNC on/off (including LEAD/follower), each supported track-load
  policy, layer changes, Key Lock on/off and every stock Pitch Range. Target
  changes and initialization must not reuse a stale crossing sample.
- With the option off, compare against stock behavior. Validate PRIME 2 and
  PRIME 4 first, then other physical-fader models before claiming wider support.

## Draft reply to the reporter

Thanks for describing the PRIME 2 / PRIME 4 behavior. Engine OS already has
soft takeover, and we are investigating why a single pass sometimes fails to
return control. The intended behavior is to keep the current tempo until the
fader reaches or crosses it, then hand control back in one pass. The actual
latching decision is below the visible QML controls, so this is not fixed in
5.3.0.4. Any eventual alternative would be optional and off by default, with
PRIME 2 / PRIME 4 hardware validation before release.
