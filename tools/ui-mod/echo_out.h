/* Echo Out on the master bus (#104).
 *
 * Header-only C11 for Engine's audio device callback: no allocation, no locks, no libm.
 * While armed it keeps the last 0.5 s of the (post-fader) output in a ring. When music
 * that has played for 0.5 s suddenly goes silent (a fader pulled down, a deck paused),
 * it replays that ring as a feedback echo that fades out over 2.2 s. It works on the
 * output PCM, so it needs no DJFx channel and does not light the stock FX indicator.
 * Latency is zero: the echo starts in the first silent block.
 *
 * shortcut: the frame counts assume a 96 kHz device like master_limiter.h; at 48 kHz
 * the delay and tail run twice as long. Read the rate from hw_params if it matters. */
#ifndef SC_ECHO_OUT_H
#define SC_ECHO_OUT_H

#include <stdint.h>

#define SC_ECHO_DELAY 48000u      /* 0.5 s = one beat at 120 BPM */
#define SC_ECHO_TAIL 211200u      /* 2.2 s */
#define SC_ECHO_FEEDBACK 0.58f
#define SC_ECHO_SOFTEN 0.45f      /* one-pole low-pass per repeat, so repeats darken */
#define SC_ECHO_LOUD_MS2 0.0009f  /* block mean square 0.03^2: "music is playing" */
#define SC_ECHO_QUIET_MS2 4.0e-6f /* 0.002^2: "silence" */

struct sc_echo {
    float l[SC_ECHO_DELAY], r[SC_ECHO_DELAY];
    float lp_l, lp_r;
    uint32_t pos;  /* ring write/read position */
    uint32_t loud; /* frames of music seen since arming; the ring is full once it reaches DELAY */
    uint32_t tail; /* frames of echo left, 0 when idle */
};

/* One stereo pair, in place; r may be NULL. enabled=0 resets and does nothing. */
static inline void sc_echo_planar(struct sc_echo *s, float *l, float *r, unsigned frames, int enabled)
{
    if (!s || !l || frames == 0)
        return;
    if (!enabled) {
        s->tail = 0;
        s->loud = 0;
        return;
    }
    float ms = 0.0f;
    for (unsigned i = 0; i < frames; i++) {
        float a = l[i], b = r ? r[i] : 0.0f;
        float p = a * a + b * b;
        if (p <= 1.0e30f) /* false for NaN/Inf */
            ms += p;
    }
    ms /= (float)frames * (r ? 2.0f : 1.0f);
    if (ms > SC_ECHO_LOUD_MS2) {
        if (s->loud < 0xf0000000u)
            s->loud += frames;
    } else if (ms < SC_ECHO_QUIET_MS2) {
        if (s->loud >= SC_ECHO_DELAY && s->tail == 0) {
            s->tail = SC_ECHO_TAIL;
            s->lp_l = s->lp_r = 0.0f;
        }
        s->loud = 0;
    }
    uint32_t pos = s->pos;
    for (unsigned i = 0; i < frames; i++) {
        if (s->tail) {
            /* The slot holds the sound from exactly DELAY frames ago. */
            float wet = (float)s->tail / (float)SC_ECHO_TAIL;
            float dl = s->l[pos], dr = s->r[pos];
            s->lp_l += SC_ECHO_SOFTEN * (dl * SC_ECHO_FEEDBACK - s->lp_l);
            s->lp_r += SC_ECHO_SOFTEN * (dr * SC_ECHO_FEEDBACK - s->lp_r);
            s->l[pos] = s->lp_l; /* only feedback is written: music that returns is not echoed */
            s->r[pos] = s->lp_r;
            l[i] += dl * wet;
            if (r)
                r[i] += dr * wet;
            s->tail--;
        } else {
            float a = l[i], b = r ? r[i] : 0.0f;
            s->l[pos] = a <= 3.0e38f && a >= -3.0e38f ? a : 0.0f; /* NaN/Inf must not loop forever */
            s->r[pos] = b <= 3.0e38f && b >= -3.0e38f ? b : 0.0f;
        }
        if (++pos == SC_ECHO_DELAY)
            pos = 0;
    }
    s->pos = pos;
}

#endif
