# Beat FX free-millisecond timing (#162): feasibility review

Reviewed 2026-11-11 against sandbox baseline `73f7b5b` and the unmerged
investigation `8d68d68`.

**Decision: defer implementation for 5.3.0.4.** No verified stock setting or
effect-local milliseconds parameter was found that can simply be enabled.
The available evidence does not establish a safe, minimal implementation of
free timing while preserving stock FX. This is a release-scope decision,
not proof that the feature is impossible.

The requested behaviour is an adjustable delay time independent of track BPM
and beatgrid. Converting a beat fraction to an `ms` label would not provide it.

## Stock evidence and search boundary

Static inspection used the supplied stock 5.3.0 Engine and its extracted QML:

- Engine BuildID: `be537e908121e76164ddbbf9730d472ba0b8f9d4`.
- Engine SHA-256: `51dd0cc10bcfae46edf4e94e889facd0a510785f1cbcb91d6cacb604b5283b1a`.
- The executable was read, not started. Findings about this binary do not
  establish behaviour across all 16 models.
- The laboratory directory does not establish the source hardware model.
  This review identifies the executable by its fingerprints, without a model
  attribution.

Paths below are relative to the extracted QML root:

| Observation | Evidence |
| --- | --- |
| Stock has both continuous and stepped time-knob input routes. The capability flag selects input handling, not an explicit timing-unit setting. | `ControlSurfaceModules/ControlSurfaceModules/FxAssignmentConfig.qml:25`; `DJFxTime.qml:13` in the same directory |
| Continuous input targets the existing `Parameter` property; stepped input increments/decrements `FxHwParamTwist`. Neither QML route supplies a time in milliseconds. | `ControlSurfaceModules/ControlSurfaceModules/private/ContinuousTimeKnobOutput.qml:16`; `SteppedTimeKnobOutput.qml:14` and `:16` in that directory |
| The control exposes `Rate`, `Parameter` and hardware push/shift-push/twist properties. Their names alone do not establish the C++ parameter mapping for every effect. | `ControlSurfaceModules/ControlSurfaceModules/FxAssignmentConfig.qml:85`, `:90`, `:99` |
| Hardware FX OLED text is painted in QML from the shared display properties, including `ParamValueString`. | `ControlSurfaceModules/ControlSurfaceModules/DJFxTime.qml:86`, `:102` |
| The FX settings tab loads Main FX, Sweep FX, transition FX and general settings. Main FX has an empty QML model, so inspection of that file alone does not enumerate settings supplied by C++. | `Skins/Embedded/Views/ControlCenter/Tabs/Preferences/FxTabContent.qml:6`; `Entries/MainFxSettings.qml:9` |
| The binary contains `LookUpParameter`, `defaultIndex < rates.size()`, `_denominator > 0`, `wzParameterTypeDelayRatio` and discrete `Time 1/32` through `Time 4` labels. | Static binary string search; these support the existing beat-ratio representation but do not prove every DSP path lacks free time. |

A bounded search of 72 FX-related QML files covered the top-level control-surface
FX modules, private time-knob outputs, main-view FX controls, FX-named preference
entries and Touch FX QML. The case-insensitive pattern
`millisecond|\bms\b|TimeMode|TimeMs|FreeTime|TempoSync` returned no matches.
Binary string searches also found generic `TempoSync` and tempo-synced control
names; they are not evidence of an FX-specific writable milliseconds switch.
String absence does not exclude an unnamed or unexposed implementation.

## Corrections to the earlier report

The earlier report's decision to avoid a speculative audio change is reasonable,
but several supporting claims exceed the evidence:

- The earlier report's JP07 attribution was not established by the laboratory
  path. The binary fingerprints identify the inspected material without
  assuming its source model.
- No-BPM fallback values of 60/120 BPM and lack of fractional-delay interpolation
  were not established by this review. They must remain unknown.
- The continuous knob route exists. A single stepped-encoder assumption and a
  fixed number of turns cannot describe all hardware.
- FX OLED rendering is present in QML. Display consistency still needs testing,
  but it is not inherently an inaccessible C++ display path.
- A new hook does not automatically require a particular edit-count change.
  The current source has 21 DSP slots; AZ04 deliberately skips seven event
  slots (`tools/ui-mod/engine_ui_hook.c`, `dsp_site_active`). Counts are
  build-specific, not a universal implementation blocker.
- Changing shared deck tempo would violate the requested independence. Whether
  a safe effect-local timing control exists has not been established; the
  earlier blanket claim about all tempo injection is not demonstrated.

## What would unblock implementation

First trace an existing **effect-local** time or sync parameter through stock
parameter construction, control events and DSP, including the actual type,
units, limits and per-build ABI. If that contract exists, use it. Otherwise this
requires a separate audio implementation and validation effort, beyond a
settings-page change in the frozen release.

Acceptance needs measured fixed delay time while BPM/pitch changes, operation
without a beatgrid, smooth time sweeps and mode switches, and agreement between
touch controls, physical controls and displays. Stock FX selection, routing,
sync, loops and audio must retain their normal behaviour. A replacement that
disables or restricts stock FX is not acceptable.

No runtime code, settings, translations, UI packs or hook binaries change in
this branch. There is no new setting to translate or document as available.

## Validation

The existing `bash tools/ui-mod/test_fx_selector.sh` passed all 11 checks:
Pioneer selector and DSP smoothing in plain, ASan/UBSan and TSan builds;
armhf Flanger scope; and master-output golden checks on armhf and AArch64 with
both floating-point contraction settings. These are baseline regression checks,
not a demonstration of free-millisecond timing. No image build, release gate
or real-device test was performed for this review.

## Suggested reply to the reporter (draft only)

Thanks for the request and the additional use cases. We reviewed the stock
Engine OS 5.3.0 FX controls and timing representation. We have not identified
a verified built-in switch that enables free-millisecond timing. Showing a
beat value in milliseconds would still leave it tied to BPM, so that would not
deliver the behaviour you requested.

We are deferring this from 5.3.0.4 until an independent timing path is
verified. Further work needs to establish an independent timing
path and test it without compromising stock FX, including physical controls
and displays. This review does not rule out a future implementation, but we
cannot promise support or a delivery date yet.
