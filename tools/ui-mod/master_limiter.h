/* End-of-chain master soft clipper + envelope peak limiter (#58).
 *
 * Header-only C11 for Engine's audio device callback: no allocation, no lookahead
 * (zero latency), no locks, no libm. Samples at or below the knee (-1.5 dBFS)
 * pass bit-exact. Above it a rational tanh (3/3 Pade) curve bends the peak into
 * the -0.1 dBFS ceiling with a continuous slope at both ends, so there is no
 * corner to create the harsh odd harmonics of a hard clip. When the input is
 * hotter than the curve's range an instant-attack peak envelope lowers the gain
 * instead, so the curve never flattens into a square wave. */
#ifndef SC_MASTER_LIMITER_H
#define SC_MASTER_LIMITER_H

#include <stdint.h>

#define SC_LIM_KNEE 0.841f                            /* -1.5 dBFS */
#define SC_LIM_CEIL 0.988f                            /* -0.1 dBFS */
#define SC_LIM_RANGE (SC_LIM_CEIL - SC_LIM_KNEE)
#define SC_LIM_SAT (SC_LIM_KNEE + 3.0f * SC_LIM_RANGE) /* input at which the curve reaches the ceiling */
/* Envelope release per frame, ~100 ms at the rate Engine opened. */
#define SC_LIM_RELEASE 0.999896f /* 96 kHz compatibility value */
#define SC_LIM_RELEASE_INV_TAU 9.984508f /* -1/log(SC_LIM_RELEASE) / 96 kHz */
#define SC_DSP_DEFAULT_RATE 96000u

struct sc_lim {
    float env; /* peak envelope, 0 while below SC_LIM_SAT */
    float release;
    unsigned rate;
};

static inline void sc_lim_set_rate(struct sc_lim *s, unsigned rate)
{
    if (!s)
        return;
    if (!rate)
        rate = SC_DSP_DEFAULT_RATE;
    if (s->rate == rate && s->release > 0.0f)
        return;
    s->rate = rate;
    s->release = 1.0f - SC_LIM_RELEASE_INV_TAU / (float)rate;
}

/* Static curve. Non-finite input becomes silence. */
static inline float sc_lim_clip(float x)
{
    float a = x < 0.0f ? -x : x;
    if (!(a <= 3.0e38f))
        return 0.0f;
    if (a <= SC_LIM_KNEE)
        return x;
    float u = (a - SC_LIM_KNEE) / SC_LIM_RANGE, y = SC_LIM_CEIL;
    if (u < 3.0f) /* tanh(u) ~ u(27+u^2)/(27+9u^2): slope 1 at u=0, 0 at u=3, monotonic between */
        y = SC_LIM_KNEE + SC_LIM_RANGE * u * (27.0f + u * u) / (27.0f + 9.0f * u * u);
    if (y > SC_LIM_CEIL)
        y = SC_LIM_CEIL;
    return x < 0.0f ? -y : y;
}

/* One stereo pair of Engine's planar float output, in place; r may be NULL
 * for a lone channel. Both channels share one gain, so the image does not move. */
static inline void sc_lim_planar(struct sc_lim *s, float *l, float *r, unsigned frames)
{
    if (!s || !l)
        return;
    sc_lim_set_rate(s, s->rate);
    float env = s->env;
    for (unsigned i = 0; i < frames; i++) {
        float a = l[i] < 0.0f ? -l[i] : l[i], b = r ? (r[i] < 0.0f ? -r[i] : r[i]) : 0.0f, peak = 0.0f;
        if (a <= 3.0e38f) peak = a; /* false for NaN/Inf: they do not drive the envelope */
        if (b <= 3.0e38f && b > peak) peak = b;
        env *= s->release;
        if (peak > env)
            env = peak; /* instant attack: no lookahead, no added latency */
        if (env <= SC_LIM_SAT)
            env = 0.0f; /* gain is unity there; also keeps the decay out of denormals */
        float g = env > 0.0f ? SC_LIM_SAT / env : 1.0f;
        if (g != 1.0f || !(a <= SC_LIM_KNEE))
            l[i] = sc_lim_clip(l[i] * g);
        if (r && (g != 1.0f || !(b <= SC_LIM_KNEE)))
            r[i] = sc_lim_clip(r[i] * g);
    }
    s->env = env;
}

