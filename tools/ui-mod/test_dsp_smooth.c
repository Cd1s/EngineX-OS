/* gcc -std=c11 -O2 -Wall -Wextra -Werror -pthread tools/ui-mod/test_dsp_smooth.c -lm -o /tmp/test_dsp_smooth && /tmp/test_dsp_smooth
 * Also run once with -fsanitize=address,undefined and once with -fsanitize=thread. */
#define _GNU_SOURCE
#include <pthread.h>
#include <math.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "dsp_smooth.h"
#include "master_limiter.h"
#include "echo_out.h"

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

/* One ChannelDJFx-style effect: enter its slot (as the hook does per block). */
static struct sc_dsp_chain *enter(void *common)
{
    struct sc_dsp_chain *c = sc_dsp_chain_enter(common);
    CHECK(c != NULL);
    return c;
}

/* enter -> post -> exit, as the hook does for one block. */
static void post_on(void *common, float *const *in, float *const *out, uint32_t frames, uint32_t valid)
{
    struct sc_dsp_chain *c = enter(common);
    sc_dsp_fx_common_post_process(c, in, out, frames, valid);
    sc_dsp_chain_exit(c);
}

static int32_t ramp_of(void *common)
{
    struct sc_dsp_chain *c = enter(common);
    int32_t r = c ? c->ramp_remaining : -1;
    sc_dsp_chain_exit(c);
    return r;
}

