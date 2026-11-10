/* Native the build host: gcc -std=c11 -O2 -Wall -Wextra -Werror -pthread
 * tools/ui-mod/test_pioneer_selector.c -lm -o out/test_pioneer_selector
 * Repeat with -fsanitize=address,undefined and -fsanitize=thread. */
#define _GNU_SOURCE
#include <math.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "fx_selector.h"

static int failures;
#define CHECK(x) do { if (!(x)) { fprintf(stderr, "%d: %s\n", __LINE__, #x); ++failures; } } while (0)
static struct sc_pfx_instance fx[3];
static float fixture(unsigned n) { return 0.2f * sinf((float)n * 0.027f); }

/* Stand-in for the ordinary worker between callback blocks. */
static int prepared_process(struct sc_pfx_instance *s, float *l, float *r, unsigned frames,
                            int mode, float time, float param, float wet) {
    sc_pfx_instance_prepare(s);
    return sc_pfx_process(s, l, r, frames, mode, time, param, wet);
}

static double render(int mode, float time, float param, float wet) {
    memset(&fx[0], 0, sizeof fx[0]);
    double signature = 0;
    for (unsigned n = 0; n < 96000; n += 128) {
        float l[128], r[128], dl[128], dr[128];
        for (unsigned i = 0; i < 128; ++i) {
            dl[i] = l[i] = fixture(n + i);
            dr[i] = r[i] = fixture(n + i + 12);
        }
        prepared_process(&fx[0], l, r, 128, mode, time, param, wet);
        if (wet == 0) {
            CHECK(!memcmp(l, dl, sizeof l)); CHECK(!memcmp(r, dr, sizeof r));
        }
        for (unsigned i = 0; i < 128; ++i) {
            CHECK(isfinite(l[i]) && isfinite(r[i]));
            CHECK(fabsf(l[i]) <= 4 && fabsf(r[i]) <= 4);
            signature += (double)(l[i] * l[i] + r[i] * r[i]);
        }
    }
    return signature;
}

static void test_modes(void) {
    const float values[] = {0, 0.3f, 1};
    for (int mode = 1; mode <= 5; ++mode) {
        for (unsigned t = 0; t < 3; ++t)
            for (unsigned p = 0; p < 3; ++p)
                for (unsigned w = 0; w < 3; ++w)
                    render(mode, values[t], values[p], values[w]);
        double a = render(mode, 0.1f, 0.2f, 1);
        double b = render(mode, 0.9f, 0.2f, 1);
        double c = render(mode, 0.1f, 0.8f, 1);
        CHECK(fabs(a - b) > 0.001);
        /* Slip Roll has no character/feedback parameter in the existing DSP. */
        if (mode != SC_PFX_SLIP_ROLL) CHECK(fabs(a - c) > 0.001);
        else CHECK(a == c);
    }
    /* Literal bypass includes signed zeros and invalid source samples. */
    memset(&fx[0], 0, sizeof fx[0]);
    float l[] = {-0.0f, NAN, INFINITY, -INFINITY}, r[4], original[4];
    memcpy(r, l, sizeof l); memcpy(original, l, sizeof l);
    prepared_process(&fx[0], l, r, 4, 3, NAN, INFINITY, 0);
    CHECK(!memcmp(l, original, sizeof l) && !memcmp(r, original, sizeof r));
}

static void test_switch(void) {
    memset(&fx[0], 0, sizeof fx[0]);
    float previous = 0;
    for (unsigned block = 0; block < 500; ++block) {
        float l[128], r[128];
        for (unsigned i = 0; i < 128; ++i) l[i] = r[i] = 0.1f;
        int mode = block < 200 ? 1 : 1 + (int)((block / 4) % 5);
        prepared_process(&fx[0], l, r, 128, mode, 0.3f, 0.5f, 0.7f);
        if (block > 10) CHECK(fabsf(l[0] - previous) < 0.02f);
        previous = l[127];
    }
}

static void test_roll_time_increase(void) {
    memset(&fx[0], 0, sizeof fx[0]);
    for (unsigned block = 0; block < 1000; ++block) {
        float l[128], r[128];
        for (unsigned i = 0; i < 128; ++i) l[i] = r[i] = 0.1f;
        prepared_process(&fx[0], l, r, 128, SC_PFX_SLIP_ROLL, block < 500 ? 0.1f : 1.0f, 0.5f, 1.0f);
        for (unsigned i = 0; i < 128; ++i) CHECK(l[i] > 0.09f && r[i] > 0.09f);
    }
    sc_pfx_instance_reset(&fx[0]);
    for (unsigned block = 0; block < 400; ++block) {
        float l[128] = {0}, r[128] = {0};
        prepared_process(&fx[0], l, r, 128, SC_PFX_SPACE, 0.5f, 0.5f, 1.0f);
        for (unsigned i = 0; i < 128; ++i) CHECK(l[i] == 0 && r[i] == 0);
    }
}

