/* Actual master callback, native AZ04 and armhf, including mono output. */
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>
#include <math.h>
static uint32_t copy_input(void *h, void *c, int ic, const float *const *in,
                           int oc, float *const *out, int frames) {
    (void)h; (void)c; assert(ic == oc);
    for (int ch = 0; ch < oc; ++ch) memcpy(out[ch], in[ch], (size_t)frames * sizeof(float));
    return 7;
}
static void test_clicks(void) {
    for (int mode = 1; mode <= 4; ++mode) {
        if (!sc_ms_effect(mode)) continue;
        memset(master_pfx, 0, sizeof master_pfx);
        memset(master_ms, 0, sizeof master_ms); sc_ms_prepare(&master_ms[0]);
        pioneer_fx_mode = mode;
        float previous = 0, maximum = 0;
        for (unsigned n = 0; n < 6 * 96000; ++n) {
            pfx_time_ms = n < 96000 ? 0 : (n < 3 * 96000 ? 10 : (n < 5 * 96000 ? 4000 : 0));
            float dry = 0.2f * sinf((float)(n % 96000) * (2.0f * 3.14159265358979323846f * 997.0f / 96000.0f));
            float l, r; const float *in[] = {&dry, &dry}; float *out[] = {&l, &r};
            hook_master_output(NULL, NULL, 2, in, 2, out, 1);
            float step = fabsf(l - previous); if (step > maximum) maximum = step;
            assert(isfinite(l) && isfinite(r)); previous = l;
        }
        assert(maximum < 0.08f);
        printf("master mode=%d switching max-step=%.6f (<0.08) PASS\n", mode, maximum);
    }
}
static void test_style_drains(void) {
    const unsigned phases[] = {0, 233, 987, 1597, 4181};
    const int targets[] = {0, 3, 5};
    for (int mode = 1; mode <= 4; ++mode) {
        if (!sc_ms_effect(mode)) continue;
        for (unsigned t = 0; t < 3; ++t) {
            float maximum = 0;
            for (unsigned phase = 0; phase < sizeof phases / sizeof *phases; ++phase) {
                memset(master_pfx, 0, sizeof master_pfx);
                memset(master_ms, 0, sizeof master_ms); sc_ms_prepare(&master_ms[0]);
                pfx_time_ms = 500;
                unsigned change = 2 * 96000 + phases[phase];
                float previous = 0;
                for (unsigned n = 0; n < change + 400; ++n) {
                    pioneer_fx_mode = n < change ? mode : targets[t];
                    float dry = 0.2f * sinf((float)(n % 96000) * (2.0f * 3.14159265358979323846f * 997.0f / 96000.0f));
                    float l, r; const float *in[] = {&dry, &dry}; float *out[] = {&l, &r};
                    hook_master_output(NULL, NULL, 2, in, 2, out, 1);
                    if (n >= change && fabsf(l - previous) > maximum) maximum = fabsf(l - previous);
                    previous = l;
                }
            }
            printf("master drain %d->%d max-step=%.6f (<0.08)\n", mode, targets[t], maximum);
            fflush(stdout);
            assert(maximum < 0.08f);
        }
    }
}
int main(void) {
    dsp_originals[DSP_MASTER] = (void *)copy_input;
    pioneer_fx_mode = 4;
    for (int channels = 1; channels <= 2; ++channels) {
        memset(master_pfx, 0, sizeof master_pfx);
        memset(master_ms, 0, sizeof master_ms);
        sc_ms_prepare(&master_ms[0]);
        pfx_time_ms = 4000;
        unsigned first = UINT32_MAX;
        for (unsigned n = 0; n <= 4000 * 96; ++n) {
            float dry = n == 0 ? 0.5f : 0, quiet = 0, l, r;
            const float *in[] = {&dry, &quiet}; float *out[] = {&l, &r};
            assert(hook_master_output(NULL, NULL, channels, in, channels, out, 1) == 7);
            /* The fading baseline has its first echo after the 20 ms fade.
             * It must contribute nothing before the requested 4 second delay. */
            if (n && fabsf(l) > 1e-5f && first == UINT32_MAX) first = n;
            if (channels == 2) assert(r == 0);
        }
        assert(first == 4000 * 96);
        printf("master ms channels=%d 4000 ms = %u frames PASS\n", channels, first);
        pfx_time_ms = 0;
        for (unsigned n = 0; n < SC_MS_FADE + 2; ++n) {
            float dry = 0, l, r; const float *in[] = {&dry, &dry}; float *out[] = {&l, &r};
            hook_master_output(NULL, NULL, channels, in, channels, out, 1);
        }
        assert(master_ms[0].blend == 0);
    }
    test_clicks();
    test_style_drains();
    puts("test_ms_master_hook PASS");
    return 0;
}
