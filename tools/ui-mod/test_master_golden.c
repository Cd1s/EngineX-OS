/* Native AArch64 AZ04 compatibility oracle. FNV-1a over exact float words,
 * 20 seconds per mode at 96 kHz. Expected values originate from dfdef97's
 * actual hook_master_output with this same deterministic callback/fixture.
 * gcc -O2 test_master_golden.c -ldl -lpthread -o test_master_golden
 * Repeat with -ffp-contract=off. No Engine/rootfs/device is needed. */
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#ifndef GOLDEN_HOOK_SOURCE
#define GOLDEN_HOOK_SOURCE "engine_ui_hook.c"
#endif
#include GOLDEN_HOOK_SOURCE
#include <assert.h>

static uint32_t copy_callback(void *host, void *context, int ic, const float *const *in,
                              int oc, float *const *out, int frames) {
    (void)host; (void)context;
    assert(ic == oc && oc == 2);
    for (int ch = 0; ch < oc; ++ch) memcpy(out[ch], in[ch], (size_t)frames * sizeof(float));
    return 0x1234;
}

int main(void) {
#ifdef GOLDEN_FP_OFF
    static const uint64_t expected[5] = {UINT64_C(0x2b14589a7cbaec75), UINT64_C(0x92b5494acccf3730),
        UINT64_C(0xb560e4dd037650cc), UINT64_C(0x2c70597c4940e052), UINT64_C(0xc96acf49bea7c17f)};
#else
    static const uint64_t expected[5] = {UINT64_C(0x6e96457533f4abba), UINT64_C(0x361ab84d3b77f724),
        UINT64_C(0xb560e4dd037650cc), UINT64_C(0x67a1e86321990ee5), UINT64_C(0x0340f2cb5a582c6b)};
#endif
    dsp_originals[DSP_MASTER] = (void *)copy_callback;
    for (int mode = 1; mode <= 5; ++mode) {
        memset(master_pfx, 0, sizeof master_pfx);
        pioneer_fx_mode = mode;
        uint32_t random = 1234567;
        uint64_t hash = UINT64_C(14695981039346656037);
        for (unsigned frame = 0; frame < 20 * 96000; frame += 128) {
            float l[128], r[128], ol[128], or_[128];
            for (unsigned i = 0; i < 128; ++i) {
                random = random * 1664525u + 1013904223u;
                l[i] = (float)((int)(random >> 20) - 2048) * (1.0f / 8192.0f);
                r[i] = (float)((int)((frame + i) % 4096) - 2048) * (1.0f / 8192.0f);
            }
            const float *in[] = {l, r}; float *out[] = {ol, or_};
            assert(hook_master_output(NULL, NULL, 2, in, 2, out, 128) == 0x1234);
            for (unsigned i = 0; i < 128; ++i) for (unsigned ch = 0; ch < 2; ++ch) {
                uint32_t bits; memcpy(&bits, &out[ch][i], 4);
                for (unsigned b = 0; b < 4; ++b) {
                    hash ^= (bits >> (b * 8)) & 255u; hash *= UINT64_C(1099511628211);
                }
            }
        }
        printf("AZ04 hook_master_output mode=%d hash=%016llx\n", mode, (unsigned long long)hash);
#ifndef PRINT_GOLDEN
        assert(hash == expected[mode - 1]);
#endif
    }
    (void)expected;
    puts("test_master_golden PASS");
    return 0;
}