static void test_post_process(void)
{
    enum { N = 300 };
    static float in_l[N], in_r[N], out_l[N], out_r[N];
    static char fx[8];
    float *in[2] = {in_l, in_r}, *out[2] = {out_l, out_r};

    /* A common seen for the first time ramps (it is usually the effect just
     * turned on); spend that ramp, then it passes through. */
    struct sc_dsp_chain *c = enter(&fx[0]);
    CHECK(c && c->ramp_remaining == SC_DSP_RAMP_FRAMES);
    sc_dsp_fx_common_post_process(c, in, out, N, N);
    sc_dsp_chain_exit(c);
    CHECK(enter(&fx[0]) == c && c->ramp_remaining == 0);
    sc_dsp_chain_exit(c);

    /* Not armed: NaN/Inf are cleaned, everything else stays. */
    for (int i = 0; i < N; i++) {
        in_l[i] = in_r[i] = 0.25f;
        out_l[i] = out_r[i] = 1.0f;
    }
    out_l[3] = NAN;
    out_r[4] = INFINITY;
    in_r[4] = NAN;   /* no finite dry sample: silence */
    out_l[5] = -INFINITY;
    sc_dsp_fx_common_post_process(c, in, out, N, N);
    CHECK(out_l[3] == 0.25f && out_r[4] == 0.0f && out_l[5] == 0.25f && out_l[6] == 1.0f && out_r[0] == 1.0f);

    /* Armed: a step from dry 0 to wet 1 becomes a 128-frame ramp, across two calls. */
    for (int i = 0; i < N; i++) {
        in_l[i] = in_r[i] = 0.0f;
        out_l[i] = out_r[i] = 1.0f;
    }
    sc_dsp_fx_mark_transition(NULL, SC_DSP_FX_TRANSITION_TURN_ON);
    post_on(&fx[0], in, out, 100, 100);
    float *in2[2] = {in_l + 100, in_r + 100}, *out2[2] = {out_l + 100, out_r + 100};
    post_on(&fx[0], in2, out2, N - 100, N - 100);
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
    post_on(&fx[0], in, out, N, N);
    CHECK(out_l[0] == 1.0f);

    /* In-place processing has no dry copy: only sanitized, ramp left armed. */
    sc_dsp_fx_mark_transition(NULL, SC_DSP_FX_TRANSITION_TURN_OFF);
    c = enter(&fx[0]);
    sc_dsp_fx_common_post_process(c, out, out, N, N);
    CHECK(out_l[0] == 1.0f && c->ramp_remaining == SC_DSP_RAMP_FRAMES);
    sc_dsp_chain_exit(c);
    post_on(&fx[0], in, out, N, N);
    CHECK(out_l[0] == 0.0f);

    /* Not a transition kind: nothing re-arms. */
    sc_dsp_fx_mark_transition(NULL, 99);
    CHECK(ramp_of(&fx[0]) == 0);

    /* No chain, no buffers: no crash, and a NULL chain never ramps. */
    CHECK(sc_dsp_chain_enter(NULL) == NULL);
    sc_dsp_fx_common_post_process(NULL, NULL, NULL, N, N);
    for (int i = 0; i < N; i++)
        out_l[i] = out_r[i] = 1.0f;
    sc_dsp_fx_common_post_process(NULL, in, out, N, N);
    CHECK(out_l[0] == 1.0f);

    /* The hook snapshots only the first 128 dry frames of a large aliased block:
     * the ramp covers them, a NaN inside them takes the dry sample, one beyond
     * them falls back to silence, and nothing reads past dry_valid_frames. */
    enum { BIG = 4096 };
    static float big_out_l[BIG], big_out_r[BIG];
    float dry_l[SC_DSP_RAMP_FRAMES], dry_r[SC_DSP_RAMP_FRAMES];
    float *dry_pair[2] = {dry_l, dry_r}, *big_out[2] = {big_out_l, big_out_r};
    for (int i = 0; i < SC_DSP_RAMP_FRAMES; i++)
        dry_l[i] = dry_r[i] = 0.5f;
    for (int i = 0; i < BIG; i++)
        big_out_l[i] = big_out_r[i] = 1.0f;
    big_out_l[SC_DSP_RAMP_FRAMES + 72] = NAN;
    big_out_r[SC_DSP_RAMP_FRAMES + 1] = INFINITY;
    big_out_l[7] = NAN;
    sc_dsp_fx_mark_transition(NULL, SC_DSP_FX_TRANSITION_SWITCH);
    c = enter(&fx[1]);
    sc_dsp_fx_common_post_process(c, dry_pair, big_out, BIG, SC_DSP_RAMP_FRAMES);
    sc_dsp_chain_exit(c);
    CHECK(big_out_l[0] == 0.5f);                                  /* ramp starts at dry */
    CHECK(sc_dsp_finite_f32(big_out_l[7]));
    CHECK(fabsf(big_out_l[SC_DSP_RAMP_FRAMES - 1] - (0.5f + 0.5f * (SC_DSP_RAMP_FRAMES - 1) / SC_DSP_RAMP_FRAMES)) < 1e-6f);
    CHECK(big_out_l[SC_DSP_RAMP_FRAMES] == 1.0f && big_out_l[BIG - 1] == 1.0f);
    CHECK(big_out_l[SC_DSP_RAMP_FRAMES + 72] == 0.0f && big_out_r[SC_DSP_RAMP_FRAMES + 1] == 0.0f);
    CHECK(c->ramp_remaining == 0);

    /* NaN in the snapshot itself reads as silence for the ramp. */
    dry_l[2] = NAN;
    for (int i = 0; i < SC_DSP_RAMP_FRAMES; i++)
        big_out_l[i] = big_out_r[i] = 1.0f;
    sc_dsp_fx_mark_transition(NULL, SC_DSP_FX_TRANSITION_TURN_ON);
    post_on(&fx[1], dry_pair, big_out, SC_DSP_RAMP_FRAMES, SC_DSP_RAMP_FRAMES);
    CHECK(sc_dsp_finite_f32(big_out_l[2]) && big_out_l[2] > 0.0f && big_out_l[2] < 1.0f);

    /* dry_valid_frames 0 with a ramp armed: nothing is read, the ramp stays armed. */
    dry_l[2] = 0.5f;
    for (int i = 0; i < N; i++) {
        in_l[i] = in_r[i] = 0.0f;
        out_l[i] = out_r[i] = 1.0f;
    }
    sc_dsp_fx_mark_transition(NULL, SC_DSP_FX_TRANSITION_TURN_ON);
    c = enter(&fx[1]);
    sc_dsp_fx_common_post_process(c, in, out, N, 0);
    CHECK(out_l[0] == 1.0f && c->ramp_remaining == SC_DSP_RAMP_FRAMES);
    /* dry_valid_frames beyond frames is clamped. */
    sc_dsp_fx_common_post_process(c, in, out, N, N * 4);
    sc_dsp_chain_exit(c);
    CHECK(out_l[0] == 0.0f && out_l[SC_DSP_RAMP_FRAMES] == 1.0f);

    /* Re-armed mid-ramp: the new transition restarts from dry, not where the old one stopped. */
    for (int i = 0; i < N; i++)
        out_l[i] = out_r[i] = 1.0f;
    sc_dsp_fx_mark_transition(NULL, SC_DSP_FX_TRANSITION_TURN_ON);
    post_on(&fx[1], in, out, 64, 64);
    CHECK(out_l[63] > 0.4f && out_l[63] < 0.6f);
    sc_dsp_fx_mark_transition(NULL, SC_DSP_FX_TRANSITION_TURN_OFF);
    for (int i = 0; i < N; i++)
        out_l[i] = out_r[i] = 1.0f;
    post_on(&fx[1], in, out, 64, 64);
    CHECK(out_l[0] == 0.0f);
}

/* Two commons on different threads: each ramps its own 128 frames, neither
 * swallows the other's count. */
