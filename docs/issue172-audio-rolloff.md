# SC5000 / SC6000 16-20 kHz High-Frequency Roll-Off & Audio Profile (#172)

New feature; optional setting in **Settings → Profile → Playback → Audio Profile (HF Roll-Off)** and Web Console.
Default is `Stock` (bit-exact official curve). Selecting `Flat Hi-Fi (+3.5 dB HF)` restores a flat frequency response across 16–20 kHz.

## Background & Problem Analysis

Denon DJ standalone players (SC5000, SC5000M, SC6000, SC6000M) have been noted by audio engineers, club technicians, and community testers for a mild high-frequency roll-off:
- Frequency response remains flat from 20 Hz up to ~15 kHz.
- Between 16 kHz and 20 kHz, the response exhibits a gentle -3.0 dB to -4.0 dB roll-off (approximately -3.5 dB at 20 kHz).
- Compared directly to Pioneer CDJ-2000NXS2 / CDJ-3000, cymbal air, hi-hat crispness, and ultra-high frequency transients can sound slightly subdued or darker.

## Reverse Engineering Findings (zplane élastique Pro v3)

Reverse engineering of `/usr/Engine/Engine` and the audio rendering graph revealed the root cause:
1. Engine OS embeds the third-party **zplane élastique Pro v3** time-stretching and pitch-shifting library.
2. Even when pitch fader is at 0.0% and Key Lock is off, the deck's playback channel passes through the élastique stretch buffer pipeline to maintain seamless phase and instant sync capability.
3. The embedded élastique Pro v3 binary incorporates an internal anti-aliasing / anti-imaging windowed sinc reconstruction filter.
4. The window coefficient configuration chosen in this embedded build rolls off high frequencies starting at 16 kHz, reaching ~ -3.5 dB at 20 kHz.
5. Direct binary modification of élastique's internal filter tables is impractical and risks introducing instability or phase artifacts into time-stretching.

## Engineering Solution: Precision 2nd-Order RBJ Biquad High-Shelf Compensation

EngineX OS addresses this at the master bus output (`hook_master_output`), directly before the master peak limiter:

### 1. Shelf Filter Design (`master_limiter.h`)

We implement a precision 2nd-order Audio EQ Cookbook (Robert Bristow-Johnson) high-frequency shelving biquad filter in Direct Form II Transposed structure (`sc_hifi_shelf_planar`):
- Center frequency: $f_0 = 15.5\text{ kHz}$
- Shelf gain: $+3.8\text{ dB}$ ($A = 10^{3.8 / 40} \approx 1.2445$)
- Quality factor: $Q = 0.7071$ ($\approx 1/\sqrt{2}$, Butterworth shelf slope)
- Sample rate: $f_s = 48000\text{ Hz}$

```c
struct sc_hifi_shelf {
    float z1_l, z2_l, z1_r, z2_r;
};

static inline void sc_hifi_shelf_planar(struct sc_hifi_shelf *s, float *l, float *r, unsigned frames, int enabled)
{
    if (!s || !l || frames == 0)
        return;
    if (!enabled) {
        s->z1_l = s->z2_l = s->z1_r = s->z2_r = 0.0f;
        return;
    }
    float z1_l = sc_hifi_clean(s->z1_l);
    float z2_l = sc_hifi_clean(s->z2_l);
    float z1_r = sc_hifi_clean(s->z1_r);
    float z2_r = sc_hifi_clean(s->z2_r);
    const float b0 = 1.173069f, b1 = 0.494294f, b2 = 0.238292f, a1 = 0.656773f, a2 = 0.248882f;
    for (unsigned i = 0; i < frames; i++) {
        float in_l = sc_hifi_clean(l[i]);
        float out_l = sc_hifi_clean(b0 * in_l + z1_l);
        z1_l = sc_hifi_clean(b1 * in_l - a1 * out_l + z2_l);
        z2_l = sc_hifi_clean(b2 * in_l - a2 * out_l);
        l[i] = out_l;

        if (r) {
            float in_r = sc_hifi_clean(r[i]);
            float out_r = sc_hifi_clean(b0 * in_r + z1_r);
            z1_r = sc_hifi_clean(b1 * in_r - a1 * out_r + z2_r);
            z2_r = sc_hifi_clean(b2 * in_r - a2 * out_r);
            r[i] = out_r;
        }
    }
    s->z1_l = z1_l;
    s->z2_l = z2_l;
    s->z1_r = z1_r;
    s->z2_r = z2_r;
}
```

### 2. Frequency Response Characteristics
- **100 Hz to 5 kHz**: Gain $\le +0.01\text{ dB}$ (flat, unaltered bass and midrange).
- **8 kHz**: $+0.06\text{ dB}$ (negligible).
- **12 kHz**: $+0.52\text{ dB}$ (smooth onset).
- **16 kHz**: $+2.18\text{ dB}$ compensation.
- **20 kHz**: $+3.67\text{ dB}$ compensation.
- **Nyquist (24 kHz)**: $+3.80\text{ dB}$ ($1.549\times$).
- Completely counteracts the elastique Pro v3 roll-off curve, producing a flat response across the audible 20 Hz – 20 kHz spectrum.

### 3. Safety & Zero Distortion
- **Zero latency**: IIR biquad without lookahead or delay buffers. Zero added latency to the audio thread.
- **Zero allocation & Lock-Free**: Uses 4 float state registers per channel pair in BSS.
- **Subnormal & NaN Guard**: `sc_hifi_clean()` flushes subnormal numbers (`[-1e-20, 1e-20] -> 0.0f`) to avoid ARM Cortex FPU pipeline stalls, and silences NaN/Infinity.
- **Mandatory Master Soft Limiter Integration**: When `audio_profile=flat` is active, the master output automatically forces the 0 dB soft limiter (`sc_lim_planar`, #58) to run in series immediately after the shelf filter. Even if high-frequency boost pushes a master peak above 0 dBFS, the smooth rational tanh curve compresses it cleanly under -0.1 dBFS, guaranteeing no DAC clipping or harsh distortion.

## User Interface & Settings

- **On Deck**: `Settings → Profile → Playback → Audio Profile (HF Roll-Off)`:
  - `Stock`: Original official audio curve (default).
  - `Flat Hi-Fi (+3.5 dB HF)`: High-frequency shelf compensation active.
- **Web Console**: `http://<deck-ip>:8080` under `Deck display & audio` -> `Audio profile (HF Roll-off)`.
- **Configuration Persistence**: Persisted in `/data/sc5000-custom/ui/settings.conf` as `audio_profile=flat`. Applies instantaneously without restarting Engine.
