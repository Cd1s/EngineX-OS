/* Color FX on each channel's filter knob (5.3.0 all-in-ones): the hook's real setValue and
 * ChannelSweepFx::process wrappers around a stand-in for stock. armhf only (A32 hook code):
 * arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror tools/ui-mod/test_color_fx_hook.c -ldl -lpthread -lm -o /tmp/test_color_fx_hook && /tmp/test_color_fx_hook
 * The real Engine path (knob -> setValue -> node) is checked in the FX sandbox (test_colorfx_native.c). */
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>
#include <math.h>

enum { FRAMES = 128 };
static unsigned stock_calls;
static float stored_knob;
static void stock_value(void *control, float v) { (void)control; stored_knob = v; }
/* Stand-in for stock: a fixed gain, which the style path never produces. */
static void stock_process(void *node, int ins, float **in, int outs, float **out, int frames, void *a, void *b) {
    (void)node; (void)ins; (void)outs; (void)a; (void)b;
    ++stock_calls;
    for (int ch = 0; ch < 2; ++ch)
        for (int i = 0; i < frames; ++i) out[ch][i] = in[ch][i] * 0.25f;
}
struct control { char head[0x38]; void *node, *spare; };
static struct control ctl[2];
static char nodes[4];
static unsigned lcg = 1;
static float noise(void) { lcg = lcg * 1664525u + 1013904223u; return 0.25f * (float)((int)(lcg >> 8) % 65536 - 32768) / 65536.0f; }
static void knob(int k, float v) { hook_cfx_value(&ctl[k], v); assert(stored_knob == v); } /* stock always gets the real value */
static void prepare(void) { for (unsigned i = 0; i < CFX_CHANNELS; ++i) sc_pfx_instance_prepare(&cfx_channels[i].dsp); }

struct result { int stock; int changed; double off_dry; };
/* `blocks` of noise through a node. stock: every sample equals stock's and stock ran each block.
 * changed: the input or the sentinel-filled output was written. off_dry: rms distance from dry. */
static struct result run(void *node, unsigned blocks) {
    struct result r = {1, 0, 0};
    double sum = 0;
    for (unsigned b = 0; b < blocks; ++b) {
        float il[FRAMES], ir[FRAMES], ol[FRAMES], orr[FRAMES], kl[FRAMES], kr[FRAMES];
        float *in[2] = {il, ir}, *out[2] = {ol, orr};
        for (unsigned i = 0; i < FRAMES; ++i) { il[i] = kl[i] = noise(); ir[i] = kr[i] = noise(); ol[i] = orr[i] = 7.0f; }
        unsigned calls = stock_calls;
        hook_cfx_process(node, 2, in, 2, out, FRAMES, NULL, NULL);
        if (stock_calls != calls + 1) r.stock = 0;
        for (unsigned i = 0; i < FRAMES; ++i) {
            float sl = kl[i] * 0.25f, sr = kr[i] * 0.25f;
            assert(isfinite(ol[i]) && isfinite(orr[i]));
            if (memcmp(&ol[i], &sl, 4) || memcmp(&orr[i], &sr, 4)) r.stock = 0;
            if (il[i] != kl[i] || ir[i] != kr[i] || ol[i] != 7.0f || orr[i] != 7.0f) r.changed = 1;
            sum += (double)(ol[i] - kl[i]) * (ol[i] - kl[i]);
        }
    }
    r.off_dry = sqrt(sum / (blocks * FRAMES));
    return r;
}
/* Back to stock: the style rings down over stock's output, then the bank is handed back. */
static void settles(int k) {
    struct result r = {0, 0, 0};
    for (unsigned b = 0; b < 40 && !(r.stock && !cfx_channels[k].dsp.current); ++b) r = run(ctl[k].node, 1);
    assert(r.stock && !cfx_channels[k].dsp.current);
    assert(run(ctl[k].node, 20).stock);
}

