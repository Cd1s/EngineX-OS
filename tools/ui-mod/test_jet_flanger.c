/* #195 Jet Flanger (SC_PFX_FLANGER in pioneer_fx.h), rendered offline.
 * Build host: gcc -std=c11 -O2 -Wall -Wextra -Werror -pthread tools/ui-mod/test_jet_flanger.c -lm -o out/test_jet_flanger
 * Repeat with -fsanitize=address,undefined and -fsanitize=thread (test_fx_selector.sh does). */
#define _GNU_SOURCE
#include <math.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "fx_selector.h"

static int failures;
#define CHECK(x) do { if (!(x)) { fprintf(stderr, "%d: %s\n", __LINE__, #x); ++failures; } } while (0)
enum { RATE = 48000, BLOCK = 128 };
static struct sc_pfx_instance fx;
static struct sc_pioneer_fx bank;

static unsigned lcg = 1;
static float noise(void) { lcg = lcg * 1664525u + 1013904223u; return (float)((int)(lcg >> 8) % 65536 - 32768) / 32768.0f; }

static void test_exp2(void) {
    for (float x = 0; x < 15.0f; x += 0.0137f) CHECK(fabsf(sc_pfx_exp2(x) / exp2f(x) - 1.0f) < 2e-4f);
    CHECK(sc_pfx_exp2(-1.0f) == 1.0f && sc_pfx_exp2(NAN) == 1.0f && isfinite(sc_pfx_exp2(1e9f)));
    for (float g = 0.30f; g <= 0.931f; g += 0.01f) CHECK(fabsf(sc_pfx_flanger_level(g) - sqrtf(1 - g * g)) < 2e-3f);
}

/* The delay moves the same ratio for the same step of the sweep: an exponential curve, from
 * 2 samples to 256 and back, once in 0.25 s (Time 0) to once in 16 s (Time 1). */
static void test_sweep(void) {
    CHECK(sc_pfx_flanger_delay(0) == SC_PFX_FLANGER_MIN);
    CHECK(fabsf(sc_pfx_flanger_delay(0.5f) - 256.0f) < 0.1f);
    CHECK(fabsf(sc_pfx_flanger_delay(0.25f) - sc_pfx_flanger_delay(0.75f)) < 1e-3f);
    float ratio = sc_pfx_flanger_delay(0.05f) / sc_pfx_flanger_delay(0);
    for (float p = 0; p < 0.44f; p += 0.05f)
        CHECK(fabsf(sc_pfx_flanger_delay(p + 0.05f) / sc_pfx_flanger_delay(p) / ratio - 1.0f) < 2e-3f);
    CHECK(fabsf(1.0f / (sc_pfx_flanger_step(0) * RATE) - 0.25f) < 0.001f);
    CHECK(fabsf(1.0f / (sc_pfx_flanger_step(1) * RATE) - 16.0f) < 0.05f);
    CHECK(sc_pfx_flanger_feedback(0) == 0.30f && sc_pfx_flanger_feedback(1) < 0.94f);
}

/* One block through the selector's own path, worker step included. */
static void block(float *l, float *r, float time, float param, float wet) {
    sc_pfx_instance_prepare(&fx);
    CHECK(sc_pfx_process(&fx, l, r, BLOCK, SC_PFX_FLANGER, time, param, wet));
}
static int quiet_state(const struct sc_pioneer_fx *b) { /* nothing left that is not exactly zero or a normal number */
    for (unsigned i = 0; i < SC_PFX_RING_SIZE; ++i) {
        float a = fabsf(b->buf_l[i]), c = fabsf(b->buf_r[i]);
        if ((a != 0 && a < 1e-20f) || (c != 0 && c < 1e-20f)) return 0;
    }
    float a = fabsf(b->fl_lp_l), c = fabsf(b->fl_lp_r);
    return (a == 0 || a >= 1e-20f) && (c == 0 || c >= 1e-20f);
}

/* A minute at the highest feedback, for three kinds of loud input and three sweep speeds: the
 * output stays finite and bounded, and after the input stops it dies away with no denormals. */