static void *independent(void *arg) {
    struct sc_pfx_instance *s = arg;
    for (unsigned n = 0; n < 1000; ++n) {
        float l[64] = {0.1f}, r[64] = {0.2f};
        prepared_process(s, l, r, 64, 1 + n % 5, 0.5f, 0.5f, 0.5f);
    }
    return NULL;
}

static void test_wet_zero_fade(void) {
    for (int mode = 1; mode <= 5; ++mode) {
        memset(&fx[0], 0, sizeof fx[0]);
        float previous = 0;
        for (unsigned n = 0; n < 120000; n += 128) {
            float l[128], r[128], dry[128];
            for (unsigned i = 0; i < 128; ++i) dry[i] = l[i] = r[i] = fixture(n + i);
            float wet = n < 96000 ? 0.8f : 0.0f;
            CHECK(prepared_process(&fx[0], l, r, 128, mode, 0.5f, 0.5f, wet));
            if (n >= 96000 && n < 100096) {
                for (unsigned i = 0; i < 128; ++i) {
                    CHECK(fabsf(l[i] - previous) < 0.025f);
                    previous = l[i];
                }
            } else previous = l[127];
            if (n >= 100096) CHECK(!memcmp(l, dry, sizeof l));
        }
        CHECK(fx[0].wet == 0);
    }
    puts("test_wet_zero_fade PASS");
}

static void test_worker_banks(void) {
    memset(&fx[0], 0, sizeof fx[0]);
    float l[512] = {0}, r[512] = {0};
    CHECK(!sc_pfx_process(&fx[0], l, r, 512, 1, 0.5f, 0.5f, 1));
    sc_pfx_instance_prepare(&fx[0]);
    /* Poison an unused delay cell: rendering may not secretly clear it. */
    fx[0].bank[0].buf_l[SC_PFX_RING_SIZE - 1000] = 0.75f;
    CHECK(sc_pfx_process(&fx[0], l, r, 512, 1, 0.5f, 0.5f, 1));
    CHECK(fx[0].bank[0].buf_l[SC_PFX_RING_SIZE - 1000] == 0.75f);
    CHECK(sc_pfx_process(&fx[0], l, r, 512, 2, 0.5f, 0.5f, 1));
    CHECK(fx[0].mode == 2);
    /* No prepared spare yet: retain mode 2 without clearing on audio thread. */
    CHECK(sc_pfx_process(&fx[0], l, r, 512, 3, 0.5f, 0.5f, 1));
    CHECK(fx[0].mode == 2);
    sc_pfx_instance_prepare(&fx[0]);
    CHECK(fx[0].bank[0].buf_l[SC_PFX_RING_SIZE - 1000] == 0);
    CHECK(sc_pfx_process(&fx[0], l, r, 512, 3, 0.5f, 0.5f, 1));
    CHECK(fx[0].mode == 3);
    sc_pfx_instance_reset(&fx[0]);
    sc_pfx_instance_prepare(&fx[0]);
    memset(l, 0, sizeof l); memset(r, 0, sizeof r);
    CHECK(sc_pfx_process(&fx[0], l, r, 512, 3, 0.5f, 0.5f, 1));
    for (unsigned i = 0; i < 512; ++i) CHECK(l[i] == 0 && r[i] == 0);
    puts("test_worker_banks PASS");
}

static void test_double_reset_no_prepare(void) {
    memset(&fx[0], 0, sizeof fx[0]);
    sc_pfx_instance_prepare(&fx[0]);
    float l[128], r[128], dry[128];
    for (unsigned i = 0; i < 128; ++i) l[i] = r[i] = dry[i] = fixture(i);
    CHECK(sc_pfx_process(&fx[0], l, r, 128, 1, 0.5f, 0.5f, 0.8f));
    sc_pfx_instance_reset(&fx[0]);
    CHECK(sc_pfx_process(&fx[0], l, r, 128, 1, 0.5f, 0.5f, 0.8f));
    sc_pfx_instance_reset(&fx[0]);
    memcpy(l, dry, sizeof l); memcpy(r, dry, sizeof r);
    CHECK(!sc_pfx_process(&fx[0], l, r, 128, 1, 0.5f, 0.5f, 0.8f));
    CHECK(!memcmp(l, dry, sizeof l) && !memcmp(r, dry, sizeof r));
    CHECK(fx[0].wet == 0);
    puts("test_double_reset_no_prepare PASS");
}

