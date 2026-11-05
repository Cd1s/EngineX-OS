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

#endif
