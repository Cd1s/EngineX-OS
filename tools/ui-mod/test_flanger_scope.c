/* Native armhf on Batam: preserve the old opaque third argument and makeup
 * for every build/board outside the verified PX scope. */
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>

static float *const *expected_output;
static unsigned calls;
static uint32_t master_copy(void *h, void *c, int ic, const float *const *in,
                            int oc, float *const *out, int frames) {
    (void)h; (void)c; assert(ic == oc);
    for (int ch = 0; ch < oc; ++ch) memcpy(out[ch], in[ch], (size_t)frames * sizeof(float));
    return 7;
}
static void test_px_master_bypass(void) {
    dsp_originals[DSP_MASTER] = (void *)master_copy;
    fx_selector_master_bypass = 1;
    pioneer_fx_mode = SC_PFX_SPACE; /* an old settings.conf value at startup */
    for (unsigned n = 0; n < 1000; ++n) {
        float a[128], b[128], l[128], r[128];
        for (unsigned i = 0; i < 128; ++i) a[i] = b[i] = 0.2f;
        const float *in[] = {a, b}; float *out[] = {l, r};
        assert(hook_master_output(NULL, NULL, 2, in, 2, out, 128) == 7);
        assert(!memcmp(a, l, sizeof a) && !memcmp(b, r, sizeof b));
    }
    puts("test_px_master_bypass PASS");
}
static void stock(void *self, float *const *in, float *const *out, uint32_t frames) {
    assert(self && out == expected_output);
    ++calls;
    for (unsigned i = 0; i < frames; ++i) { in[0][i] *= 0.25f; in[1][i] *= 0.25f; }
}

static void transition_original(void *handler, const void *event) { (void)handler; (void)event; }
static void test_px_off_on_reset(void) {
    int owner, effect;
    unsigned char handler[24] = {0}, event[64] = {0};
    uint32_t address = (uint32_t)(uintptr_t)&owner;
    memcpy(handler + 12, &address, 4);
    memset(sc_pfx_routes, 0, sizeof sc_pfx_routes);
    sc_pfx_select(0, 1); assert(sc_pfx_bind(0, &owner, &effect));
    fx_selector_supported = 1;
    dsp_originals[DSP_TURN_OFF] = dsp_originals[DSP_TURN_ON] = dsp_originals[DSP_ENABLE] = (void *)transition_original;
    hook_fx_turn_off(handler, event); hook_fx_turn_on(handler, event); hook_fx_enable(handler, event);
    assert(sc_pfx_routes[0].reset_epoch == 3);
    fx_selector_supported = 0;
    puts("test_px_off_on_reset PASS");
}

int main(void) {
    int self;
    dsp_originals[DSP_FLANGER] = (void *)stock;
    fx_selector_supported = 0;
    const uintptr_t opaque[] = {0, 1, 2, 4, 0x12345678};
    for (unsigned v = 0; v < sizeof opaque / sizeof *opaque; ++v) {
        float l[128], r[128], a[128], b[128];
        for (unsigned i = 0; i < 128; ++i) l[i] = r[i] = a[i] = b[i] = 0.2f;
        float *in[] = {l, r}, *reference[] = {a, b};
        expected_output = (float *const *)opaque[v];
        memset(sc_dsp_state(), 0, sizeof *sc_dsp_state());
        float dry = sc_dsp_block_energy(reference, 128);
        stock(&self, reference, expected_output, 128);
        sc_dsp_makeup_apply(&self, reference, 128, dry);
        memset(sc_dsp_state(), 0, sizeof *sc_dsp_state());
        hook_flanger(&self, in, expected_output, 128);
        assert(!memcmp(l, a, sizeof l) && !memcmp(r, b, sizeof r));
        assert(l[127] > 0.05f); /* makeup also runs for third arguments other than 2 */
    }
    assert(calls == 10);
    assert(!px_carrier(NULL) && !px_is_carrier(NULL));
    test_px_off_on_reset();
    test_px_master_bypass();
    puts("test_flanger_scope PASS; test_px_null_owner PASS");
    return 0;
}
