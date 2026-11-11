/* Compile against the integration header and the new header; compare stdout.
 * Include ordering matches the hook and tests >1 ring wrap with changing Rate. */
#include <stdio.h>
#ifndef PX_HEADER
#define PX_HEADER "pioneer_fx.h"
#endif
#include PX_HEADER
static struct sc_pfx_instance fx;
int main(void) {
    for (int mode = 1; mode <= 5; ++mode) {
        memset(&fx, 0, sizeof fx);
        uint32_t random = 1234567;
        uint64_t hash = UINT64_C(14695981039346656037);
        for (unsigned n = 0; n < 8 * 96000; n += 128) {
            float l[128], r[128];
            for (unsigned i = 0; i < 128; ++i) {
                random = random * 1664525u + 1013904223u;
                l[i] = (float)((int)(random >> 20) - 2048) / 8192.0f;
                r[i] = (float)((int)((n + i) % 4096) - 2048) / 8192.0f;
            }
            sc_pfx_instance_prepare(&fx);
            sc_pfx_process(&fx, l, r, 128, mode, (float)((n / 48000) % 11) / 10.0f, 0.4f, 0.7f);
            for (unsigned i = 0; i < 128; ++i) {
                uint32_t a, b; memcpy(&a, &l[i], 4); memcpy(&b, &r[i], 4);
                hash ^= a; hash *= UINT64_C(1099511628211); hash ^= b; hash *= UINT64_C(1099511628211);
            }
        }
#ifndef PX_HEADER_BASELINE
#if defined(__arm__) || defined(GOLDEN_FP_OFF)
        static const uint64_t expected[] = {UINT64_C(0xe28d9998bbe0bddf),UINT64_C(0x1fac56ca9e685e25),UINT64_C(0x961440d4a910c858),UINT64_C(0xee9078a600ffb0d8),UINT64_C(0x1258a4e4e495f7ff)};
#else
        static const uint64_t expected[] = {UINT64_C(0xc95f7fdf087972c7),UINT64_C(0x15950e3e846bebaf),UINT64_C(0x65a48b655db1f960),UINT64_C(0x00535276b5f4aa02),UINT64_C(0x3033b6f2c11822b1)};
#endif
        if (hash != expected[mode - 1]) return 1;
#endif
        printf("PX OFF mode=%d hash=%016llx\n", mode, (unsigned long long)hash);
    }
    return 0;
}