static void test_stable_at_max(void) {
    static const float times[] = {0, 0.5f, 1};
    for (unsigned kind = 0; kind < 3; ++kind)
        for (unsigned t = 0; t < 3; ++t) {
            memset(&fx, 0, sizeof fx);
            float peak = 0, tail = 0;
            for (unsigned n = 0; n < 75u * RATE; n += BLOCK) {
                float l[BLOCK], r[BLOCK];
                for (unsigned i = 0; i < BLOCK; ++i) {
                    float x = 0;
                    if (n < 60u * RATE) {
                        if (kind == 0) x = ((n + i) / 37u) & 1u ? 1.0f : -1.0f;       /* full-scale square near 600 Hz */
                        else if (kind == 1) x = 0.9f * noise();
                        else x = 0.9f * sinf((float)(n + i) * 0.2779f);                 /* a held 1950 Hz note */
                    }
                    l[i] = x; r[i] = -x;
                }
                block(l, r, times[t], 1.0f, 1.0f);
                for (unsigned i = 0; i < BLOCK; ++i) {
                    CHECK(isfinite(l[i]) && isfinite(r[i]));
                    float a = fabsf(l[i]) > fabsf(r[i]) ? fabsf(l[i]) : fabsf(r[i]);
                    if (n < 60u * RATE) { if (a > peak) peak = a; }
                    else if (n >= 70u * RATE && a > tail) tail = a;
                }
            }
            CHECK(peak < 2.5f);   /* input peak 1.0 */
            CHECK(tail < 1e-6f);  /* ten seconds after the input stopped */
            CHECK(quiet_state(&fx.bank[fx.current - 1]));
        }
}

/* The centre of the sweep, held: the comb has a peak where the delay fits a whole period.
 * More Parameter makes that peak narrow (resonant): the level falls away faster beside it. */
static float gain_at(float hz, float param) {
    memset(&bank, 0, sizeof bank);
    bank.active_mode = SC_PFX_FLANGER; bank.controlled = 1; bank.time = 1.0f; bank.parameter = param;
    double in = 0, out = 0;
    for (unsigned n = 0; n < RATE / 2; ++n) {
        bank.fl_phase = 0.25f; /* delay 2 * 2^3.5 = 22.6 samples */
        float x = 0.02f * sinf(6.2831853f * hz * (float)n / RATE), l = x, r = x;
        sc_pioneer_fx_planar(&bank, &l, &r, 1, SC_PFX_FLANGER);
        if (n >= RATE / 4) { in += (double)x * x; out += (double)l * l; }
    }
    return (float)sqrt(out / in);
}
static void test_resonance(void) {
    const float peak_hz = RATE / sc_pfx_flanger_delay(0.25f);
    float broad = gain_at(peak_hz, 0) / gain_at(peak_hz * 1.12f, 0), sharp = gain_at(peak_hz, 1) / gain_at(peak_hz * 1.12f, 1);
    printf("jet flanger peak over its shoulder: %.2f at parameter 0, %.2f at 1; peak gain %.2f and %.2f\n",
           broad, sharp, gain_at(peak_hz, 0), gain_at(peak_hz, 1));
    CHECK(gain_at(peak_hz, 0) > 2.0f * gain_at(peak_hz * 1.5f, 0)); /* a comb even at Parameter 0 */
    CHECK(broad < 1.3f && sharp > 2.5f);                            /* and a resonant one at 1 */
    CHECK(gain_at(peak_hz, 1) < 6.0f);
}

/* Bass goes through: a 40 Hz tone keeps its level all the way round the sweep (a comb without
 * the low cut would null it when the delay passes half its period... it never gets that long,
 * so this checks the level, and the next test checks the whole spectrum's loudness). */
