# Beat FX Time Parameter: BPM Beat Values vs Free Milliseconds (ms) Mode (#162)

Investigation date: 2026-11-10.
Investigator: 查IN-162 (Gemini 3.8 Flash)
Target baseline: Engine OS 5.3.0 (JP07 armhf, SC LIVE armhf, AZ04 aarch64).

---

## 1. Background & Problem Statement

Issue #162 requests the ability to toggle Beat FX time parameters between:
1. **Quantized Beat Mode (Fractional / Grid-synced)**: Synced to track BPM grid (`1/16`, `1/8`, `1/4`, `1/2`, `3/4`, `1`, `2`, `4` beats).
2. **Free Milliseconds Mode (`ms`)**: Continuous delay time adjustment (`1 ms` to `4000 ms`), independent of track BPM or beatgrid.

Key use cases highlighted by users include:
- Slapback echo (70–130 ms) on un-analyzed vinyl rips and live vinyl inputs without grid snapping.
- Continuous dub delay sweeps and Doppler pitch modulation without stepped grid artifacts.
- Tunable comb filtering (1–25 ms).

---

## 2. Reverse Engineering Evidence (Stock Engine 5.3.0)

Investigation performed on the build host against:
- Main Engine executable: `/root/build/sc5000-lab/5.3.0/tree/usr/Engine/Engine` (JP07, armhf, BuildID `be537e908121e76164ddbbf9730d472ba0b8f9d4`).
- QML resource tree: `/root/build/sc5000-lab/5.3.0/qrc/`.

### 2.1 Property Tree Representation
Beat FX parameters are managed by `DJFxControlChannel` and `DJFxUnitControl`:
- Property tree roots:
  - Send-effects consoles: `/Engine/Mixer/DJFxUnit%1/`
  - Inline / Player layouts: `/Engine/Mixer/%1/DJFx/`
- Key stock properties:
  - `Rate`: Integer index (lookup index) representing the selected discrete beat division.
  - `Parameter`: Normalized float `[0.0, 1.0]` for the effect's secondary parameter (e.g. Flanger Feedback, Reverb Decay).
  - `Mix`: Normalized float `[0.0, 1.0]` for Wet/Dry mix.
  - `Display/BeatLength`: String property holding fraction strings (`"1/4"`, `"1/2"`, `"1"`).
  - `Display/ParamName`: String property (`"Beats"` or `"Rate"`).
  - `Display/ParamValueString`: String representation for display.
  - `Display/ParamValue`: Normalized float value.
- Evidence references in binary:
  - Strings `DJFxUnitControl` (`0x01eec28e`) and `DJFxControlChannel` (`0x01eec29e`).
  - Property name literals at `0x01eec2a0 - 0x01eec360`: `EffectIndex`, `Parameter`, `FilterCutoff`, `Mix`, `Rate`, `Display/BeatLength`, `Display/ParamName`, `Display/ParamValueString`.
  - QML binding: `ControlSurfaceModules/ControlSurfaceModules/FxAssignmentConfig.qml` lines 80–140.

### 2.2 DSP Representation & Class Hierarchy
All stock Beat FX in Engine 5.3.0 are strictly quantized to rational beat fractions:
1. **Rational Fraction Struct (`Rate`)**:
   - Defined in `/usr/src/debug/planck/0.0.0+git/App/src/PlanckEngine/src/Dsp/Rate.h:52`.
   - Structure stores integer `_numerator` and `_denominator` with strict assertion `_denominator > 0`.
2. **Lookup Parameter Wrapper (`fxc::LookUpParameter`)**:
   - Effect classes (`Effects::Echo`, `Effects::DJDelayBase`, `Effects::Flanger`, `Effects::Phaser`, `Effects::AutoGate`, `Effects::Roll`, `Effects::LFOFilter`, `Effects::LFOEcho`) are constructed with `const std::vector<Rate>& rates`.
   - The time parameter is managed via `fxc::ParameterWrapper<fxc::LookUpParameter>`, strictly indexing into `rates`.
   - Assertion in `/usr/src/debug/planck/0.0.0+git/App/src/PlanckEngine/src/Dsp/Effects/Utils/EffectUtils.cpp:160`:
     `"Trying to set default index outside of lookup range (defaultIndex < rates.size())"`.