static void test_two_chains(void)
{
    static char a_id, b_id;
    static float in_l[256], in_r[256], a_l[256], a_r[256], b_l[256], b_r[256];
    float *in[2] = {in_l, in_r}, *a[2] = {a_l, a_r}, *b[2] = {b_l, b_r};
    for (int i = 0; i < 256; i++) {
        in_l[i] = in_r[i] = 0.0f;
        a_l[i] = a_r[i] = b_l[i] = b_r[i] = 1.0f;
    }
    sc_dsp_fx_mark_transition(NULL, SC_DSP_FX_TRANSITION_SWITCH);
    struct sc_dsp_chain *ca = enter(&a_id);
    struct sc_dsp_chain *cb = enter(&b_id);
    CHECK(ca != cb);
    sc_dsp_fx_common_post_process(ca, in, a, 50, 50);          /* A: frames 0..49 */
    sc_dsp_chain_exit(ca);
    sc_dsp_fx_common_post_process(cb, in, b, 128, 128);        /* B: all 128 in one block */
    sc_dsp_chain_exit(cb);
    CHECK(b_l[0] == 0.0f && fabsf(b_l[127] - 127.0f / 128.0f) < 1e-6f && cb->ramp_remaining == 0);
    float *in2[2] = {in_l + 50, in_r + 50}, *a2[2] = {a_l + 50, a_r + 50};
    post_on(&a_id, in2, a2, 206, 206);  /* A continues at frame 50 */
    CHECK(fabsf(a_l[50] - 50.0f / 128.0f) < 1e-6f);             /* B's ramp did not use up A's */
    CHECK(fabsf(a_l[127] - 127.0f / 128.0f) < 1e-6f && a_l[128] == 1.0f && ca->ramp_remaining == 0);
    CHECK(enter(&b_id) == cb && cb->ramp_remaining == 0);       /* A finishing did not re-arm B */
    sc_dsp_chain_exit(cb);
}

static void reset_chains(void)
{
    memset(sc_dsp_state()->chain, 0, sizeof sc_dsp_state()->chain);
}

/* Enter and leave: the slot a common holds, or NULL. */
static struct sc_dsp_chain *slot_of(void *common)
{
    struct sc_dsp_chain *c = sc_dsp_chain_enter(common);
    sc_dsp_chain_exit(c);
    return c;
}

/* More live commons than slots: no eviction, no out-of-bounds write; the extra
 * one gets no chain and its block is only sanitized. */
static void test_chain_full(void)
{
    static char many[SC_DSP_CHAIN_SLOTS + 1];
    static float in_l[8], in_r[8], out_l[8], out_r[8];
    float *in[2] = {in_l, in_r}, *out[2] = {out_l, out_r};
    struct sc_dsp_chain *first = NULL;
    int taken = 0;
    reset_chains();                                               /* only this test's commons */
    for (size_t i = 0; i < sizeof many; i++) {
        struct sc_dsp_chain *c = slot_of(&many[i]);
        if (!i)
            first = c;
        taken += c != NULL;
    }
    CHECK(taken == SC_DSP_CHAIN_SLOTS);                           /* the extra one got none */
    CHECK(slot_of(&many[0]) == first);                            /* a known common keeps its slot */
    static char late;
    CHECK(slot_of(&late) == NULL);                                /* full: no slot, no eviction */
    /* A slot synced after the caller sampled gen is ahead, not stale (no unsigned wrap). */
    struct sc_dsp_chain *ahead = slot_of(&many[1]);
    uint32_t keep = sc_dsp_state()->generation;
    if (ahead)
        ahead->generation = keep + 1u;
    CHECK(slot_of(&late) == NULL);
    if (ahead)
        ahead->generation = keep;
    /* Fewer events than the threshold reclaim nothing; reaching it frees stale slots. */
    for (int i = 0; i < SC_DSP_CHAIN_STALE_GENERATIONS - 1; i++)
        sc_dsp_fx_mark_transition(NULL, SC_DSP_FX_TRANSITION_SWITCH);
    CHECK(slot_of(&many[0]) == first);
    CHECK(slot_of(&late) == NULL);
    sc_dsp_fx_mark_transition(NULL, SC_DSP_FX_TRANSITION_SWITCH);
    CHECK(slot_of(&many[0]) == first);
    struct sc_dsp_chain *reclaimed = sc_dsp_chain_enter(&late);
    CHECK(reclaimed && reclaimed != first && reclaimed->ramp_remaining == SC_DSP_RAMP_FRAMES);
    CHECK(reclaimed && reclaimed->self == &late);
    sc_dsp_chain_exit(reclaimed);
    CHECK(slot_of(&many[0]) == first);                            /* the live one kept its slot */
    for (int i = 0; i < 8; i++) {
        in_l[i] = in_r[i] = 0.25f;
        out_l[i] = 1.0f;
        out_r[i] = i == 2 ? NAN : 1.0f;
    }
    sc_dsp_fx_common_post_process(NULL, in, out, 8, 8);
    CHECK(out_l[0] == 1.0f && out_r[2] == 0.25f);                 /* sanitized, not ramped */
}