static void test_smoother_denormals(void) {
    memset(&fx[0], 0, sizeof fx[0]);
    sc_pfx_instance_prepare(&fx[0]);
    fx[0].time = fx[0].parameter = fx[0].wet = 1.0e-25f;
    float l = 0, r = 0;
    CHECK(sc_pfx_process(&fx[0], &l, &r, 1, 1, 0, 0, 0));
    CHECK(fx[0].time == 0 && fx[0].parameter == 0 && fx[0].wet == 0);
    puts("test_smoother_denormals PASS");
}

static void test_arm_arm(void) {
    int owner, effect;
    struct sc_pfx_route *r = &sc_pfx_routes[0];
    memset(sc_pfx_routes, 0, sizeof sc_pfx_routes);
    sc_pfx_arm(0, 1, 0x35800000, 0x36000000);
    CHECK(sc_pfx_bind(0, &owner, &effect)); sc_pfx_activate(r);
    sc_pfx_arm(0, 0, 0, 0); /* FX off: no process/restore between selections */
    CHECK(__atomic_load_n(&r->restore_mix, __ATOMIC_ACQUIRE));
    sc_pfx_arm(0, 2, 0x35800000, 0x36000000);
    CHECK(sc_pfx_bind(0, &owner, &effect)); sc_pfx_activate(r);
    CHECK(!__atomic_load_n(&r->restore_mix, __ATOMIC_ACQUIRE));
    CHECK(__atomic_load_n(&r->native_override, __ATOMIC_ACQUIRE));
    sc_pfx_reset_owner(&owner);
    CHECK(__atomic_load_n(&r->reset_epoch, __ATOMIC_ACQUIRE) == 1);
    sc_pfx_forget_owner(&owner);
    puts("test_arm_arm PASS");
}

static void test_routes(void) {
    int owner[2], effect[2];
    unsigned mode = 0;
    CHECK(!sc_pfx_enter(&effect[0], &mode));
    sc_pfx_select(0, 1); sc_pfx_select(1, 2);
    CHECK(sc_pfx_bind(0, &owner[0], &effect[0]));
    CHECK(sc_pfx_bind(1, &owner[1], &effect[1]));
    sc_pfx_control(&owner[0], 0, 0.25f);
    sc_pfx_control(&owner[1], 0, 0.75f);
    struct sc_pfx_route *r = sc_pfx_enter(&effect[0], &mode);
    CHECK(r == &sc_pfx_routes[0] && mode == 1);
    CHECK(sc_pfx_float(__atomic_load_n(&r->mix, __ATOMIC_ACQUIRE)) == 0.25f);
    CHECK(!sc_pfx_enter(&effect[0], &mode)); /* contention never waits */
    sc_pfx_leave(r);
    int buffers[2];
    __atomic_store_n(&sc_pfx_routes[0].input_a, &buffers[0], __ATOMIC_RELEASE);
    CHECK(!sc_pfx_enter_input(&effect[0], &buffers[1], &mode));
    r = sc_pfx_enter_input(&effect[0], &buffers[0], &mode);
    CHECK(r == &sc_pfx_routes[0]); sc_pfx_leave(r);
    r = sc_pfx_enter(&effect[1], &mode);
    CHECK(r == &sc_pfx_routes[1] && mode == 2);
    CHECK(sc_pfx_float(__atomic_load_n(&r->mix, __ATOMIC_ACQUIRE)) == 0.75f);
    sc_pfx_leave(r);
    sc_pfx_select(0, 3);
    r = sc_pfx_enter(&effect[0], &mode);
    CHECK(r && mode == 3); /* same carrier switches without a stock gap */
    sc_pfx_leave(r);
    CHECK(sc_pfx_bind(0, &owner[0], &effect[0]));
    sc_pfx_unbind_owner(&owner[0]);
    CHECK(!sc_pfx_enter(&effect[0], &mode));
    sc_pfx_select(0, 0); /* stock Flanger must be available */
    CHECK(!sc_pfx_bind(0, &owner[0], &effect[0]));
    CHECK(!sc_pfx_enter(&effect[0], &mode));
    sc_pfx_select(0, 1);
    CHECK(sc_pfx_bind(0, &owner[0], &effect[1]));
    CHECK(!sc_pfx_enter(&effect[1], &mode)); /* ambiguous -> stock */
    sc_pfx_select(0, 0); sc_pfx_select(1, 0);
    unsigned a = 0x35812340, b = 0x36045670;
    CHECK(!sc_pfx_arm(0, 1, a, b));
    CHECK(sc_pfx_challenge(&owner[0], sc_pfx_float(a)));
    CHECK(sc_pfx_challenge(&owner[1], sc_pfx_float(b)));
    CHECK(!sc_pfx_bind_context(&owner[1], &effect[1]));
    CHECK(sc_pfx_challenge(&owner[0], sc_pfx_float(a)));
    CHECK(sc_pfx_challenge(&owner[0], sc_pfx_float(b)));
    CHECK(sc_pfx_bind_context(&owner[0], &effect[0]));
    CHECK(sc_pfx_arm(0, 2, a, b)); /* proven carrier can change mode atomically */
    r = sc_pfx_enter(&effect[0], &mode); CHECK(r && mode == 2); sc_pfx_leave(r);
    sc_pfx_unbind_owner(&owner[0]);
    CHECK(!sc_pfx_bind_context(&owner[0], &effect[0]));
    sc_pfx_arm(0, 3, a, b);
    sc_pfx_challenge(&owner[0], sc_pfx_float(a));
    sc_pfx_challenge(&owner[0], sc_pfx_float(b));
    CHECK(sc_pfx_bind_context(&owner[0], &effect[0]));
    __atomic_store_n(&sc_pfx_routes[0].native_owner, &owner[0], __ATOMIC_RELEASE);
    __atomic_store_n(&sc_pfx_routes[0].mix_handler, &owner[0], __ATOMIC_RELEASE);
    sc_pfx_forget_owner(&owner[0]);
    CHECK(!sc_pfx_enter(&effect[0], &mode));
    CHECK(!__atomic_load_n(&sc_pfx_routes[0].mix_handler, __ATOMIC_ACQUIRE));
    sc_pfx_select(0, 0);
}

