/* Native armhf on the build host: preserve the old opaque third argument and makeup
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
static void *rate_handler;
static const void *rate_event;
static unsigned rate_calls;
static void rate_original(void *handler, const void *event) {
    assert(handler == rate_handler && event == rate_event);
    ++rate_calls;
}
static void rate_destroy(void *owner) { assert(owner); }
static void test_px_rate_events(void) {
    int owner[2], effect[2];
    unsigned char handler[24] = {0}, event[40] = {0}, saved[40];
    const char *name = "Flanger";
    unsigned length = 7, index = 2;
    uint32_t address = (uint32_t)(uintptr_t)&owner[0];
    memcpy(handler + 12, &address, 4);
    memcpy(event + 8, &name, 4); memcpy(event + 12, &length, 4);
    memcpy(event + 36, &index, 4);
    memset(px_rates, 0, sizeof px_rates);
    memset(sc_pfx_routes, 0, sizeof sc_pfx_routes);
    px_rate_original = (void *)rate_original;
    rate_handler = handler; rate_event = event; rate_calls = 0;
    fx_selector_supported = 1;
    hook_px_rate(handler, event); /* Arrives before the binding handshake. */
    assert(px_rate_value(&owner[0]) == 0.2f);
    for (unsigned u = 0; u < 2; ++u) {
        sc_pfx_select(u, u + 1); assert(sc_pfx_bind(u, &owner[u], &effect[u]));
        sc_pfx_control(&owner[u], 0, 0.7f); sc_pfx_control(&owner[u], 1, 0.4f);
        sc_pfx_control(&owner[u], 2, 0.6f);
    }
    for (index = 0; index <= 10; ++index) {
        memcpy(event + 36, &index, 4); memcpy(saved, event, sizeof saved);
        hook_px_rate(handler, event);
        assert(!memcmp(event, saved, sizeof event));
        assert(sc_pfx_float(sc_pfx_routes[0].time) == (float)index / 10.0f);
        assert(sc_pfx_float(sc_pfx_routes[1].time) == 0.6f);
        assert(sc_pfx_float(sc_pfx_routes[0].parameter) == 0.4f);
        assert(sc_pfx_float(sc_pfx_routes[0].mix) == 0.7f);
    }
    index = UINT32_MAX; memcpy(event + 36, &index, 4);
    hook_px_rate(handler, event);
    assert(sc_pfx_float(sc_pfx_routes[0].time) == 1.0f);
    name = "Reverb"; length = 6; index = 0;
    memcpy(event + 8, &name, 4); memcpy(event + 12, &length, 4);
    memcpy(event + 36, &index, 4);
    hook_px_rate(handler, event); /* Stock names are forwarded without a PX write. */
    assert(sc_pfx_float(sc_pfx_routes[0].time) == 1.0f);
    fx_selector_supported = 0;
    name = "Flanger"; length = 7;
    memcpy(event + 8, &name, 4); memcpy(event + 12, &length, 4);
    hook_px_rate(handler, event);
    assert(sc_pfx_float(sc_pfx_routes[0].time) == 1.0f && rate_calls == 15);
    px_destructor_original = (void *)rate_destroy;
    hook_px_destroy(&owner[0]);
    assert(px_rate_value(&owner[0]) == 0.5f && !sc_pfx_routes[0].bound);
    puts("test_px_rate_events PASS (11 indices, prebind, isolation, passthrough, destruction)");
}
static double render_px_rate(unsigned index) {
    int owner, effect;
    unsigned char handler[24] = {0}, event[40] = {0};
    const char *name = "Flanger";
    unsigned length = 7;
    uint32_t address = (uint32_t)(uintptr_t)&owner;
    memcpy(handler + 12, &address, 4);
    memcpy(event + 8, &name, 4); memcpy(event + 12, &length, 4);
    memcpy(event + 36, &index, 4);
    memset(px_rates, 0, sizeof px_rates);
    memset(sc_pfx_routes, 0, sizeof sc_pfx_routes);
    sc_pfx_select(0, SC_PFX_SPIRAL); assert(sc_pfx_bind(0, &owner, &effect));
    sc_pfx_control(&owner, 0, 0.7f); sc_pfx_control(&owner, 1, 0.4f);
    rate_handler = handler; rate_event = event;
    fx_selector_supported = 1;
    hook_px_rate(handler, event);
    struct sc_pfx_route *r = &sc_pfx_routes[0];
    sc_pfx_activate(r);
    double energy = 0;
    for (unsigned block = 0; block < 750; ++block) {
        float l[128], right[128]; float *in[] = {l, right};
        for (unsigned i = 0; i < 128; ++i)
            l[i] = right[i] = (float)((int)((block * 128 + i) % 1009) - 504) / 4096.0f;
        r->input_a = in;
        sc_pfx_instance_prepare(&r->dsp);
        if (pfx_time_ms) sc_ms_prepare(&r->dsp.ms);
        hook_flanger(&effect, in, (float *const *)2, 128);
        for (unsigned i = 0; i < 128; ++i) energy += (double)l[i] * l[i];
    }
    fx_selector_supported = 0;
    return energy;
}
static void test_px_ms_render(void) {
    pfx_time_ms = 10;
    double a = render_px_rate(1), b = render_px_rate(9);
    pfx_time_ms = 4000;
    double longer = render_px_rate(1), again = render_px_rate(9);
    printf("ms energies %.17g %.17g %.17g %.17g\n", a, b, longer, again);
    fflush(stdout);
    assert(a == b && longer == again && a != longer);
    pfx_time_ms = 0;
    puts("test_px_ms_render PASS (fixed ms independent of native Rate; 10/4000 ms)");
}
static void test_px_rate_render(void) {
    /* Fresh state and identical Parameter/input: a warmed delay must not make
     * an ignored Rate event appear to modulate the production process hook. */
    double a = render_px_rate(1), b = render_px_rate(9), again = render_px_rate(1);
    double difference = a - b;
    assert(a == again && difference * difference > 0.000001);
    puts("test_px_rate_render PASS (fresh-state production event-to-audio path)");
}
static void test_px_off_on_reset(void) {
    unsigned char owner[0x40] = {0}; /* live-owner fixture, empty collection */
    int effect;
    unsigned char handler[24] = {0}, event[64] = {0};
    uint32_t address = (uint32_t)(uintptr_t)&owner;
    memcpy(handler + 12, &address, 4);
    memset(sc_pfx_routes, 0, sizeof sc_pfx_routes);
    sc_pfx_select(0, 1); assert(sc_pfx_bind(0, &owner, &effect));
    fx_selector_supported = 1;
    dsp_originals[DSP_TURN_OFF] = dsp_originals[DSP_TURN_ON] = dsp_originals[DSP_ENABLE] = (void *)transition_original;
    hook_fx_turn_off(handler, event); hook_fx_turn_on(handler, event); hook_fx_enable(handler, event);
    assert(sc_pfx_routes[0].reset_epoch == 2); /* ENABLE must preserve delay tails */
    fx_selector_supported = 0;
    puts("test_px_off_on_reset PASS");
}

