/* Actual master callback, native AZ04 and armhf, including mono output. */
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>
#include <math.h>
#include <sys/mount.h>
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
static void write_file(const char *path, const char *text) {
    char tmp[PATH_MAX];
    snprintf(tmp, sizeof tmp, "%s.tmp", path);
    FILE *f = fopen(tmp, "w");
    assert(f);
    assert(fputs(text, f) >= 0);
    assert(!fclose(f));
    assert(!rename(tmp, path));
}
static float run_echo_tail(void) {
    memset(master_echo, 0, sizeof master_echo);
    for (unsigned n = 0; n < 57600; ++n) {
        float loud = 0.25f, l, r;
        const float *in[] = {&loud, &loud}; float *out[] = {&l, &r};
        hook_master_output(NULL, NULL, 2, in, 2, out, 1);
    }
    float peak = 0;
    for (unsigned n = 0; n < 1024; ++n) {
        float silent = 0, l, r;
        const float *in[] = {&silent, &silent}; float *out[] = {&l, &r};
        hook_master_output(NULL, NULL, 2, in, 2, out, 1);
        if (fabsf(l) > peak) peak = fabsf(l);
    }
    return peak;
}
static void test_hidden_means_off(void) {
    char dir[] = "/tmp/fx-declutter-XXXXXX";
    assert(mkdtemp(dir));
    char board_path[PATH_MAX], conf_path[PATH_MAX];
    snprintf(board_path, sizeof board_path, "%s/product-code", dir);
    product_code_path = board_path;
    if (unshare(CLONE_NEWNS) == 0 && mount("none", "/", NULL, MS_REC | MS_PRIVATE, NULL) == 0) {
        mkdir("/data", 0755);
        assert(mount("tmpfs", "/data", "tmpfs", 0, NULL) == 0);
        mkdir("/data/sc5000-custom", 0755);
        mkdir(UI_ROOT, 0755);
        snprintf(conf_path, sizeof conf_path, "%s/settings.conf", UI_ROOT);
    } else {
        snprintf(conf_path, sizeof conf_path, "%s/settings.conf", dir);
        four_deck_settings = conf_path;
    }
    write_file(board_path, "JP07\n");
    write_file(conf_path, "upfader_echo=1\npioneer_fx=off\nfx_time_mode=ms\nfx_time_ms=250\n");
    lim_start_poll();
    usleep(180000);

    // 1. Standalone players keep upfader_echo=1 stored in settings.conf, but hidden Echo Out behaves as Off.
    static const char *const players[] = {"JP07\n", "JP08\n", "JP13\n", "JP14\n"};
    for (unsigned p = 0; p < sizeof players / sizeof *players; ++p) {
        write_file(board_path, players[p]);
        write_file(conf_path, "upfader_echo=1\npioneer_fx=off\nfx_time_mode=ms\nfx_time_ms=250\n");
        usleep(180000);
        FILE *f = fopen(conf_path, "r"); assert(f);
        char saved[256] = {0}; assert(fread(saved, 1, sizeof saved - 1, f) > 0); fclose(f);
        assert(strstr(saved, "upfader_echo=1") && strstr(saved, "fx_time_mode=ms") && strstr(saved, "fx_time_ms=250"));
        assert(run_echo_tail() == 0.0f);
    }

    // Same stored upfader_echo=1 on a mixer console (JC16) rings out as expected.
    write_file(board_path, "JC16\n");
    write_file(conf_path, "upfader_echo=1\npioneer_fx=off\nfx_time_mode=ms\nfx_time_ms=250\n");
    usleep(180000);
    assert(run_echo_tail() > 0.05f);

    // 2. Hidden PX Delay Time Mode / (ms) when Color FX Style is Off or Space behaves as Off while keeping 250 ms stored.
    write_file(conf_path, "pioneer_fx=off\nfx_time_mode=ms\nfx_time_ms=250\n");
    usleep(180000);
    assert(__atomic_load_n(&pfx_time_ms, __ATOMIC_ACQUIRE) == 250);
    memset(master_pfx, 0, sizeof master_pfx);
    memset(master_ms, 0, sizeof master_ms); sc_ms_prepare(&master_ms[0]);
    for (unsigned n = 0; n < 512; ++n) {
        float dry = 0.3f * sinf((float)n * 0.1f), l, r;
        const float *in[] = {&dry, &dry}; float *out[] = {&l, &r};
        hook_master_output(NULL, NULL, 2, in, 2, out, 1);
        assert(l == dry && r == dry);
    }

    write_file(conf_path, "pioneer_fx=space\nfx_time_mode=ms\nfx_time_ms=250\n");
    usleep(180000);
    struct sc_master_pioneer_fx ref_space = {0};
    memset(master_pfx, 0, sizeof master_pfx);
    for (unsigned n = 0; n < 512; ++n) {
        float dry = 0.2f * sinf((float)n * 0.07f), l, r, rl = dry, rr = dry;
        const float *in[] = {&dry, &dry}; float *out[] = {&l, &r};
        hook_master_output(NULL, NULL, 2, in, 2, out, 1);
        sc_master_pioneer_fx_planar(&ref_space, &rl, &rr, 1, SC_MASTER_PFX_SPACE);
        assert(memcmp(&l, &rl, 4) == 0 && memcmp(&r, &rr, 4) == 0);
    }

    // 3. Hidden Color FX Style on JC11 PRIME 4 (fx_selector_master_bypass=1) behaves as Off even with spiral + ms stored.
    write_file(conf_path, "pioneer_fx=spiral\nfx_time_mode=ms\nfx_time_ms=250\n");
    usleep(180000);
    __atomic_store_n(&fx_selector_master_bypass, 1, __ATOMIC_RELEASE);
    memset(master_pfx, 0, sizeof master_pfx);
    memset(master_ms, 0, sizeof master_ms); sc_ms_prepare(&master_ms[0]);
    for (unsigned n = 0; n < 512; ++n) {
        float dry = 0.25f * cosf((float)n * 0.05f), l, r;
        const float *in[] = {&dry, &dry}; float *out[] = {&l, &r};
        hook_master_output(NULL, NULL, 2, in, 2, out, 1);
        assert(l == dry && r == dry);
    }
    __atomic_store_n(&fx_selector_master_bypass, 0, __ATOMIC_RELEASE);

    // 4. Standalone players hide Color FX Style (no PX effects on SC5000/SC6000): a stored style behaves as Off.
    for (unsigned p = 0; p < sizeof players / sizeof *players; ++p) {
        write_file(board_path, players[p]);
        write_file(conf_path, "pioneer_fx=space\nfx_time_mode=ms\nfx_time_ms=250\n");
        usleep(180000);
        assert(__atomic_load_n(&pioneer_fx_mode, __ATOMIC_RELAXED) == 0);
        memset(master_pfx, 0, sizeof master_pfx);
        memset(master_ms, 0, sizeof master_ms); sc_ms_prepare(&master_ms[0]);
        for (unsigned n = 0; n < 512; ++n) {
            float dry = 0.2f * sinf((float)n * 0.07f), l, r;
            const float *in[] = {&dry, &dry}; float *out[] = {&l, &r};
            hook_master_output(NULL, NULL, 2, in, 2, out, 1);
            assert(l == dry && r == dry);
        }
    }
    unlink(board_path);
    unlink(conf_path);
    rmdir(dir);
    puts("master hidden-means-off PASS");
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
    test_hidden_means_off();
    puts("test_ms_master_hook PASS");
    return 0;
}