int main(void) {
    cfx_value_original = (void *)stock_value;
    cfx_process_original = (void *)stock_process;
    for (int k = 0; k < 2; ++k) ctl[k].node = &nodes[2 * k], ctl[k].spare = &nodes[2 * k + 1];
    prepare();

    assert(cfx_amount(0.5f) == 0 && cfx_amount(0.495f) == 0 && cfx_amount(0.505f) == 0 && cfx_amount(NAN) == 0);
    assert(cfx_amount(0.0f) == 1 && cfx_amount(1.0f) == 1 && cfx_amount(0.6f) > 0 && cfx_amount(0.4f) > 0);
    assert(fabsf(cfx_amount(0.25f) - cfx_amount(0.75f)) < 1e-6f); /* the same turn either way */

    // A channel whose knob never moved is stock, style or not.
    cfx_style = SC_PFX_SPIRAL;
    assert(run(ctl[0].node, 4).stock);

    // 1. Centre band with a style picked: stock, bit for bit.
    static const float centre[] = {0.5f, 0.495f, 0.505f};
    for (unsigned i = 0; i < 3; ++i) { knob(0, centre[i]); assert(run(ctl[0].node, 8).stock); }

    // 2. Either way off centre: stock is skipped and the style plays, more the further the knob goes.
    static const int styles[] = {SC_PFX_SPIRAL, SC_PFX_PITCH_ECHO, SC_PFX_SLIP_ROLL, SC_PFX_DUB_ECHO, SC_PFX_SPACE};
    for (unsigned s = 0; s < 5; ++s) {
        cfx_style = styles[s];
        static const float sides[] = {0.05f, 0.95f};
        for (unsigned side = 0; side < 2; ++side) {
            knob(0, side ? 0.6f : 0.4f);
            struct result small = run(ctl[0].node, 60);
            knob(0, sides[side]);
            struct result full = run(ctl[0].node, 60);
            assert(!small.stock && !full.stock && full.changed && full.off_dry > small.off_dry && full.off_dry > 1e-3);
            // The second node does nothing at centre; off centre it stays that way (stock skipped, buffers untouched).
            unsigned calls = stock_calls;
            struct result spare = run(ctl[0].spare, 4);
            assert(stock_calls == calls && !spare.changed);
            // 3. Channels are independent: channel 2 at centre is stock while channel 1 plays.
            knob(1, 0.5f);
            assert(run(ctl[1].node, 8).stock && !cfx_channels[1].dsp.current);
            knob(0, 0.5f);
            settles(0);
            prepare(); /* the poll worker clears the handed-back bank */
            printf("style %d %s: centre stock, off-dry %.4f -> %.4f\n", styles[s], side ? "right" : "left", small.off_dry, full.off_dry);
        }
    }

    // 4. Style Off is stock at every knob position, including a knob left turned while it plays.
    knob(0, 0.1f);
    assert(!run(ctl[0].node, 30).stock);
    cfx_style = 0;
    settles(0);
    static const float anywhere[] = {0.0f, 0.3f, 0.5f, 0.7f, 1.0f};
    for (unsigned i = 0; i < 5; ++i) { knob(0, anywhere[i]); assert(run(ctl[0].node, 8).stock); assert(run(ctl[0].spare, 2).stock); }
    prepare();

    // 5. Not stereo: stock, untouched.
    cfx_style = SC_PFX_SPACE;
    knob(0, 0.9f);
    float l[FRAMES] = {0}, *mono[1] = {l};
    unsigned calls = stock_calls;
    float r[FRAMES] = {0}, *pair[2] = {l, r};
    hook_cfx_process(ctl[0].node, 1, mono, 2, pair, FRAMES, NULL, NULL);
    assert(stock_calls == calls + 1);

    // 6. settings.conf: pioneer_fx= drives the channel knobs only, never the master output; players keep it Off.
    char dir[] = "/tmp/cfx-hook-XXXXXX";
    assert(mkdtemp(dir));
    char board[PATH_MAX], conf[PATH_MAX];
    snprintf(board, sizeof board, "%s/product-code", dir);
    snprintf(conf, sizeof conf, "%s/settings.conf", dir);
    product_code_path = board;
    four_deck_settings = conf;
    FILE *f = fopen(board, "w"); fputs("JC16\n", f); fclose(f);
    f = fopen(conf, "w"); fputs("pioneer_fx=dub_echo\n", f); fclose(f);
    cfx_style = 0;
    lim_start_poll();
    usleep(250000);
    assert(__atomic_load_n(&cfx_style, __ATOMIC_RELAXED) == SC_PFX_DUB_ECHO);
    assert(__atomic_load_n(&pioneer_fx_mode, __ATOMIC_RELAXED) == 0);
    f = fopen(board, "w"); fputs("JP07\n", f); fclose(f);
    f = fopen(conf, "w"); fputs("pioneer_fx=space\nlang=en\n", f); fclose(f);
    usleep(250000);
    assert(__atomic_load_n(&cfx_style, __ATOMIC_RELAXED) == 0);
    unlink(board); unlink(conf); rmdir(dir);

    puts("test_color_fx_hook PASS");
    return 0;
}
