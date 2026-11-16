/* #207: a longer top end for the stock Reverb's decay.
 *
 * Engine's "Reverb" is the Classic 1 type of its one reverb DSP. It turns the Parameter knob
 * A (0..1) into a decay value: for A >= 0.5, time = 0.65 + (A - 0.5) * 0.5, so the knob ends
 * at 0.90. The DSP has no limit of its own there. Measured on the real DSP in the PRIME 4
 * sandbox (60 dB decay after a noise burst, test_reverb_sandbox.sh): 0.90 is 4.9 s, 0.95 is
 * 9 s, 0.975 is 16 s, 0.99 is 28 s, 1.00 is about 55 s, and 1.05 grows until it is not a
 * number. So the knob may come close to 1.00 and must never reach it: there is no "infinite".
 *
 * Above SC_REVERB_KNEE the slope doubles; at and below it every position is stock. Only while
 * the effect is on: Engine lets the tail ring after FX OFF, and that tail stays the stock one
 * (4.9 s at most), so nothing long is left behind in a room. */
#ifndef SC_REVERB_TIME_H
#define SC_REVERB_TIME_H
#include <stdint.h>
#include <string.h>

#define SC_REVERB_KNEE 0.82f
#define SC_REVERB_TOP ((1.0f - SC_REVERB_KNEE) + 0.5f) /* 0.68: A - 0.5 plus the part above the knee, at A = 1 */
#define SC_REVERB_TIME_MAX 0.99f                       /* 0.65 + SC_REVERB_TOP / 2; keep it below 1.00 */

/* What Engine computes with the code below in place, for a knob at or above its centre.
 * Whatever arrives, the result is at most SC_REVERB_TIME_MAX (the stock formula has no limit). */
static inline float sc_reverb_time(float a, int on) {
    float d = a - 0.5f, e = on ? d - (SC_REVERB_KNEE - 0.5f) : 0.0f;
    if (e > 0) d += e;
    if (d > SC_REVERB_TOP) d = SC_REVERB_TOP;
    return 0.65f + d * 0.5f;
}

/* Runs in place of the formula's first two instructions (vsub s14, s14, s15 and the load of
 * 0.65 into s16) and resumes at the third, Engine's own vfma s16, s14, s15 (s15 is 0.5).
 * r0 is the DSP object + 0x5f0 there; +0x610 is 1.0f while the effect is on and 0.0f when off.
 * s14 is dead after that vfma and the flags are set again before their next use. */
enum { SC_REVERB_CODE_WORDS = 19, SC_REVERB_FUNCTION = 0x280 /* from the function's start to the site */ };
static inline void sc_reverb_time_code(uint32_t code[SC_REVERB_CODE_WORDS], uint32_t resume) {
    const float tail[3] = {SC_REVERB_KNEE - 0.5f, SC_REVERB_TOP, 0.65f};
    const uint32_t text[] = {
        0xee377a67, /* vsub.f32   s14, s14, s15   displaced: A - 0.5 */
        0xed908a08, /* vldr       s16, [r0, #32]  on: 1.0, off: 0.0 */
        0xeeb58ac0, /* vcmpe.f32  s16, #0.0 */
        0xeef1fa10, /* vmrs       APSR_nzcv, fpscr */
        0xcd9f8a0a, /* vldrgt     s16, [pc, #40]  knee - 0.5 */
        0xce378a48, /* vsubgt.f32 s16, s14, s16   on: A - knee; off: still 0.0 */
        0xeeb58ac0, /* vcmpe.f32  s16, #0.0 */
        0xeef1fa10, /* vmrs       APSR_nzcv, fpscr */
        0xce377a08, /* vaddgt.f32 s14, s14, s16   above the knee: twice the slope */
        0xed9f8a06, /* vldr       s16, [pc, #24]  the top */
        0xeeb47ac8, /* vcmpe.f32  s14, s16 */
        0xeef1fa10, /* vmrs       APSR_nzcv, fpscr */
        0xceb07a48, /* vmovgt.f32 s14, s16        never above it */
        0xed9f8a03, /* vldr       s16, [pc, #12]  displaced: 0.65 */
        0xe51ff004, /* ldr        pc, [pc, #-4] */
        resume,
    };
    memcpy(code, text, sizeof text);
    memcpy(&code[16], tail, sizeof tail);
}

/* The code above relies on r0 and on the on/off float. `site` is the formula in a loaded or
 * mapped Engine: true when the function around it sets them up as in the builds measured. */
static inline int sc_reverb_time_context(const void *site) {
    static const struct { int at; uint32_t word; } expected[] = {
        {-SC_REVERB_FUNCTION + 0x24, 0xe5d02644}, /* ldrb r2, [r0, #0x644]  the on/off byte */
        {-SC_REVERB_FUNCTION + 0x30, 0xe280ce61}, /* add  ip, r0, #0x610 */
        {-SC_REVERB_FUNCTION + 0x4c, 0x1eb06a47}, /* vmovne.f32 s12, s14    1.0 when on */
        {-SC_REVERB_FUNCTION + 0x54, 0xed8c6a00}, /* vstr s12, [ip] */
        {-0x18, 0xe2840e5f},                      /* add  r0, r4, #0x5f0 */
        {8, 0xeea78a27},                          /* vfma.f32 s16, s14, s15 */
    };
    for (unsigned i = 0; i < sizeof expected / sizeof *expected; ++i)
        if (memcmp((const char *)site + expected[i].at, &expected[i].word, 4)) return 0;
    return 1;
}
#endif
