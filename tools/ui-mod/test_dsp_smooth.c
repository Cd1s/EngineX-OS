/* gcc -std=c11 -O2 -Wall -Wextra -Werror tools/ui-mod/test_dsp_smooth.c -o /tmp/test_dsp_smooth && /tmp/test_dsp_smooth */
#include <math.h>
#include <stdio.h>
#include <stdlib.h>

#include "dsp_smooth.h"

static int failures;
#define CHECK(cond) do { if (!(cond)) { fprintf(stderr, "%s:%d: %s\n", __FILE__, __LINE__, #cond); failures++; } } while (0)

static void test_finite(void)
{
    CHECK(sc_dsp_finite_f32(0.0f) && sc_dsp_finite_f32(-1.5f) && sc_dsp_finite_f32(3.0e38f));
    CHECK(!sc_dsp_finite_f32(NAN) && !sc_dsp_finite_f32(INFINITY) && !sc_dsp_finite_f32(-INFINITY));
    CHECK(sc_dsp_finite_f64(0.0) && sc_dsp_finite_f64(-1e300));
    CHECK(!sc_dsp_finite_f64((double)NAN) && !sc_dsp_finite_f64((double)INFINITY) && !sc_dsp_finite_f64(-(double)INFINITY));

    unsigned char raw[24] = {0};
    float f = 0.75f, bad = NAN;
    double d = -2.5;
    memcpy(raw + 1, &f, sizeof f);   /* unaligned on purpose */
    memcpy(raw + 5, &bad, sizeof bad);
    memcpy(raw + 9, &d, sizeof d);
    CHECK(sc_dsp_read_f32(raw, 1) == 0.75f);
    CHECK(sc_dsp_read_f32(raw, 5) == 0.0f);
    CHECK(sc_dsp_read_f64(raw, 9) == -2.5);
    CHECK(sc_dsp_read_f32(NULL, 0) == 0.0f && sc_dsp_read_f64(NULL, 8) == 0.0);
}

static float phase_of(const unsigned char *gate)
{
    float v;
    memcpy(&v, gate + SC_DSP_AUTOGATE_PHASE_OFFSET, sizeof v);
    return v;
}

static void set_phase(unsigned char *gate, float v)
{
    memcpy(gate + SC_DSP_AUTOGATE_PHASE_OFFSET, &v, sizeof v);
}

static void test_autogate(void)
{
    static unsigned char gate[0x200], other[0x200];
    set_phase(gate, 0.7f);
    set_phase(other, 0.3f);
    sc_dsp_autogate_pre_process(gate, 450);   /* first sight: nothing to undo */
    sc_dsp_autogate_pre_process(gate, 450);
    CHECK(phase_of(gate) == 0.7f);

    sc_dsp_fx_mark_transition(gate, 99);      /* not a transition kind */
    sc_dsp_autogate_pre_process(gate, 450);
    CHECK(phase_of(gate) == 0.7f);

    sc_dsp_fx_mark_transition(NULL, SC_DSP_FX_TRANSITION_SWITCH);
    sc_dsp_autogate_pre_process(gate, 450);
    CHECK(phase_of(gate) == 0.0f);
    set_phase(gate, 0.4f);                     /* the gate runs on */
    sc_dsp_autogate_pre_process(gate, 450);    /* idempotent: one clear per transition */
    CHECK(phase_of(gate) == 0.4f);

    /* A gate first seen after the transition keeps its phase. */
    sc_dsp_autogate_pre_process(other, 450);
    CHECK(phase_of(other) == 0.3f);

    sc_dsp_fx_mark_transition(NULL, SC_DSP_FX_TRANSITION_CHANGE);
    sc_dsp_autogate_pre_process(gate, 450);
    sc_dsp_autogate_pre_process(other, 450);
    CHECK(phase_of(gate) == 0.0f && phase_of(other) == 0.0f);

    /* More instances than slots never write out of bounds or crash. */
    static unsigned char many[12][0x200];
    for (int i = 0; i < 12; i++)
        sc_dsp_autogate_pre_process(many[i], 1);
    sc_dsp_autogate_pre_process(NULL, 1);
}