static void test_bass_and_loudness(void) {
    static const float params[] = {0, 0.5f, 1};
    for (unsigned p = 0; p < 3; ++p) {
        memset(&fx, 0, sizeof fx);
        double bass_in = 0, bass_out = 0, low = 1e9, high = 0;
        for (unsigned n = 0; n < 8u * RATE; n += BLOCK) {
            float l[BLOCK], r[BLOCK], dry[BLOCK];
            for (unsigned i = 0; i < BLOCK; ++i) dry[i] = l[i] = r[i] = 0.3f * sinf(6.2831853f * 40.0f * (float)(n + i) / RATE);
            block(l, r, 0.3f, params[p], 1.0f);
            double a = 0, b = 0;
            for (unsigned i = 0; i < BLOCK; ++i) { a += (double)dry[i] * dry[i]; b += (double)l[i] * l[i]; }
            bass_in += a; bass_out += b;
            if (n >= RATE && (n / BLOCK) % 9 == 0) { /* a window of 9 blocks is about one 40 Hz period */
                double g = sqrt(bass_out / bass_in);
                if (g < low) low = g;
                if (g > high) high = g;
                bass_in = bass_out = 0;
            }
        }
        CHECK(low > 0.60 && high < 1.25); /* within about 4 dB of the dry tone everywhere in the sweep */

        memset(&fx, 0, sizeof fx);
        double in = 0, out = 0;
        float lp = 0;
        for (unsigned n = 0; n < 16u * RATE; n += BLOCK) {
            float l[BLOCK], r[BLOCK];
            for (unsigned i = 0; i < BLOCK; ++i) { lp += 0.12f * (0.5f * noise() - lp); l[i] = r[i] = lp; in += (double)lp * lp; }
            block(l, r, 0.3f, params[p], 1.0f);
            for (unsigned i = 0; i < BLOCK; ++i) out += (double)l[i] * l[i];
        }
        double db = 10 * log10(out / in);
        printf("jet flanger loudness at parameter %.1f: %+.2f dB\n", params[p], db);
        CHECK(db > -3.0 && db < 3.0); /* full Wet on music-like noise: about as loud as the dry signal */
    }
}

/* Wet 0 is the untouched input; an off/on event starts from silence, not from the old sweep. */
static void test_bypass_and_flush(void) {
    memset(&fx, 0, sizeof fx);
    for (unsigned n = 0; n < RATE; n += BLOCK) {
        float l[BLOCK], r[BLOCK], dl[BLOCK], dr[BLOCK];
        for (unsigned i = 0; i < BLOCK; ++i) { dl[i] = l[i] = 0.5f * noise(); dr[i] = r[i] = 0.5f * noise(); }
        block(l, r, 0.5f, 1.0f, 0.0f);
        CHECK(!memcmp(l, dl, sizeof l) && !memcmp(r, dr, sizeof r));
    }
    for (unsigned n = 0; n < RATE; n += BLOCK) {
        float l[BLOCK], r[BLOCK];
        for (unsigned i = 0; i < BLOCK; ++i) l[i] = r[i] = 0.9f * noise();
        block(l, r, 0.5f, 1.0f, 1.0f);
    }
    sc_pfx_instance_reset(&fx); /* what the hook does after a turn-off/turn-on event */
    for (unsigned n = 0; n < RATE / 2; n += BLOCK) {
        float l[BLOCK] = {0}, r[BLOCK] = {0};
        block(l, r, 0.5f, 1.0f, 1.0f);
        for (unsigned i = 0; i < BLOCK; ++i) CHECK(l[i] == 0 && r[i] == 0);
    }
    /* A mode the selector does not have is off, not this effect. */
    float l[BLOCK], r[BLOCK], d[BLOCK];
    for (unsigned i = 0; i < BLOCK; ++i) d[i] = l[i] = r[i] = 0.5f * noise();
    memset(&fx, 0, sizeof fx);
    for (unsigned n = 0; n < 8; ++n) { sc_pfx_instance_prepare(&fx); sc_pfx_process(&fx, l, r, BLOCK, SC_PFX_FLANGER + 1, 0.5f, 0.5f, 1.0f); }
    CHECK(!memcmp(l, d, sizeof l));
    sc_pfx_select(0, SC_PFX_FLANGER);
    CHECK((sc_pfx_routes[0].route & 7) == SC_PFX_FLANGER);
    sc_pfx_select(0, SC_PFX_FLANGER + 1);
    CHECK((sc_pfx_routes[0].route & 7) == SC_PFX_FLANGER);
}

int main(void) {
    test_exp2();
    test_sweep();
    test_resonance();
    test_bass_and_loudness();
    test_bypass_and_flush();
    test_stable_at_max();
    if (failures) { fprintf(stderr, "%d failures\n", failures); return 1; }
    puts("test_jet_flanger PASS");
    return 0;
}