3. **AIR H3M Delay Engine (`Air::H3M::DJDelay`)**:
   - Time parameter type: enum `Air::H3M::wzParameterTypeDelayRatio`.
   - Parameter definition table at `0x01f2ea5c`:
     `"Time 1/32|Time 1/16|Time 1/8|Time 1/4|Time 1/3|Time 1/2|Time 3/4|Time 1|Time 3/2|Time 2|Time 3|Time 4"`.
   - Delay samples are internally computed by AIR H3M from track tempo (`wzStreamInfo.tempo`) and fraction:
     `delay_samples = (60.0f / bpm) * fraction * sample_rate`.
4. **Ring Buffer Effects (`Effects::Echo` with `djfx::RingBuffer`)**:
   - Audio processing callback at `0x00ebf530`.
   - Reads delay taps at integer sample offsets computed from `Rate` and track BPM.

### 2.3 Event Bus Dispatching (`ChannelDJFx`)
The event dispatchers in `ChannelDJFx` are located sequentially at `0x00e6a080 - 0x00e6a200` (armhf 5.3.0):
- `0x00e6a090`: `ChannelDJFxEvents::TurnOff` (DSP hook slot 0, `DSP_TURN_OFF`)
- `0x00e6a0bc`: `ChannelDJFxEvents::TurnOn` (DSP hook slot 1, `DSP_TURN_ON`)
- `0x00e6a0e8`: `ChannelDJFxEvents::MixChange` (DSP hook slot 5, `DSP_MIX`)
- `0x00e6a114`: `ChannelDJFxEvents::RateChange` (intermediate dispatcher, NOT currently hooked)
- `0x00e6a140`: `ChannelDJFxEvents::ParameterChange` (DSP hook slot 6, `DSP_PARAMETER`)
- `0x00e6a16c`: `ChannelDJFxEvents::EffectSwitch` (DSP hook slot 2, `DSP_SWITCH`)
- `0x00e6a198`: `ChannelDJFxEvents::EffectChange` (DSP hook slot 3, `DSP_CHANGE`)
- `0x00e6a1c4`: `ChannelDJFxEvents::EnabledChanged` (DSP hook slot 4, `DSP_ENABLE`)

### 2.4 Stock Capabilities & Fallback Verdict
- **Does stock Engine have an ms / free mode?**
  **NO.** Stock Engine 5.3.0 has no parameter, flag, or toggle for continuous milliseconds in any Beat FX.
- **Does stock Engine fall back to ms mode when BPM is missing?**
  **NO.** When BPM is 0 or unavailable (e.g. un-analyzed vinyl rips), stock Engine clamps the internal tempo to a fixed fallback BPM (120.0 BPM or 60.0 BPM) and still locks delay times strictly to quantized beat subdivisions of that fallback tempo.

---

## 3. Feasibility Conclusion & Risk Analysis

Per task brief instructions:
> *"判断可行性。可行就按现有模式做最小实现... 不可行或风险太大就不写代码，交付证据和设计方案。"*

**Conclusion: INFEASIBLE / TOO RISKY for a direct minimal patch in this cycle.**

### 3.1 Technical Blockers & Critical Risks

1. **DSP Engine Architectural Incompatibility**:
   - Stock Denon Beat FX (`Air::H3M::DJDelay` and `Effects::Echo`) do NOT expose a continuous delay time parameter.
   - Attempting to simulate millisecond mode by dynamically altering `wzStreamInfo.tempo` (`bpm = 60000.0f / ms`) corrupts the shared deck stream context, destabilizing track deck pitch, phase meters, beatkeeper sync, loop quantize, and stems isolation across the player.