/* A slot a call still holds is never taken over, however many FX events pass,
 * and the same common overlapping itself gets no chain (only sanitized). */
static void test_chain_busy(void)
{
    static char many[2 * SC_DSP_CHAIN_SLOTS];
    static float in_l[8], in_r[8], out_l[8], out_r[8];
    float *in[2] = {in_l, in_r}, *out[2] = {out_l, out_r};
    reset_chains();
    for (int i = 0; i < SC_DSP_CHAIN_SLOTS; i++)
        CHECK(slot_of(&many[i]) != NULL);
    struct sc_dsp_chain *held = sc_dsp_chain_enter(&many[0]);     /* in flight, not exited */
    CHECK(held && held->busy == 1);
    for (int i = 0; i < 3 * SC_DSP_CHAIN_STALE_GENERATIONS; i++)
        sc_dsp_fx_mark_transition(NULL, SC_DSP_FX_TRANSITION_SWITCH);
    /* Every other slot is now stale and gets reused; the held one must not be. */
    int refused = 0;
    for (int i = SC_DSP_CHAIN_SLOTS; i < 2 * SC_DSP_CHAIN_SLOTS; i++) {
        struct sc_dsp_chain *c = sc_dsp_chain_enter(&many[i]);
        CHECK(c != held);
        refused += c == NULL;
        sc_dsp_chain_exit(c);
    }
    CHECK(refused == 1 && held->self == &many[0] && held->busy == 1);
    /* Same common again while in flight: no chain, sanitize only. */
    CHECK(sc_dsp_chain_enter(&many[0]) == NULL);
    CHECK(held->busy == 1);                                       /* the failed try did not release it */
    for (int i = 0; i < 8; i++) {
        in_l[i] = in_r[i] = 0.25f;
        out_l[i] = 1.0f;
        out_r[i] = i == 3 ? INFINITY : 1.0f;
    }
    sc_dsp_fx_common_post_process(NULL, in, out, 8, 8);
    CHECK(out_l[0] == 1.0f && out_r[3] == 0.25f);
    sc_dsp_chain_exit(held);
    CHECK(held->busy == 0);
    /* Once released it is stale and free to reuse. */
    static char fresh;
    struct sc_dsp_chain *c = sc_dsp_chain_enter(&fresh);
    CHECK(c == held && c->self == &fresh && c->ramp_remaining == SC_DSP_RAMP_FRAMES);
    sc_dsp_chain_exit(c);
    sc_dsp_chain_exit(NULL);                                      /* NULL-safe */
}

/* Real threads: several "render" threads run enter/post/exit over a shared set
 * of commons (so some overlap on the same one) while another keeps firing FX
 * events. Under -fsanitize=thread this reports any unsynchronised slot access;
 * `inside` is deliberately plain, so a second owner of one slot would race. */
enum { THREADS = 4, COMMONS = 40, ITERS = 20000, BLOCK = 192 };
static char thread_commons[COMMONS];
static int inside[SC_DSP_CHAIN_SLOTS];
static volatile int stop_events;

static void *render_thread(void *arg)
{
    uintptr_t bad = 0;
    unsigned seed = (unsigned)(uintptr_t)arg * 2654435761u + 1;
    float in_l[BLOCK], in_r[BLOCK], out_l[BLOCK], out_r[BLOCK];
    float *in[2] = {in_l, in_r}, *out[2] = {out_l, out_r};
    for (int it = 0; it < ITERS; it++) {
        seed = seed * 1103515245u + 12345u;
        void *common = &thread_commons[(seed >> 16) % COMMONS];
        for (int i = 0; i < BLOCK; i++) {
            in_l[i] = in_r[i] = 0.0f;
            out_l[i] = out_r[i] = i == 5 ? NAN : 1.0f;
        }
        struct sc_dsp_chain *c = sc_dsp_chain_enter(common);
        if (c) {
            int idx = (int)(c - sc_dsp_state()->chain);
            if (++inside[idx] != 1) bad++;
            if (c->self != common || c->ramp_remaining < 0 || c->ramp_remaining > SC_DSP_RAMP_FRAMES) bad++;
        }
        sc_dsp_fx_common_post_process(c, in, out, BLOCK, BLOCK);
        if (c)
            inside[(int)(c - sc_dsp_state()->chain)]--;
        sc_dsp_chain_exit(c);
        for (int i = 0; i < BLOCK; i++)
            if (!sc_dsp_finite_f32(out_l[i]) || !sc_dsp_finite_f32(out_r[i]) ||
                out_l[i] < 0.0f || out_l[i] > 1.0f) {
                bad++;
                break;
            }
    }
    return (void *)bad;
}