static void test_post_process(void)
{
    enum { N = 300 };
    static float in_l[N], in_r[N], out_l[N], out_r[N];
    float *in[2] = {in_l, in_r}, *out[2] = {out_l, out_r};

    /* Drain any ramp armed by earlier tests. */
    sc_dsp_fx_common_post_process(NULL, in, out, N);

    /* Not armed: NaN/Inf are cleaned, everything else stays. */
    for (int i = 0; i < N; i++) {
        in_l[i] = in_r[i] = 0.25f;
        out_l[i] = out_r[i] = 1.0f;
    }
    out_l[3] = NAN;
    out_r[4] = INFINITY;
    in_r[4] = NAN;   /* no finite dry sample: silence */
    out_l[5] = -INFINITY;
    sc_dsp_fx_common_post_process(NULL, in, out, N);
    CHECK(out_l[3] == 0.25f && out_r[4] == 0.0f && out_l[5] == 0.25f && out_l[6] == 1.0f && out_r[0] == 1.0f);

    /* Armed: a step from dry 0 to wet 1 becomes a 128-frame ramp, across two calls. */
    for (int i = 0; i < N; i++) {
        in_l[i] = in_r[i] = 0.0f;
        out_l[i] = out_r[i] = 1.0f;
    }
    sc_dsp_fx_mark_transition(NULL, SC_DSP_FX_TRANSITION_TURN_ON);
    sc_dsp_fx_common_post_process(NULL, in, out, 100);
    float *in2[2] = {in_l + 100, in_r + 100}, *out2[2] = {out_l + 100, out_r + 100};
    sc_dsp_fx_common_post_process(NULL, in2, out2, N - 100);
    CHECK(out_l[0] == 0.0f && out_r[0] == 0.0f);
    float max_step = out_l[0];
    for (int i = 1; i < N; i++) {
        float step = out_l[i] - out_l[i - 1];
        CHECK(step >= 0.0f);
        CHECK(out_l[i] == out_r[i]);
        if (step > max_step)
            max_step = step;
    }
    CHECK(max_step <= 1.0f / SC_DSP_RAMP_FRAMES + 1e-6f);
    CHECK(fabsf(out_l[SC_DSP_RAMP_FRAMES - 1] - (float)(SC_DSP_RAMP_FRAMES - 1) / SC_DSP_RAMP_FRAMES) < 1e-6f);
    CHECK(out_l[SC_DSP_RAMP_FRAMES] == 1.0f && out_l[N - 1] == 1.0f);

    /* The ramp is spent: the next block passes through. */
    for (int i = 0; i < N; i++)
        out_l[i] = out_r[i] = 1.0f;
    sc_dsp_fx_common_post_process(NULL, in, out, N);
    CHECK(out_l[0] == 1.0f);

    /* In-place processing has no dry copy: only sanitized, ramp left armed. */
    sc_dsp_fx_mark_transition(NULL, SC_DSP_FX_TRANSITION_TURN_OFF);
    sc_dsp_fx_common_post_process(NULL, out, out, N);
    CHECK(out_l[0] == 1.0f);
    sc_dsp_fx_common_post_process(NULL, in, out, N);
    CHECK(out_l[0] == 0.0f);

    sc_dsp_fx_common_post_process(NULL, NULL, NULL, N); /* no crash */
}

static void test_fader(void)
{
    enum { FRAMES = 450, FLOATS = 2 * FRAMES };
    static float from[FLOATS], to[FLOATS], out[FLOATS], stock[FLOATS];
    for (int i = 0; i < FLOATS; i++) {
        from[i] = 0.0f;
        to[i] = 1.0f;
    }
    const double w0 = 0.2, w1 = 0.3, step = (w1 - w0) / FRAMES;
    sc_dsp_fader_smooth_block(from, to, out, FLOATS, w0, w1);
    for (int i = 0; i < FLOATS; i++)
        stock[i] = (float)w1;  /* stock: one weight for the whole period */
    CHECK(fabs(out[0] - (w0 + step)) < 1e-6);
    CHECK(fabs(out[FLOATS - 1] - w1) < 1e-6);          /* ends where the next block starts */
    double worst = 0.0;
    for (int f = 0; f < FRAMES; f++) {
        CHECK(out[2 * f] == out[2 * f + 1]);              /* L and R share a weight */
        if (f) {
            double d = fabs(out[2 * f] - out[2 * f - 2] - step);
            if (d > worst)
                worst = d;
        }
    }
    CHECK(worst < 1e-6);                                   /* no stair inside the block */
    CHECK(fabs(stock[0] - out[0]) > 0.09);                  /* the stock step it replaces */

    /* In place, odd length: the lone last sample is its own frame and reaches w_end. */
    float buf[5] = {0, 0, 0, 0, 0};
    float one[5] = {1, 1, 1, 1, 1};
    sc_dsp_fader_smooth_block(buf, one, buf, 5, 0.0, 0.9);
    CHECK(fabsf(buf[0] - 0.3f) < 1e-6f && buf[0] == buf[1] && fabsf(buf[4] - 0.9f) < 1e-6f);

    /* Clamped above 1. */
    sc_dsp_fader_smooth_block(from, to, out, 4, 0.5, 1.5);
    CHECK(out[3] == 1.0f);

    /* Invalid arguments leave out alone. */
    float keep[4] = {7, 7, 7, 7};
    sc_dsp_fader_smooth_block(from, to, keep, 4, 0.5, 0.5);
    sc_dsp_fader_smooth_block(from, to, keep, 4, 1.0, 1.2);
    sc_dsp_fader_smooth_block(from, to, keep, 4, -0.1, 0.5);
    sc_dsp_fader_smooth_block(from, to, keep, 4, (double)NAN, 0.5);
    sc_dsp_fader_smooth_block(from, to, keep, 1, 0.0, 0.5);
    sc_dsp_fader_smooth_block(NULL, to, keep, 4, 0.0, 0.5);
    CHECK(keep[0] == 7 && keep[3] == 7);
}

int main(void)
{
    test_finite();
    test_autogate();
    test_post_process();
    test_fader();
    if (failures) {
        fprintf(stderr, "%d check(s) failed\n", failures);
        return EXIT_FAILURE;
    }
    puts("dsp_smooth: all checks passed");
    return EXIT_SUCCESS;
}