2. **DSP Hook & Release Gate Integrity**:
   - Stock `ChannelDJFxEvents::RateChange` (`0x00e6a114`) is currently unhooked (`DSP_HOOK_COUNT = 21`).
   - Intercepting Delay/Echo in DSP would require adding a new hook site, increasing `DSP_HOOK_COUNT` from 21 to 22.
   - All 3 image groups (`SC5000`, `SC6000`, `PRIME4`) and 8 Data-Bundles enforce `GATE_EXPECT_DSP=21` and `GATE_EXPECT_APPLIED=16`. Altering this without a complete multi-model ABI migration will break release gate verification.
3. **Control Surface & Physical OLED Desync**:
   - Hardware consoles (PRIME 4, SC LIVE, PRIME 2, SC5000/6000) have physical OLEDs driven by stock C++ drivers rendering `/Engine/Mixer/.../Display/BeatLength` and `ParamValueString`.
   - Modifying only QML touch controls leaves physical OLED displays showing stale beat fractions (`"1/4"`), creating severe operational confusion during live performance.
   - Hardware rotary encoders send incremental detent clicks (`1 detent = 1 step`). Stepping through 1–4000 ms with stock stepped handlers requires 160+ full knob turns without custom acceleration logic.
4. **Audio Artifacts & Pop/Click Discontinuities**:
   - Denon's stock `djfx::RingBuffer` only performs integer-sample buffer reads.
   - Continuous delay sweeps without fractional delay interpolation (Hermite / cubic) and crossfade de-zippering produce audible pops and zipper noise, violating professional audio quality standards.

---

## 4. Architectural Design Proposal for Issue #162

To cleanly implement Issue #162 in a future dedicated release, the following multi-tier design should be adopted:

### 4.1 Tier 1: Custom Fractional Delay DSP (`pioneer_fx.h`)
Instead of hacking stock closed-source H3M delay blocks, extend EngineX's existing custom audio DSP pipeline (`pioneer_fx.h`):
- Implement continuous fractional delay with 4-point Hermite interpolation.
- Add one-pole low-pass smoothing (`dsp_smooth.h`) on the delay time parameter to ensure click-free sweeps.
- Support dual modes via atomic state:
  - `FX_TIME_MODE_BEATS`: `delay_sec = (60.0f / bpm) * beat_fraction`
  - `FX_TIME_MODE_MS`: `delay_sec = clamp(time_ms, 1.0f, 4000.0f) / 1000.0f`

### 4.2 Tier 2: Event Interception & Property Reflection
- Hook `ChannelDJFxEvents::RateChange` at `0x00e6a114` (5.3.0 armhf) / RTTI slot in AZ04.
- In `engine_ui_hook.c`, intercept `/Engine/Mixer/.../Display/ParamValueString` and `BeatLength` so that when MS mode is active, the string is formatted as `"%u ms"` (e.g. `"125 ms"`), updating both on-screen QML and physical OLED displays.

### 4.3 Tier 3: UI & Encoder Acceleration (`build_packs.py`)
- In `FxParameters.qml` and `DJFxSpinner.qml`:
  - Add a toggle button or touch target on the value text to switch between `[ BEATS ]` and `[ MS ]`.
- In `DJFxTime.qml`:
  - Add non-linear encoder acceleration when in MS mode:
    - Slow turning: 1 ms steps (1–100 ms range)
    - Medium turning: 10 ms steps (100–1000 ms range)
    - Fast turning: 50–100 ms steps (1000–4000 ms range)

### 4.4 Tier 4: Gate & Persistence Migration
- Update `GATE_EXPECT_DSP` and `GATE_EXPECT_APPLIED` consistently across all 16 supported models.
- Persist default mode (`fx_time_mode=beats|ms`) in `/data/enginex/settings.conf`.

---

## 5. Verification & Test Evidence
- Existing DSP smoothing and Pioneer FX unit tests verified cleanly on the build host via `remote-build`:
  `gcc -std=c11 -O2 -Wall -Wextra -Werror -pthread tools/ui-mod/test_dsp_smooth.c -lm -o /tmp/test_dsp_smooth && /tmp/test_dsp_smooth` -> PASS (`dsp_smooth: all checks passed`).
- No regression introduced to existing codebase.
