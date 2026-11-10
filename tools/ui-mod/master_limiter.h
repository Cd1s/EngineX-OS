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
/* Envelope release per frame, ~100 ms. shortcut: assumes 96 kHz like the Flanger
 * makeup; at 48 kHz it releases in ~200 ms. Read the rate from hw_params if it matters. */
#define SC_LIM_RELEASE 0.999896f

struct sc_lim {
    float env; /* peak envelope, 0 while below SC_LIM_SAT */
};

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
    float env = s->env;
    for (unsigned i = 0; i < frames; i++) {
        float a = l[i] < 0.0f ? -l[i] : l[i], b = r ? (r[i] < 0.0f ? -r[i] : r[i]) : 0.0f, peak = 0.0f;
        if (a <= 3.0e38f) peak = a; /* false for NaN/Inf: they do not drive the envelope */
        if (b <= 3.0e38f && b > peak) peak = b;
        env *= SC_LIM_RELEASE;
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

/* #64 Auto-Gain: brings quiet material up toward a -0.4 dBFS peak before the
 * limiter above, which always runs after it. A stereo-linked peak follower
 * (instant rise, ~3 s fall) sets the target; the gain only ever boosts (at
 * most +9 dB), falls within ~50 ms when the music gets louder, rises over
 * ~2 s, and moves linearly sample by sample inside a block (no zipper noise).
 * A block whose peak is under -42 dBFS freezes everything, so silence, intros
 * and noise floors are never pulled up. shortcut: time constants assume 96 kHz. */
#define SC_GAIN_TARGET 0.95f  /* -0.4 dBFS */
#define SC_GAIN_MAX 2.82f     /* +9 dB */
#define SC_GAIN_GATE 0.008f   /* -42 dBFS */
#define SC_GAIN_FALL 288000.0f /* level follower release, frames (~3 s) */
#define SC_GAIN_DOWN 4800.0f   /* gain decrease time constant, frames (~50 ms) */
#define SC_GAIN_UP 192000.0f   /* gain increase time constant, frames (~2 s) */

struct sc_gain {
    float level, gain; /* 0 means not started: unity gain */
};

static inline void sc_gain_planar(struct sc_gain *s, float *l, float *r, unsigned frames)
{
    if (!s || !l || !frames)
        return;
    float peak = 0.0f;
    for (unsigned i = 0; i < frames; i++) {
        float a = l[i] < 0.0f ? -l[i] : l[i], b = r ? (r[i] < 0.0f ? -r[i] : r[i]) : 0.0f;
        if (a <= 3.0e38f && a > peak) peak = a; /* NaN/Inf never drive the level */
        if (b <= 3.0e38f && b > peak) peak = b;
    }
    float g0 = s->gain > 0.0f ? s->gain : 1.0f, g1 = g0;
    if (peak >= SC_GAIN_GATE) {
        float n = (float)frames, fall = n / SC_GAIN_FALL;
        if (fall > 1.0f) fall = 1.0f;
        s->level = peak > s->level ? peak : s->level + (peak - s->level) * fall;
        float target = SC_GAIN_TARGET / s->level;
        if (target > SC_GAIN_MAX) target = SC_GAIN_MAX;
        if (target < 1.0f) target = 1.0f;
        float k = n / (target < g0 ? SC_GAIN_DOWN : SC_GAIN_UP);
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

/* #172 SC5000/SC6000 16-20 kHz high-frequency roll-off shelf compensation (+3.8 dB RBJ biquad) */
struct sc_hifi_shelf {
	float z1_l, z2_l, z1_r, z2_r;
};

static inline float sc_hifi_clean(float x)
{
	return (x <= 3.0e38f && x >= -3.0e38f) ? ((x > -1.0e-20f && x < 1.0e-20f) ? 0.0f : x) : 0.0f;
}

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

#endif
