/* Run plain, ASan/UBSan and TSan, natively on the build host. */
#include "pioneer_ms.h"
#include <assert.h>
#include <math.h>
#include <pthread.h>
#include <stdio.h>

static struct sc_pfx_ms state;
static unsigned requested_ms;
static void fresh(void) { memset(&state, 0, sizeof state); sc_ms_prepare(&state); }
static void config_line(struct sc_ms_config *c, const char *text) {
    char line[256]; snprintf(line, sizeof line, "%s", text); sc_ms_setting(c, line);
}
static void test_config(void) {
    static const char *bad[] = {"ms", "NaN", "inf", "1.5", "125ms", "-1", "+10", "", "4294967296", "9999999999999999999999999"};
    for (unsigned i = 0; i < sizeof bad / sizeof *bad; ++i) {
        unsigned n = 123; assert(!sc_ms_integer(bad[i], &n) && n == 123);
    }
    struct sc_ms_config c = {500, 0};
    config_line(&c, "fx_time_mode=ms\n"); config_line(&c, "fx_time_ms=4000\n");
    assert(c.enabled && c.ms == 4000);
    /* Rebuild on every file replacement, including a removed key or empty file. */
    c = (struct sc_ms_config){500, 0};
    config_line(&c, "fx_time_ms=10"); assert(!c.enabled && c.ms == 10);
    config_line(&c, "fx_time_ms=4294967295"); assert(c.ms == 4000);
    config_line(&c, "fx_time_ms=0"); assert(c.ms == 10);
    config_line(&c, "fx_time_ms=ms"); assert(c.ms == 500);
    config_line(&c, "fx_time_mode=msjunk"); assert(!c.enabled);
    puts("config strict integers, overflow, reset, clamp PASS");
}
static void test_impulses(void) {
    const unsigned values[] = {10, 500, 4000};
    for (unsigned v = 0; v < 3; ++v) {
        fresh(); unsigned delay = values[v] * 96, first = UINT32_MAX;
        for (unsigned i = 0; i <= delay + 100; ++i) {
            float l = 0, r = 0;
            sc_ms_mix(&state, i == 0 ? 0.5f : 0, 0, &l, &r, 4, values[v], 0.5f, 0);
            if (fabsf(l) > 1e-5f && first == UINT32_MAX) first = i;
            assert(r == 0 && isfinite(l));
        }
        assert(first == delay);
        printf("Dub Echo impulse %u ms = %u frames PASS\n", values[v], first);
    }
}
static void test_off_and_neighbours(void) {
    fresh(); static struct sc_pfx_ms before; memcpy(&before, &state, sizeof state);
    for (unsigned mode = 0; mode <= 5; ++mode) {
        float l = 0.125f, r = -0.25f;
        sc_ms_mix(&state, 0.1f, -0.2f, &l, &r, (int)mode, 0, 0.5f, 0);
        assert(l == 0.125f && r == -0.25f && !memcmp(&before, &state, sizeof state));
        if (mode == 0 || mode == 3 || mode == 5) {
            sc_ms_mix(&state, 0.1f, -0.2f, &l, &r, (int)mode, 4000, 0.5f, 0);
            assert(l == 0.125f && r == -0.25f && !memcmp(&before, &state, sizeof state));
        }
    }
    puts("OFF byte identity; Slip Roll/Space unchanged PASS");
}
static void test_clicks_and_golden(void) {
    for (int mode = 1; mode <= 4; ++mode) {
        if (!sc_ms_effect(mode)) continue;
        fresh(); float previous = 0, maximum = 0;
        uint64_t hash = UINT64_C(14695981039346656037);
        for (unsigned i = 0; i < 8 * 96000; ++i) {
            float dry = 0.2f * sinf((float)(i % 96000) * (2.0f * 3.14159265358979323846f * 997.0f / 96000.0f));
            float l = dry, r = -dry;
            unsigned ms = i < 96000 ? 0 : (i < 3 * 96000 ? 10 : (i < 5 * 96000 ? 4000 : (i < 7 * 96000 ? 500 : 0)));
            sc_ms_mix(&state, dry, -dry, &l, &r, mode, ms, 0.5f, 1);
            float step = fabsf(l - previous); if (step > maximum) maximum = step;
            assert(isfinite(l) && isfinite(r) && fabsf(l) <= 4 && fabsf(r) <= 4);
            previous = l;
            uint32_t bits; memcpy(&bits, &l, 4); hash ^= bits; hash *= UINT64_C(1099511628211);
        }
        assert(maximum < 0.08f && state.blend == 0.0f);
#if defined(__aarch64__) && !defined(PRINT_GOLDEN)
        /* Native -O1 baseline; repeated under both sanitizer builds. */
        const uint64_t expected[] = {UINT64_C(0xec2b2409104335dd), UINT64_C(0x0596e7ce26519651), 0, UINT64_C(0x2a1b7a897011823f)};
        assert(hash == expected[mode - 1]);
#endif
        printf("mode=%d OFF/10/4000/500/OFF max-step=%.6f (<0.08) golden=%016llx PASS\n", mode, maximum, (unsigned long long)hash);
    }
}
static void *worker(void *arg) {
    (void)arg; sc_ms_prepare(&state);
    for (unsigned i = 0; i < 10000; ++i) __atomic_store_n(&requested_ms, i % 2 ? 10 : 4000, __ATOMIC_RELEASE);
    return NULL;
}
static void test_thread_handoff(void) {
    memset(&state, 0, sizeof state);
    pthread_t thread; assert(!pthread_create(&thread, NULL, worker, NULL));
    for (unsigned i = 0; i < 100000; ++i) {
        float l = 0.1f, r = -0.1f;
        sc_ms_mix(&state, l, r, &l, &r, 4, __atomic_load_n(&requested_ms, __ATOMIC_ACQUIRE), 0.5f, 1);
    }
    assert(!pthread_join(thread, NULL));
    assert(__atomic_load_n(&state.ready, __ATOMIC_ACQUIRE) == 2);
    puts("worker/audio handoff PASS");
}
static void test_nonfinite(void) {
    fresh();
    const float bad[] = {NAN, INFINITY, -INFINITY, 3e38f, -3e38f};
    for (unsigned i = 0; i < 20000; ++i) {
        float l = bad[i % 5], r = -l;
        sc_ms_mix(&state, l, r, &l, &r, 4, UINT32_MAX, 0.5f, 1);
        assert(isfinite(l) && isfinite(r) && fabsf(l) <= 4 && fabsf(r) <= 4);
    }
    puts("nonfinite input and maximum integer clamp PASS");
}
int main(void) {
    test_config(); test_impulses(); test_off_and_neighbours(); test_clicks_and_golden(); test_thread_handoff(); test_nonfinite();
    puts("test_fx_ms PASS"); return 0;
}