static void *event_thread(void *arg)
{
    (void)arg;
    while (!__atomic_load_n(&stop_events, __ATOMIC_ACQUIRE)) {
        sc_dsp_fx_mark_transition(NULL, SC_DSP_FX_TRANSITION_CHANGE);
        sched_yield();
    }
    return NULL;
}

static void test_chain_threads(void)
{
    pthread_t render[THREADS], events;
    reset_chains();
    memset(inside, 0, sizeof inside);
    __atomic_store_n(&stop_events, 0, __ATOMIC_RELEASE);
    CHECK(pthread_create(&events, NULL, event_thread, NULL) == 0);
    for (uintptr_t t = 0; t < THREADS; t++)
        CHECK(pthread_create(&render[t], NULL, render_thread, (void *)(t + 1)) == 0);
    for (int t = 0; t < THREADS; t++) {
        void *bad = NULL;
        pthread_join(render[t], &bad);
        CHECK(bad == NULL);
    }
    __atomic_store_n(&stop_events, 1, __ATOMIC_RELEASE);
    pthread_join(events, NULL);
    for (int i = 0; i < SC_DSP_CHAIN_SLOTS; i++)
        CHECK(sc_dsp_state()->chain[i].busy == 0);                /* every try was paired with an exit */
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

/* #54: one Flanger-like block, dry level measured before, dipped in place. */
static void flanger_block(void *self, float *l, float *r, uint32_t n, float dip, uint32_t *t)
{
    float *buf[2] = {l, r};
    for (uint32_t i = 0; i < n; i++, (*t)++)
        l[i] = r[i] = 0.25f * sinf((float)*t * 0.01f);
    float dry = sc_dsp_block_energy(buf, n);
    for (uint32_t i = 0; i < n; i++) { l[i] *= dip; r[i] *= dip; }
    sc_dsp_makeup_apply(self, buf, n, dry);
}

static void test_flanger_makeup(void)
{
    enum { B = 128 };
    static float l[B], r[B];
    float *buf[2] = {l, r};
    static char fx[4];
    uint32_t t = 0;
    memset(sc_dsp_state(), 0, sizeof *sc_dsp_state());
    /* A 0.6x dip converges back to the dry level within a few hundred ms. */
    for (int i = 0; i < 2000; i++) flanger_block(&fx[0], l, r, B, 0.6f, &t);
    float in = 0.0f, out = 0.0f;
    for (int i = 0; i < 100; i++) {
        uint32_t t0 = t;
        flanger_block(&fx[0], l, r, B, 0.6f, &t);
        out += sc_dsp_block_energy(buf, B);
        t = t0;
        for (uint32_t k = 0; k < B; k++, t++) l[k] = r[k] = 0.25f * sinf((float)t * 0.01f);
        in += sc_dsp_block_energy(buf, B);
    }
    CHECK(fabsf(sqrtf(out / in) - 1.0f) < 0.02f);
    /* No dip, or a louder block: bit-exact passthrough, never attenuated. */
    for (float dip = 1.0f; dip <= 1.5f; dip += 0.5f) {
        flanger_block(&fx[1], l, r, B, dip, &t);
        float want = 0.25f * sinf((float)(t - 1) * 0.01f) * dip;
        CHECK(l[B - 1] == want && r[B - 1] == want);
    }
    /* A deep dip is capped at +6 dB; silence and non-finite input change nothing. */
    for (int i = 0; i < 2000; i++) flanger_block(&fx[2], l, r, B, 0.1f, &t);
    CHECK(sc_dsp_state()->makeup[2].gain <= SC_DSP_MAKEUP_MAX && sc_dsp_state()->makeup[2].gain > 1.9f);
    memset(l, 0, sizeof l); memset(r, 0, sizeof r);
    sc_dsp_makeup_apply(&fx[3], buf, B, 0.0f);
    sc_dsp_makeup_apply(&fx[3], buf, B, NAN);
    sc_dsp_makeup_apply(NULL, buf, B, 1.0f);
    sc_dsp_makeup_apply(&fx[3], NULL, B, 1.0f);
    CHECK(l[0] == 0.0f && r[B - 1] == 0.0f);
    /* The gain moves linearly within a block: no step at a block boundary. */
    flanger_block(&fx[0], l, r, B, 0.6f, &t);
    CHECK(sc_dsp_state()->makeup[0].gain > 1.6f && sc_dsp_state()->makeup[0].gain < 1.72f);
    /* More instances than slots: the table recycles, nothing breaks. */
    static char many[SC_DSP_MAKEUP_SLOTS * 2];
    for (unsigned i = 0; i < sizeof many; i++) flanger_block(&many[i], l, r, B, 0.5f, &t);
    for (uint32_t k = 0; k < B; k++) CHECK(sc_dsp_finite_f32(l[k]) && fabsf(l[k]) <= 0.5f);
}

static void *flanger_thread(void *arg)
{
    float l[64], r[64];
    uint32_t t = 0;
    for (int i = 0; i < 20000; i++) flanger_block(arg, l, r, 64, 0.7f, &t);
    return NULL;
}

static void test_flanger_threads(void)
{
    /* Each render thread its own instance, plus two sharing one (busy slot skipped). */
    static char fx[3];
    pthread_t th[4];
    void *who[4] = {&fx[0], &fx[1], &fx[2], &fx[2]};
    memset(sc_dsp_state(), 0, sizeof *sc_dsp_state());
    for (int i = 0; i < 4; i++) CHECK(!pthread_create(&th[i], NULL, flanger_thread, who[i]));
    for (int i = 0; i < 4; i++) CHECK(!pthread_join(th[i], NULL));
    for (int i = 0; i < SC_DSP_MAKEUP_SLOTS; i++) {
        const struct sc_dsp_makeup *m = &sc_dsp_state()->makeup[i];
        CHECK(!m->busy && (!m->self || (m->gain >= 1.0f && m->gain <= SC_DSP_MAKEUP_MAX)));
    }
}

/* #58 master soft clipper / peak limiter. */
static void test_master_limiter(void)
{
    /* Curve: bit-exact up to the knee, C1 at the knee, monotonic, flat into the ceiling. */
    for (float x = 0.0f; x <= SC_LIM_KNEE; x += 0.001f)
        CHECK(sc_lim_clip(x) == x && sc_lim_clip(-x) == -x);
    const float h = 1e-4f;
    CHECK(fabsf((sc_lim_clip(SC_LIM_KNEE + h) - SC_LIM_KNEE) / h - 1.0f) < 0.01f);
    CHECK((sc_lim_clip(SC_LIM_SAT) - sc_lim_clip(SC_LIM_SAT - h)) / h < 0.01f);
    float prev = 0.0f, max_slope = 0.0f, max_bend = 0.0f, prev_slope = 1.0f;
    for (int i = 1; i <= 20000; i++) {
        float x = 2.0f * (float)i / 20000.0f, y = sc_lim_clip(x), slope = (y - prev) / (2.0f / 20000.0f);
        CHECK(y >= prev - 1e-6f && y <= SC_LIM_CEIL); /* 1 ulp of rounding on the flat top */
        if (slope > max_slope) max_slope = slope;
        if (fabsf(slope - prev_slope) > max_bend) max_bend = fabsf(slope - prev_slope);
        prev = y, prev_slope = slope;
    }
    CHECK(max_slope < 1.01f);
    CHECK(max_bend < 0.01f); /* no slope jump anywhere: a hard clip jumps by 1 */
    CHECK(sc_lim_clip(NAN) == 0.0f && sc_lim_clip(INFINITY) == 0.0f && sc_lim_clip(-INFINITY) == 0.0f);

    /* +6 dB overdriven sine: peaks stay at or below -0.1 dBFS and finite, and the
     * harsh high harmonics (7th..21st) are far below a hard clip at the same
     * ceiling. 9600 frames hold exactly 100 periods of 1 kHz at 96 kHz. */
    enum { N = 9600 };
    static float l[N], r[N], hard[N];
    struct sc_lim s = {0};
    float peak = 0.0f;
    for (int i = 0; i < N; i++) {
        float x = 2.0f * sinf(2.0f * 3.14159265f * 1000.0f * (float)i / 96000.0f);
        l[i] = x, r[i] = 0.5f * x;
        hard[i] = x > SC_LIM_CEIL ? SC_LIM_CEIL : x < -SC_LIM_CEIL ? -SC_LIM_CEIL : x;
    }
    sc_lim_planar(&s, l, r, N);
    for (int i = 0; i < N; i++) {
        CHECK(isfinite(l[i]) && isfinite(r[i]));
        if (fabsf(l[i]) > peak) peak = fabsf(l[i]);
        CHECK(fabsf(r[i]) <= SC_LIM_KNEE); /* linked: the quieter side is only turned down */
    }
    CHECK(peak <= SC_LIM_CEIL && peak > 0.95f);
    double soft_hi = 0.0, hard_hi = 0.0;
    for (int k = 7; k <= 21; k += 2) {
        double sr = 0, si = 0, hr = 0, hi = 0;
        for (int i = 0; i < N; i++) {
            double w = 2.0 * 3.14159265358979 * 100.0 * k * i / N;
            sr += l[i] * cos(w), si += l[i] * sin(w);
            hr += hard[i] * cos(w), hi += hard[i] * sin(w);
        }
        soft_hi += sr * sr + si * si, hard_hi += hr * hr + hi * hi;
    }
    CHECK(soft_hi < 0.25 * hard_hi); /* measured 9.9 dB less high-order energy; require 6 */

    /* After the overdrive stops, the envelope releases back to unity. */
    for (int i = 0; i < N; i++) l[i] = r[i] = 0.5f;
    sc_lim_planar(&s, l, r, N);
    CHECK(s.env == 0.0f && l[N - 1] == 0.5f && r[N - 1] == 0.5f);

    /* Below the knee nothing is touched, bit for bit; a lone channel works too. */
    float quiet[256], copy[256];
    for (int i = 0; i < 256; i++) quiet[i] = copy[i] = 0.84f * sinf((float)i * 0.37f);
    struct sc_lim d = {0};
    sc_lim_planar(&d, quiet, NULL, 256);
    CHECK(!memcmp(quiet, copy, sizeof quiet));
    sc_lim_planar(NULL, quiet, NULL, 256);
    sc_lim_planar(&d, NULL, quiet, 256);
    CHECK(!memcmp(quiet, copy, sizeof quiet));

    /* NaN/Inf become silence and do not poison the envelope. */
    float bl[3] = {NAN, INFINITY, 0.5f}, br[3] = {-INFINITY, 0.25f, NAN};
    struct sc_lim e = {0};
    sc_lim_planar(&e, bl, br, 3);
    CHECK(bl[0] == 0.0f && bl[1] == 0.0f && bl[2] == 0.5f && br[0] == 0.0f && br[1] == 0.25f && br[2] == 0.0f);
    CHECK(e.env == 0.0f);
}

/* #64 Auto-Gain into the #58 limiter, the way hook_master_output runs them. */
static float gain_run(struct sc_gain *g, struct sc_lim *lim, float amp, int blocks, float *peak_out)
{
    enum { B = 512 };
    float l[B], r[B], peak = 0.0f;
    for (int b = 0; b < blocks; b++) {
        for (int i = 0; i < B; i++)
            l[i] = r[i] = amp * sinf(2.0f * 3.14159265f * 1000.0f * (float)(b * B + i) / 96000.0f);
        sc_gain_planar(g, l, r, B);
        sc_lim_planar(lim, l, r, B);
        for (int i = 0; i < B; i++) {
            CHECK(isfinite(l[i]) && fabsf(l[i]) <= SC_LIM_CEIL && fabsf(r[i]) <= SC_LIM_CEIL);
            if (b >= blocks - 20 && fabsf(l[i]) > peak) peak = fabsf(l[i]);
        }
    }
    *peak_out = peak;
    return g->gain;
}

static void test_auto_gain(void)
{
    float peak;
    /* 0.35 (-9 dBFS) is brought up near 0 dBFS within 10 s, never past the ceiling. */
    struct sc_gain g = {0};
    struct sc_lim lim = {0};
    float gain = gain_run(&g, &lim, 0.35f, 1875, &peak);
    CHECK(gain > 2.6f && gain <= SC_GAIN_MAX && peak > 0.85f);
    /* Then a loud 1.8 passage: gain falls back to unity fast, the limiter holds the ceiling. */
    gain = gain_run(&g, &lim, 1.8f, 200, &peak);
    CHECK(gain == 1.0f && peak <= SC_LIM_CEIL);

    /* Under the -42 dBFS gate nothing is boosted, from a fresh start or after music. */
    struct sc_gain q = {0};
    struct sc_lim ql = {0};
    gain = gain_run(&q, &ql, 0.007f, 1875, &peak);
    CHECK(q.gain == 1.0f && q.level == 0.0f && peak < 0.0071f);
    struct sc_gain m = {0};
    struct sc_lim ml = {0};
    gain_run(&m, &ml, 0.5f, 1875, &peak);
    float held = m.gain;
    gain_run(&m, &ml, 0.002f, 1875, &peak);
    CHECK(m.gain == held && peak < 0.002f * held + 1e-4f); /* frozen, not raised further */

    /* Per-sample ramp: no step larger than the block's gain change. */
    struct sc_gain z = {.level = 0.2f, .gain = 1.0f};
    float l[512], r[512];
    for (int i = 0; i < 512; i++) l[i] = r[i] = 0.2f;
    sc_gain_planar(&z, l, r, 512);
    CHECK(z.gain > 1.0f && l[511] == 0.2f * z.gain);
    for (int i = 1; i < 512; i++) CHECK(l[i] >= l[i - 1] && l[i] - l[i - 1] < 1e-4f);

    /* NaN/Inf do not drive the level; NULL is safe. */
    float bl[3] = {NAN, INFINITY, 0.5f};
    struct sc_gain n = {0};
    sc_gain_planar(&n, bl, NULL, 3);
    CHECK(isfinite(n.level) && n.level == 0.5f);
    sc_gain_planar(NULL, bl, NULL, 3);
    sc_gain_planar(&n, NULL, NULL, 3);

    /* At unity (fresh, or gated) the block is left bit for bit; the hook's off path skips it entirely. */
    float quiet[256], copy[256];
    for (int i = 0; i < 256; i++) quiet[i] = copy[i] = 0.0079f * sinf((float)i * 0.37f);
    struct sc_gain u = {0};
    sc_gain_planar(&u, quiet, quiet, 256);
    CHECK(!memcmp(quiet, copy, sizeof quiet));
}

/* #104 Echo Out: music plays 1 s, goes silent, the last beat rings out for 2.2 s. */
static struct sc_echo echo_state; /* 384 KB: not on the stack */

static float echo_block(float amp, int silent, int enabled, float *rms)
{
    enum { B = 256 };
    static unsigned n;
    float l[B], r[B], sum = 0.0f;
    for (int i = 0; i < B; i++, n++)
        l[i] = r[i] = silent ? 0.0f : amp * sinf(2.0f * 3.14159265f * 440.0f * (float)n / 96000.0f);
    sc_echo_planar(&echo_state, l, r, B, enabled);
    for (int i = 0; i < B; i++) {
        CHECK(isfinite(l[i]) && l[i] == r[i]);
        sum += l[i] * l[i];
    }
    *rms = sqrtf(sum / B);
    return l[B - 1];
}

static void test_echo_out(void)
{
    float rms;
    memset(&echo_state, 0, sizeof echo_state);
    /* Music, then silence: while music plays the signal is untouched. */
    for (int b = 0; b < 400; b++) { echo_block(0.5f, 0, 1, &rms); CHECK(rms > 0.3f && rms < 0.4f); }
    CHECK(echo_state.tail == 0);
    /* The first silent block already carries the echo (zero latency), and it is as loud as the music. */
    echo_block(0.5f, 1, 1, &rms);
    CHECK(echo_state.tail > 0 && rms > 0.25f);
    /* The tail decays to nothing within 2.2 s (211200 frames = 825 blocks) and then stays silent. */
    float prev_peak = rms, early = 0.0f, late = 0.0f;
    for (int b = 1; b < 900; b++) {
        echo_block(0.5f, 1, 1, &rms);
        if (b == 100) early = rms;
        if (b == 700) late = rms;
        if (b > 830) CHECK(rms == 0.0f);
    }
    CHECK(early > 0.0f && late < early && late < prev_peak * 0.2f && echo_state.tail == 0);
    /* Silence again does not retrigger: the loud counter was reset. */
    echo_block(0.5f, 1, 1, &rms);
    CHECK(rms == 0.0f && echo_state.tail == 0);
    /* A short blip (< 0.5 s of music) does not trigger an echo. */
    for (int b = 0; b < 40; b++) echo_block(0.5f, 0, 1, &rms);
    echo_block(0.5f, 1, 1, &rms);
    CHECK(rms == 0.0f && echo_state.tail == 0);
    /* Disabled: bit-exact pass-through and no trigger; disabling mid-tail stops it at once. */
    for (int b = 0; b < 400; b++) echo_block(0.5f, 0, 0, &rms);
    echo_block(0.5f, 1, 0, &rms);
    CHECK(rms == 0.0f && echo_state.tail == 0);
    for (int b = 0; b < 400; b++) echo_block(0.5f, 0, 1, &rms);
    echo_block(0.5f, 1, 1, &rms);
    CHECK(echo_state.tail > 0);
    echo_block(0.5f, 1, 0, &rms);
    CHECK(rms == 0.0f && echo_state.tail == 0);
    /* NaN in, finite out; NULL arguments and a mono pair are safe. */
    float nl[4] = {NAN, INFINITY, 0.1f, 0.1f};
    sc_echo_planar(&echo_state, nl, NULL, 4, 1);
    sc_echo_planar(NULL, nl, NULL, 4, 1);
    sc_echo_planar(&echo_state, NULL, NULL, 4, 1);
    for (unsigned i = 0; i < SC_ECHO_DELAY; i++) CHECK(isfinite(echo_state.l[i]) && isfinite(echo_state.r[i]));
}

int main(void)
{
    test_finite();
    test_autogate();
    test_post_process();
    test_two_chains();
    test_chain_full();
    test_chain_busy();
    test_chain_threads();
    test_fader();
    test_flanger_makeup();
    test_flanger_threads();
    test_master_limiter();
    test_auto_gain();
    test_echo_out();
    if (failures) {
        fprintf(stderr, "%d check(s) failed\n", failures);
        return EXIT_FAILURE;
    }
    puts("dsp_smooth: all checks passed");
    return EXIT_SUCCESS;
}