static int concurrent_owner, concurrent_effect;
static pthread_barrier_t route_start;
static void *route_writer(void *unused) {
    (void)unused; pthread_barrier_wait(&route_start);
    for (unsigned n = 0; n < 20000; ++n) {
        if (!(n % 5)) sc_pfx_unbind_owner(&concurrent_owner);
        sc_pfx_arm(0, 1 + n % 5, 0x35800000, 0x36000000);
        sc_pfx_challenge(&concurrent_owner, sc_pfx_float(0x35800000));
        sc_pfx_challenge(&concurrent_owner, sc_pfx_float(0x36000000));
        sc_pfx_bind_context(&concurrent_owner, &concurrent_effect);
        sc_pfx_instance_prepare(&sc_pfx_routes[0].dsp);
        sc_pfx_control(&concurrent_owner, 0, 0.5f);
        sc_pfx_control(&concurrent_owner, 1, (float)(n % 100) / 100.0f);
    }
    return NULL;
}
static void *route_audio(void *unused) {
    (void)unused; pthread_barrier_wait(&route_start);
    for (unsigned n = 0; n < 2000; ++n) {
        unsigned mode;
        struct sc_pfx_route *r = sc_pfx_enter(&concurrent_effect, &mode);
        if (!r) continue;
        float l[64] = {0.1f}, right[64] = {0.2f};
        float mix = sc_pfx_float(__atomic_load_n(&r->mix, __ATOMIC_ACQUIRE));
        float param = sc_pfx_float(__atomic_load_n(&r->parameter, __ATOMIC_ACQUIRE));
        sc_pfx_process(&r->dsp, l, right, 64, mode, param, param, mix);
        for (unsigned i = 0; i < 64; ++i) if (!isfinite(l[i]) || !isfinite(right[i])) __atomic_fetch_add(&failures, 1, __ATOMIC_RELAXED);
        sc_pfx_leave(r);
    }
    return NULL;
}

static void test_concurrent_routes(void) {
    pthread_t writer, a, b;
    CHECK(!pthread_barrier_init(&route_start, NULL, 3));
    CHECK(!pthread_create(&writer, NULL, route_writer, NULL));
    CHECK(!pthread_create(&a, NULL, route_audio, NULL));
    CHECK(!pthread_create(&b, NULL, route_audio, NULL));
    CHECK(!pthread_join(writer, NULL)); CHECK(!pthread_join(a, NULL)); CHECK(!pthread_join(b, NULL));
    CHECK(!pthread_barrier_destroy(&route_start));
    sc_pfx_forget_owner(&concurrent_owner);
}

int main(void) {
    test_wet_zero_fade(); test_worker_banks(); test_double_reset_no_prepare(); test_smoother_denormals(); test_arm_arm();
    test_modes(); test_switch(); test_roll_time_increase(); test_routes(); test_concurrent_routes();
    pthread_t a, b;
    CHECK(!pthread_create(&a, NULL, independent, &fx[1]));
    CHECK(!pthread_create(&b, NULL, independent, &fx[2]));
    CHECK(!pthread_join(a, NULL)); CHECK(!pthread_join(b, NULL));
    printf("pioneer selector: %s (%d failures)\n", failures ? "FAIL" : "PASS", failures);
    return failures ? 1 : 0;
}
