# Issue #172: high-frequency response investigation

Baseline: `73f7b5bacd1d68300db28b016fd6762574d9c5d8`, Engine OS 5.3.0.

No EngineX runtime change is justified by these results. The tested EngineX
wrappers introduce no persistent high-frequency loss with optional processing
disabled. Unmodified stock interpolation kernels can reproduce a similar loss.
The complete playback graph and physical outputs remain unmeasured, so this
does **not** establish which stock kernel is active on either player or prove
that current stock firmware has the reported end-to-end response.

The issue's original report asks whether a problem remembered from older
firmware still exists. It supplies no current captures or stock/EngineX A/B.
The SRC, pitch and DAC explanations in its body are hypotheses.

## EngineX audio changes audited

The 21 armhf hook sites in `tools/ui-mod/engine_ui_hook.c` are:

| Site | EngineX action and relevance to persistent HF loss |
| --- | --- |
| `DSP_TURN_OFF` | Mark an FX transition, then dispatch the stock event. No PCM filter. |
| `DSP_TURN_ON` | Same; also reset an explicitly mapped optional effect. |
| `DSP_SWITCH` | Mark a transition and restore/unbind optional effect routing. |
| `DSP_CHANGE` | Same as switch. |
| `DSP_ENABLE` | Mark a transition and dispatch the stock event. |
| `DSP_MIX` | Reject non-finite control values; handle explicitly selected optional effects. |
| `DSP_PARAMETER` | Same finite-value guard and optional effect controls. |
| `DSP_NUDGE` | Reject non-finite nudge events; valid events reach stock unchanged. |
| `DSP_TEMPO` | Reject non-finite/nonpositive tempo events. No sample interpolation here. |
| `DSP_JUMP` | Reject non-finite jump events. |
| `DSP_RAMP_TIME` | Reject non-finite/negative time events. |
| `DSP_MOTOR_RAMP_TIME` | Same for motor ramp time. |
| `DSP_SAMPLER_IMMEDIATE` | Record an event generation and call stock. |
| `DSP_SAMPLER_STOP` | Same. |
| `DSP_SAMPLER_PLAY` | Same. |
| `DSP_AUTOGATE` | Clear the effect's phase once after an FX transition. Changes the active gate, not ordinary playback. |
| `DSP_COMMON` | Replace non-finite samples and crossfade dry/wet for 128 frames after a transition/new chain (1.33 ms at 96 kHz). No continuous low-pass. |
| `DSP_DUB_ECHO` | Stock effect plus non-finite/±8 sample guard. Normal samples pass unchanged. |
| `DSP_WASH` | Same sample guard. |
| `DSP_FLANGER` | Stock effect plus scalar RMS makeup (#54), restricted to gain 1–2. Optional effect replacements act only when selected. Stock comb notches are not corrected by this scalar. |
| `DSP_MASTER` | Stock callback, optional output routing, Echo Out, effect emulations, auto gain and soft limiter; see below. |

Other relevant changes:

- **Soft limiter (#58):** `master_limiter=1`, default off. Below its 0.841
  knee, steady-state samples are unchanged. Above it, a nonlinear curve and
  linked peak envelope compress level; neither is a persistent HF shelf.
- **Auto gain (#64):** `auto_gain=1`, default off. Scalar linked gain up to
  2.82, followed by the limiter. Time constants smooth the gain envelope,
  not the PCM waveform. It can change dynamics/distortion when enabled.
- **Other master processing:** four-deck routing copies/selects buffers.
  Echo Out's low-pass darkens its feedback repeats; ordinary continuous music
  passes unchanged. Optional effect emulations intentionally alter sound;
  their off path returns without changing samples.
- **`dsp_smooth.h`:** the standalone fader helper crossfades two inputs and
  is not installed as an Engine Fader hook. Transition and energy smoothing
  are distinct from low-pass filtering of the music samples.
- **`sc5000-perf-tune`:** CPU governor/minimum frequency, IRQ affinity and
  priority, and non-RT thread affinity. Audio RT names/policies are excluded
  from thread migration. `etc/default/sc5000-perf` supplies IRQ patterns and
  sweep timing, not PCM rates or coefficients. Scheduling failures could
  cause underruns/clicks; these settings contain no mechanism for a stable
  16–20 kHz response curve.
- **ALSA:** the hook substitutes a monotonic timestamp clock when Engine
  requests wall-clock timestamps; it does not intercept hardware sample-rate
  setup or change PCM samples. The second-generation ALSA configuration only
  boots the preload hook. No first-generation PCM rate/converter/filter
  override was found in the overlay or packer.
- **Stock limiter capability:** EngineX also NOPs a stock constructor's
  `Config.audio.hasLimiter` false write at `0x153ddb0`. This is separate from
  #58. Stock disassembly shows `r3=0` followed by `strb r3,[r4,#36]` at that
  site. The patch affects a capability; it neither replaces the stock limiter
  implementation nor supplies HF coefficients. The resulting stock limiter
  activation/threshold and full response were not exercised by this test.
- **Stems:** capability/processor routing enables separate stem generation
  and playback. Separated or muted stems can have different spectra. The
  fallback separator's masks sum to one; the neural separator preserves a
  residual (`input - sum(other stems)`). That does not prove the complete
  rendered/playback chain is transparent, and ordinary non-Stems playback
  does not run through the remote separator.

## Measurements

`test_hf_response.c` includes the real hook implementation. A deterministic
identity callback stands in for stock processing; the Flanger stand-in is a
known 0.7 scalar. These fixtures isolate EngineX's incremental contribution.
They do not simulate stock decoding, time stretching, conversion or DACs.

Native armhf, 96,000 frames/s, stereo, 128-frame blocks, stepped sines at
20/100/1,000/10,000/16,000/18,000/20,000 Hz. Allow two seconds to settle
(20 seconds for auto gain), then measure one coherent second. Fundamental
gain comes from sine/cosine projection; RMS is recorded separately because
nonlinearity and resampling images can make it misleading. Values below
are dB relative to the input fundamental, not relative to a postprocessed
1 kHz reference.

| EngineX fixture | 1 kHz | 16 kHz | 18 kHz | 20 kHz |
| --- | ---: | ---: | ---: | ---: |
| Master optional processing off, peak 0.1 | 0 | 0 | 0 | 0 |
| FX common identity, settled | 0 | 0 | 0 | 0 |
| Sweep output guard, peak 0.1 | 0 | 0 | 0 | 0 |
| Flanger 0.7 scalar + makeup | -0.000020 | -0.000024 | -0.000021 | -0.000017 |
| Limiter enabled, peak 0.1 | 0 | 0 | 0 | 0 |
| Limiter enabled, peak 1.0 | -0.119057 | -0.002135 | -0.115186 | -0.119005 |
| Limiter enabled, peak 2.0 | -5.070898 | -4.876074 | -5.064321 | -5.072916 |
| Auto gain enabled, peak 0.1 | +9.004432 | +9.004432 | +9.004432 | +9.004432 |
| Echo Out armed, continuous music | 0 | 0 | 0 | 0 |

The zero-gain fixtures above are bit-identical in the measurement window.
The limiter's nonuniform values reflect its nonlinear, sampled-peak behavior;
at peak 2.0 it reduces the entire signal, not just the top octave.

## Stock 5.3.0 kernels

The stock armhf Engine fixture has GNU BuildID
`be537e908121e76164ddbbf9730d472ba0b8f9d4` and SHA-256
`51dd0cc10bcfae46edf4e94e889facd0a510785f1cbcb91d6cacb604b5283b1a`.

RTTI and vtable entries identify `air::LinearResampler`,
`air::HermitianResampler` and `air::HermitianClippedResampler`. Their final
interpolation methods are at `0x17b5818`, `0x17b5850` and `0x17b58d4`.
Disassembly shows self-contained ARM/VFP code without external references.
The optional stock test loads those exact bytes into executable memory after
checking their fingerprints, and calls their original ABI with four neighboring
samples and the fractional position. No vendor bytes are committed, and the
Engine application is not started. The fractional positions model conversion
to 96 kHz; they are not a capture of Engine's actual graph.

| Stock interpolation / input→output rate | 1 kHz | 16 kHz | 18 kHz | 20 kHz |
| --- | ---: | ---: | ---: | ---: |
| Linear, 44.1→96 kHz | -0.014696 | -3.939207 | -5.052971 | -6.335500 |
| Hermitian, 44.1→96 kHz | -0.000045 | -2.069825 | -3.104752 | -4.435762 |
| HermitianClipped, 44.1→96 kHz | -0.000045 | -2.069825 | -3.104752 | -4.435762 |
| Linear, 48→96 kHz | -0.009304 | -2.498775 | -3.206144 | -4.021334 |
| Hermitian / HermitianClipped, 48→96 kHz | -0.000030 | -1.475725 | -2.236389 | -3.225790 |
| All three, 96→96 kHz | 0 | 0 | 0 | 0 |

Thus stock code alone can produce attenuation of the reported scale. However,
the binary also contains `air::FFmpegResampler`/`libswresample` and Elastique
paths. Their presence does not establish where any converter is selected.
This investigation does not identify the active path per track/pitch/Key Lock
state, measure the complete stock limiter, or determine the DAC filter.

## Reproduction and disposition

On the designated build host, compile using the command in
`tools/ui-mod/test_hf_response.c`, then run:

```sh
./test_hf_response
# Optional: first verify the complete stock fixture SHA-256 above.
./test_hf_response /path/to/verified/stock/Engine
```

Both runs passed; the existing native armhf `test_dsp_smooth.c` suite also
passed. The optional stock fixture is not required for ordinary regression
checks. No full firmware build, release gate or hardware access was performed.

Keep runtime audio processing unchanged. Do not add an inverse shelf or
claim the issue fixed. A conclusive device comparison needs the same source
files at 44.1/48/96 kHz, current stock versus EngineX, matched settings and
levels, 0% pitch/Key Lock off and on, and separate S/PDIF/RCA captures. In
particular, record the stock Master Limiter setting as well as EngineX's
optional limiter, auto gain and Stems state.