static void test_double_reset_dry(void) {
    int owner, effect;
    float l[128], right[128], dry_l[128], dry_r[128];
    float *in[] = {l, right};
    memset(sc_pfx_routes, 0, sizeof sc_pfx_routes);
    struct sc_pfx_route *r = &sc_pfx_routes[0];
    sc_pfx_select(0, 1); assert(sc_pfx_bind(0, &owner, &effect));
    r->input_a = in; r->mix = sc_pfx_bits(0.8f); r->parameter = sc_pfx_bits(0.5f);
    sc_pfx_activate(r); sc_pfx_instance_prepare(&r->dsp);
    fx_selector_supported = 1;
    unsigned before = calls;
    /* Initial render, then two resets in consecutive 128-frame blocks. No
     * prepare between them: both banks become DIRTY on the second reset. */
    for (unsigned block = 0; block < 4; ++block) {
        for (unsigned i = 0; i < 128; ++i) {
            l[i] = dry_l[i] = (float)((int)i - 64) / 512.0f;
            right[i] = dry_r[i] = (float)((int)i - 32) / 512.0f;
        }
        if (block == 1 || block == 2) sc_pfx_reset_owner(&owner);
        hook_flanger(&effect, in, (float *const *)2, 128);
        if (block >= 2) {
            assert(!memcmp(l, dry_l, sizeof l) && !memcmp(right, dry_r, sizeof right));
            assert(r->native_override && r->dsp.wet == 0);
            assert(r->dsp.bank_state[0] == SC_PFX_DIRTY && r->dsp.bank_state[1] == SC_PFX_DIRTY);
        }
    }
    assert(calls == before); /* never fall through to full-wet stock Flanger */
    sc_pfx_instance_prepare(&r->dsp);
    hook_flanger(&effect, in, (float *const *)2, 128);
    assert(r->dsp.current && calls == before);
    fx_selector_supported = 0;
    puts("test_double_reset_dry PASS");
}

static void *ready_writer(void *path) {
    for (unsigned i = 0; i < 200; ++i) assert(px_write_ready(path));
    return NULL;
}

static void test_px_product_scope(void) {
    assert(px_selector_capable("5.3.0", "JC11"));
    assert(!px_selector_capable("5.3.0", "JP07"));
    assert(!px_selector_capable("5.3.0", "JP08"));
    assert(!px_selector_capable("5.0.4", "JC11"));
    assert(!px_selector_capable("5.3.0-sclive4", "JC11"));
    char dir[] = "/tmp/px-ready-XXXXXX", path[128];
    assert(mkdtemp(dir));
    snprintf(path, sizeof path, "%s/ready", dir);
    assert(px_write_ready(path));
    FILE *f = fopen(path, "r"); char text[64] = {0};
    assert(f && fgets(text, sizeof text, f) && !fclose(f));
    assert(!strcmp(text, "armhf-px-v2 JC11\n"));
    pthread_t writer;
    assert(!pthread_create(&writer, NULL, ready_writer, path));
    for (unsigned i = 0; i < 500; ++i) {
        f = fopen(path, "r");
        assert(f && fgets(text, sizeof text, f) && !fclose(f));
        assert(!strcmp(text, "armhf-px-v2 JC11\n"));
    }
    assert(!pthread_join(writer, NULL));
    assert(!unlink(path) && !rmdir(dir));
    assert(!px_write_ready(path)); /* unavailable /run: do not install/bypass */
    puts("test_px_product_scope PASS; test_px_ready_preflight PASS; test_atomic_ready PASS");
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
    test_double_reset_dry();
    test_px_product_scope();
    test_px_off_on_reset();
    test_px_rate_events();
    test_px_rate_render();
    test_px_ms_render();
    test_px_master_bypass();
    puts("test_flanger_scope PASS; test_px_null_owner PASS");
    return 0;
}
