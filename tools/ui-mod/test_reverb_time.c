/* #207: the code the hook runs inside Engine's Reverb decay formula computes exactly what
 * reverb_time.h says, and that is the stock value up to the knee and never 1.00 or more.
 * Native armhf on the build host (the code is A32):
 *   arm-linux-gnueabihf-gcc -O2 -mfpu=neon-vfpv4 -Wall -Wextra -Werror tools/ui-mod/test_reverb_time.c -o out/test_reverb_time
 * (VFPv4 for Engine's own vfma, which the deck's Cortex-A17 has). */
#define _GNU_SOURCE
#include <assert.h>
#include <math.h>
#include <stdio.h>
#include <sys/mman.h>
#include "reverb_time.h"

/* Engine's third instruction of the formula, then back to the caller of formula(). */
__attribute__((naked, target("arm"))) static void resume(void) {
    __asm__("vfma.f32 s16, s14, s15\n vmov.f32 s0, s16\n vpop {d8}\n bx lr\n");
}
/* The registers as they are at the site: the knob in s14, 0.5 in s15, r0 + 32 the on/off float. */
__attribute__((naked, target("arm"), noinline)) static float formula(float knob, void *code, const float *state) {
    (void)knob; (void)code; (void)state;
    __asm__("vpush {d8}\n vmov.f32 s14, s0\n vmov.f32 s15, #0.5\n mov ip, r0\n mov r0, r1\n bx ip\n");
}

int main(void) {
    uint32_t *code = mmap(NULL, 4096, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(code != MAP_FAILED);
    sc_reverb_time_code(code, (uint32_t)(uintptr_t)resume & ~1u);
    assert(!mprotect(code, 4096, PROT_READ | PROT_EXEC));
    __builtin___clear_cache((char *)code, (char *)code + 4096);
    float on[9] = {0}, off[9] = {0};
    on[8] = 1.0f;

    float last = 0;
    for (unsigned n = 2048; n <= 4096; ++n) { /* every 1/4096 from the centre to the top */
        float a = (float)n / 4096.0f, got = formula(a, code, on), stock = 0.65f + (a - 0.5f) * 0.5f;
        assert(got == sc_reverb_time(a, 1));
        if (a <= SC_REVERB_KNEE) assert(got == stock);
        else assert(got > stock);
        assert(got >= last && got < 1.0f);
        last = got;
        assert(formula(a, code, off) == stock && sc_reverb_time(a, 0) == stock); /* off: the stock tail */
    }
    assert(fabsf(formula(1.0f, code, on) - SC_REVERB_TIME_MAX) < 1e-6f);
    assert(formula(1.0f, code, on) > 0.98f); /* the stock top is 0.90 */
    /* Engine keeps the knob at or below 1; if it ever did not, the stock formula would pass 1.00. */
    const float beyond[] = {1.01f, 1.2f, 2.0f, 100.0f, INFINITY};
    for (unsigned i = 0; i < sizeof beyond / sizeof *beyond; ++i) {
        assert(formula(beyond[i], code, on) == formula(1.0f, code, on));
        assert(formula(beyond[i], code, off) < 1.0f);
    }
    puts("test_reverb_time PASS");
    return 0;
}