/* #64 Auto-Gain: brings quiet material up toward a -4.4 dBFS peak before the
 * limiter above, which always runs after it. A stereo-linked peak follower
 * (instant rise, ~3 s fall) sets the target; the gain only ever boosts (at
 * most +9 dB), falls within ~200 ms when the music gets louder, rises over
 * ~2 s, and moves linearly sample by sample inside a block (no zipper noise).
 * A block whose peak is under -42 dBFS freezes everything, so silence, intros
 * and noise floors are never pulled up. Time constants are scaled to the
 * output rate Engine opened. */
#define SC_GAIN_TARGET 0.600f /* -4.4 dBFS: about 2.9 dB below the limiter knee */
#define SC_GAIN_MAX 2.82f     /* +9 dB */
#define SC_GAIN_GATE 0.008f   /* -42 dBFS */
#define SC_GAIN_FALL 288000.0f /* level follower release, frames (~3 s) */
#define SC_GAIN_DOWN 19200.0f  /* gain decrease time constant, frames (~200 ms) */
#define SC_GAIN_UP 192000.0f   /* gain increase time constant, frames (~2 s) */

struct sc_gain {
    float level, gain; /* 0 means not started: unity gain */
    float fall, down, up;
    unsigned rate;
};

static inline void sc_gain_set_rate(struct sc_gain *s, unsigned rate)
{
    if (!s)
        return;
    if (!rate)
        rate = SC_DSP_DEFAULT_RATE;
    if (s->rate == rate && s->fall > 0.0f)
        return;
    s->rate = rate;
    float scale = (float)rate / (float)SC_DSP_DEFAULT_RATE;
    s->fall = SC_GAIN_FALL * scale;
    s->down = SC_GAIN_DOWN * scale;
    s->up = SC_GAIN_UP * scale;
}

static inline void sc_gain_planar(struct sc_gain *s, float *l, float *r, unsigned frames)
{
    if (!s || !l || !frames)
        return;
    sc_gain_set_rate(s, s->rate);
    float peak = 0.0f;
    for (unsigned i = 0; i < frames; i++) {
        float a = l[i] < 0.0f ? -l[i] : l[i], b = r ? (r[i] < 0.0f ? -r[i] : r[i]) : 0.0f;
        if (a <= 3.0e38f && a > peak) peak = a; /* NaN/Inf never drive the level */
        if (b <= 3.0e38f && b > peak) peak = b;
    }
    float g0 = s->gain > 0.0f ? s->gain : 1.0f, g1 = g0;
    if (peak >= SC_GAIN_GATE) {
        float n = (float)frames, fall = n / s->fall;
        if (fall > 1.0f) fall = 1.0f;
        s->level = peak > s->level ? peak : s->level + (peak - s->level) * fall;
        float target = SC_GAIN_TARGET / s->level;
        if (target > SC_GAIN_MAX) target = SC_GAIN_MAX;
        if (target < 1.0f) target = 1.0f;
        float k = n / (target < g0 ? s->down : s->up);
        g1 = g0 + (target - g0) * (k > 1.0f ? 1.0f : k);
        if (g1 - target < 1e-4f && target - g1 < 1e-4f) g1 = target; /* float steps stall one ulp short of unity */
    }
    s->gain = g1;
    if (g0 == 1.0f && g1 == 1.0f)
        return; /* unity: leave the block untouched */
    for (unsigned i = 0; i < frames; i++) {
        float g = g0 + (g1 - g0) * (float)(i + 1) / (float)frames;
        l[i] *= g;
        if (r)
            r[i] *= g;
    }
}

