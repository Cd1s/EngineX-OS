/* gcc -std=c11 -O2 -Wall -Wextra -Werror tools/ui-mod/test_dsp_smooth.c -o /tmp/test_dsp_smooth && /tmp/test_dsp_smooth */
#include <math.h>
#include <float.h>
#include <pthread.h>
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

static float phase_of(const unsigned char *p) { float f; memcpy(&f,p+0x1e8,4); return f; }
static void render(void *owner,float *const out[2],uint32_t n) { sc_dsp_fx_end(sc_dsp_fx_begin(owner),out,n); }
static void fill(float *const out[2],unsigned n,float value) {
    for(unsigned ch=0;ch<2;ch++)for(unsigned i=0;i<n;i++)out[ch][i]=value;
}
static void test_isolation_and_turnoff(void) {
    memset(sc_dsp_state(),0,sizeof *sc_dsp_state());
    static int a,b; unsigned char gate_a[512]={0},gate_b[512]={0};
    float phase=.75f;memcpy(gate_a+0x1e8,&phase,4);memcpy(gate_b+0x1e8,&phase,4);
    struct sc_dsp_fx_slot *sa=sc_dsp_fx_begin(&a),*sb=sc_dsp_fx_begin(&b);
    CHECK(sa&&sb&&sa!=sb);CHECK(!sc_dsp_fx_begin(&a));
    sc_dsp_autogate_for_slot(sa,gate_a,1);sc_dsp_autogate_for_slot(sb,gate_b,1);
    sc_dsp_fx_end(sa,NULL,0);sc_dsp_fx_end(sb,NULL,0);
    float l[256],r[256],*out[2]={l,r};
    fill(out,256,1);render(&a,out,256);fill(out,256,2);render(&b,out,256);
    sc_dsp_fx_mark_transition(&a,SC_DSP_FX_TRANSITION_TURN_OFF);
    sb=sc_dsp_fx_begin(&b);sc_dsp_autogate_for_slot(sb,gate_b,1);
    fill(out,256,2);sc_dsp_fx_end(sb,out,256);CHECK(l[0]==2&&phase_of(gate_b)==.75f);
    sa=sc_dsp_fx_begin(&a);sc_dsp_autogate_for_slot(sa,gate_a,1);CHECK(phase_of(gate_a)==0);
    phase=.4f;memcpy(gate_a+0x1e8,&phase,4);sc_dsp_autogate_for_slot(sa,gate_a,1);CHECK(phase_of(gate_a)==.4f);
    fill(out,256,0);sc_dsp_fx_end(sa,out,64);CHECK(l[0]==1&&l[63]==65.0f/128);
    fill(out,256,0);render(&a,out,192);CHECK(l[0]==.5f&&l[63]==1.0f/128&&l[64]==0);
    CHECK(sa->remaining==0);
    // Re-arm from the ACTUAL last emitted sample, including halfway through OFF.
    fill(out,256,1);render(&a,out,256);
    sc_dsp_fx_mark_transition(&a,SC_DSP_FX_TRANSITION_TURN_OFF);
    fill(out,256,0);render(&a,out,32);float last=l[31];
    sc_dsp_fx_mark_transition(&a,SC_DSP_FX_TRANSITION_TURN_ON);
    fill(out,256,1);render(&a,out,256);CHECK(l[0]==last&&l[128]==1);
    // Finite extreme interpolation cannot overflow wet-dry in float arithmetic.
    fill(out,256,FLT_MAX);render(&a,out,256);
    sc_dsp_fx_mark_transition(&a,SC_DSP_FX_TRANSITION_CHANGE);
    fill(out,256,-FLT_MAX);render(&a,out,256);
    for(unsigned i=0;i<256;i++)CHECK(sc_dsp_finite_f32(l[i])&&sc_dsp_finite_f32(r[i]));
    puts("FX owner isolation, gate phase, OFF continuity, re-arm and finite extremes: PASS");
}
static void test_stateless_and_exhaustion(void) {
    memset(sc_dsp_state(),0,sizeof *sc_dsp_state());
    float l[]={NAN,INFINITY,-INFINITY,.25f},r[]={1,2,3,4};float *out[]={l,r};
    sc_dsp_fx_common_post_process(NULL,out,out,4);CHECK(l[0]==0&&l[1]==0&&l[2]==0&&l[3]==.25f);
    sc_dsp_sweep_fx_post_process(NULL,out,4);CHECK(r[3]==4);
    for(unsigned i=1;i<=SC_DSP_FX_SLOTS;i++) CHECK(sc_dsp_fx_lookup((void *)(uintptr_t)(i*4096),1));
    void *extra=(void *)(uintptr_t)((SC_DSP_FX_SLOTS+1)*4096);
    CHECK(!sc_dsp_fx_begin(extra));sc_dsp_fx_mark_transition(extra,SC_DSP_FX_TRANSITION_SWITCH);
    CHECK(sc_dsp_state()->slot_misses==2);
    for(unsigned i=1;i<=SC_DSP_FX_SLOTS;i++) CHECK(sc_dsp_fx_lookup((void *)(uintptr_t)(i*4096),0)->event==0);
    sc_dsp_fx_mark_transition(NULL,SC_DSP_FX_TRANSITION_SWITCH);
    l[0]=NAN;sc_dsp_fx_end(NULL,out,1);CHECK(l[0]==0);
    uint32_t before=sc_dsp_state()->sampler_generation;
    sc_dsp_sampler_mark_event(NULL,SC_DSP_SAMPLER_PLAY);CHECK(sc_dsp_state()->sampler_generation==before+1);
    puts("Slot exhaustion is stateless, no eviction/cross-owner fallback; sanitation/Sampler: PASS");
}
static int threaded_owner;
static void *producer(void *unused) {
    (void)unused;
    for(unsigned i=0;i<5000;i++)sc_dsp_fx_mark_transition(&threaded_owner,SC_DSP_FX_TRANSITION_SWITCH);
    return NULL;
}
static void test_concurrent_marks(void) {
    memset(sc_dsp_state(),0,sizeof *sc_dsp_state());
    pthread_t threads[4];for(unsigned i=0;i<4;i++)CHECK(!pthread_create(&threads[i],NULL,producer,NULL));
    float l[32],r[32],*out[]={l,r};
    for(unsigned i=0;i<2000;i++){fill(out,32,1);render(&threaded_owner,out,32);CHECK(sc_dsp_finite_f32(l[0]));}
    for(unsigned i=0;i<4;i++)CHECK(!pthread_join(threads[i],NULL));
    struct sc_dsp_fx_slot *f=sc_dsp_fx_lookup(&threaded_owner,0);
    CHECK((__atomic_load_n(&f->event,__ATOMIC_ACQUIRE)>>3)==20000);
    fill(out,32,0);render(&threaded_owner,out,32);CHECK(f->seen_event==f->event);
    puts("Four control producers / audio consumer: 20000 transitions retained: PASS");
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
    test_isolation_and_turnoff();
    test_stateless_and_exhaustion();
    test_concurrent_marks();
    test_fader();
    if (failures) {
        fprintf(stderr, "%d check(s) failed\n", failures);
        return EXIT_FAILURE;
    }
    puts("dsp_smooth: all checks passed");
    return EXIT_SUCCESS;
}
