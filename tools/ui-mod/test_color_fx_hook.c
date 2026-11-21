/* Color FX on each channel's filter knob (5.3.0 all-in-ones): the hook's real setValue and
 * ChannelSweepFx::process wrappers around a stand-in for stock. armhf only (A32 hook code):
 * arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror tools/ui-mod/test_color_fx_hook.c -ldl -lpthread -lm -o /tmp/test_color_fx_hook && /tmp/test_color_fx_hook
 * The real Engine path (knob -> setValue -> node) is checked in the FX sandbox (test_colorfx_native.c). */
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>
#include <math.h>
#include <sys/mman.h>
#include <sys/stat.h>

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

/* install_cfx_scope on a fake Engine image (anonymous, sparse): the known prologues are written at the
 * known offsets, then the vtable slot is made to hold something else. The install must fail closed:
 * log one line, patch nothing, write no ready marker. A wrong prologue is a different branch (no slot line). */
static char *capture_stderr(int (*fn)(uintptr_t, size_t, const char *), uintptr_t base, const char *version, int *rc) {
    static char out[512];
    fflush(stderr);
    int saved = dup(2);
    FILE *t = tmpfile();
    assert(saved >= 0 && t);
    assert(dup2(fileno(t), 2) == 2);
    *rc = fn(base, 4096, version);
    fflush(stderr);
    assert(dup2(saved, 2) == 2);
    close(saved);
    rewind(t);
    size_t n = fread(out, 1, sizeof out - 1, t);
    out[n] = 0;
    fclose(t);
    return out;
}
static void test_install_scope_mismatch(void) {
    static const struct { const char *version; uintptr_t value, slot, process; } sites[] = {
        {"5.3.0", 0xd8ba3c, 0x2b1e488, 0xec127c},
        {"5.3.0-sclive4", 0xd9619c, 0x2b9e0b0, 0xecdbd4},
    };
    static const uint32_t value_prefix[] = {0xe52d400c, 0xe98d4800};
    static const uint32_t process_prefix[] = {0xe16d42f4, 0xe1cd60f8};
    const char *saved_board = product_code_path;
    product_code_path = "/nonexistent-product-code"; /* an all-in-one: not a player */
    struct stat before;
    int had_marker = !stat(CFX_READY, &before);
    for (unsigned i = 0; i < sizeof sites / sizeof *sites; ++i) {
        size_t len = 0x2c00000;
        char *img = mmap(NULL, len, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS | MAP_NORESERVE, -1, 0);
        assert(img != MAP_FAILED);
        memcpy(img + sites[i].value, value_prefix, 8);
        memcpy(img + sites[i].process, process_prefix, 8);
        uintptr_t other = (uintptr_t)img + sites[i].value + 4; /* a slot that does not hold `value` */
        memcpy(img + sites[i].slot, &other, sizeof other);
        char value_bytes[8], process_bytes[8];
        memcpy(value_bytes, img + sites[i].value, 8);
        memcpy(process_bytes, img + sites[i].process, 8);
        int rc = -1;
        const char *log = capture_stderr(install_cfx_scope, (uintptr_t)img, sites[i].version, &rc);
        assert(rc == 0);
        assert(!strcmp(log, "engine-ui-hook: Color FX slot 4 mismatch, not installed\n"));
        assert(!memcmp(img + sites[i].value, value_bytes, 8) && !memcmp(img + sites[i].process, process_bytes, 8));
        uintptr_t held;
        memcpy(&held, img + sites[i].slot, sizeof held);
        assert(held == other);
        struct stat after;
        assert((stat(CFX_READY, &after) == 0) == (had_marker != 0)); /* no marker written by this call */
        if (had_marker) assert(before.st_mtime == after.st_mtime && before.st_ino == after.st_ino);

        /* The slot is right but the prologue is not: fail closed too, and say nothing about the slot. */
        memcpy(img + sites[i].slot, &(uintptr_t){(uintptr_t)img + sites[i].value}, sizeof(uintptr_t));
        memset(img + sites[i].value, 0, 8);
        log = capture_stderr(install_cfx_scope, (uintptr_t)img, sites[i].version, &rc);
        assert(rc == 0 && !strstr(log, "slot 4"));
        munmap(img, len);
    }
    int rc = -1;
    assert(!*capture_stderr(install_cfx_scope, 0, "5.0.4", &rc) && rc == 0); /* unknown build: silent, as before */
    product_code_path = saved_board;
}

int main(void) {
    test_install_scope_mismatch();
    cfx_value_original = (void *)stock_value;
    cfx_process_original = (void *)stock_process;
    for (int k = 0; k < 2; ++k) ctl[k].node = &nodes[2 * k], ctl[k].spare = &nodes[2 * k + 1];
    prepare();

    // First claim: hook_cfx_value claims a channel (CAS on node) and stores the knob after it. In that gap the
    // audio thread sees a claimed node whose knob was never stored: it must read centre (stock), not full wet.
    for (unsigned i = 0; i < CFX_CHANNELS; ++i) {
        unsigned bits = cfx_knobs[i];
        float v;
        memcpy(&v, &bits, sizeof v);
        assert(v == 0.5f && cfx_amount(v) == 0);
    }
    static char claimed_not_stored;
    cfx_style = SC_PFX_SPIRAL;
    __atomic_store_n(&cfx_channels[CFX_CHANNELS - 1].node, (void *)&claimed_not_stored, __ATOMIC_RELEASE);
    assert(run(&claimed_not_stored, 8).stock && !cfx_channels[CFX_CHANNELS - 1].dsp.current);
    __atomic_store_n(&cfx_channels[CFX_CHANNELS - 1].node, NULL, __ATOMIC_RELEASE);
    cfx_style = 0;

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