/* #172 Hi-Fi output compensation (Gen-1 players JP07/JP08/JP13/JP14 only).
 *
 * On SC5000/SC6000 players, Engine renders DSP_MASTER at 48 kHz (225 frames)
 * and ALSACombinedDevice upsamples 2x to 96 kHz via a 4-point Catmull-Rom
 * cubic Hermite interpolator (phases {0, 0.5}), which rolls off the passband
 * by -0.26/-0.91/-1.48/-2.24/-3.23 dB at 10/14/16/18/20 kHz.
 *
 * Two cascaded minimum-phase DF2T biquads at 48 kHz invert that fixed stage
 * within +/-0.011 dB across 20 Hz - 20 kHz (0.002 dB RMS, <= +0.005 dB net
 * everywhere, so 48 kHz @ 0 % becomes flat and never overshoots) and roll the
 * boost back down above ~21.5 kHz to 0.00 dB at 24 kHz Nyquist. A 480-frame
 * (10 ms) linear dry/wet ramp switches on and off without clicks. */
#define SC_HIFI_STEP (1.0f / 480.0f)

struct sc_hifi {
    float z[2][4]; /* per-channel DF2T states: [z1_0, z2_0, z1_1, z2_1] */
    float mix;     /* 0.0f = bypassed, 1.0f = fully engaged */
};

static inline float sc_hifi_sample(float *z, float x)
{
    static const float b10 = 1.085936189f, b11 = 0.125369817f, b12 = 0.037535761f;
    static const float a11 = 0.235026851f, a12 = 0.013814930f;
    static const float b20 = 1.032249928f, b21 = 1.523236990f, b22 = 0.561940193f;
    static const float a21 = 1.513246536f, a22 = 0.604180515f;

    float u = (x >= -3.0e38f && x <= 3.0e38f) ? x : 0.0f;
    float y1 = b10 * u + z[0];
    z[0] = b11 * u - a11 * y1 + z[1];
    z[1] = b12 * u - a12 * y1;
    float y2 = b20 * y1 + z[2];
    z[2] = b21 * y1 - a21 * y2 + z[3];
    z[3] = b22 * y1 - a22 * y2;
    return y2;
}

static inline int sc_hifi_planar(struct sc_hifi *s, float *l, float *r, unsigned frames, int enabled)
{
    if (!s || !l || !frames)
        return 0;
    float mix = s->mix;
    if (!enabled && mix <= 0.0f)
        return 0;
    if (enabled && mix <= 0.0f) {
        for (unsigned c = 0; c < 2; c++)
            for (unsigned k = 0; k < 4; k++)
                s->z[c][k] = 0.0f;
        mix = 0.0f;
    }
    if (enabled && mix >= 1.0f) {
        for (unsigned i = 0; i < frames; i++) {
            l[i] = sc_hifi_sample(s->z[0], l[i]);
            if (r)
                r[i] = sc_hifi_sample(s->z[1], r[i]);
        }
        mix = 1.0f;
    } else {
        float delta = enabled ? SC_HIFI_STEP : -SC_HIFI_STEP;
        for (unsigned i = 0; i < frames; i++) {
            mix += delta;
            if (mix >= 1.0f) mix = 1.0f;
            if (mix <= 0.0f) mix = 0.0f;
            float dl = (l[i] >= -3.0e38f && l[i] <= 3.0e38f) ? l[i] : 0.0f;
            float wl = sc_hifi_sample(s->z[0], dl);
            l[i] = dl + mix * (wl - dl);
            if (r) {
                float dr = (r[i] >= -3.0e38f && r[i] <= 3.0e38f) ? r[i] : 0.0f;
                float wr = sc_hifi_sample(s->z[1], dr);
                r[i] = dr + mix * (wr - dr);
            }
        }
    }
    if (!enabled && mix <= 0.0f) {
        for (unsigned c = 0; c < 2; c++)
            for (unsigned k = 0; k < 4; k++)
                s->z[c][k] = 0.0f;
        s->mix = 0.0f;
        return 1;
    }
    for (unsigned c = 0; c < 2; c++)
        for (unsigned k = 0; k < 4; k++)
            if (s->z[c][k] > -1.0e-20f && s->z[c][k] < 1.0e-20f)
                s->z[c][k] = 0.0f;
    s->mix = mix;
    return 1;
}

#endif
