/* #336 S13: master-bus NaN/Inf sanitization (even with limiter & auto-gain off)
 * and bounded ALSA snd_pcm_writei -EBADFD recovery.
 *
 * Build & run on build host (native AArch64 and armhf):
 *   gcc -std=gnu11 -O2 -Wall -Wextra -U_FORTIFY_SOURCE tools/ui-mod/test_s13_audio_guards.c -ldl -lpthread -lm -o /tmp/test_s13 && /tmp/test_s13
 */
#define _GNU_SOURCE
#define ENGINE_UI_HOOK_TEST
int hook_test_engine = 1;
#include "engine_ui_hook.c"
#include <assert.h>
#include <math.h>
#include <stdio.h>
#include <string.h>

static uint32_t fake_master_with_nonfinite(void *host, void *ctx, int in_ch, const float *const *in,
                                           int out_ch, float *const *out, int frames) {
    (void)host; (void)ctx; (void)in_ch; (void)in;
    for (int ch = 0; ch < out_ch; ch++) {
        for (int i = 0; i < frames; i++) out[ch][i] = 0.25f * (float)(ch + 1);
    }
    if (out_ch >= 2 && frames >= 8) {
        out[0][1] = NAN;
        out[0][2] = INFINITY;
        out[0][3] = -INFINITY;
        out[1][4] = NAN;
        out[1][5] = INFINITY;
    }
    if (out_ch >= 4 && frames >= 8) {
        out[2][0] = -INFINITY;
        out[3][7] = NAN;
    }
    return 0;
}

static void test_master_sanitizes_with_limiter_off(void) {
    dsp_originals[DSP_MASTER] = (void *)fake_master_with_nonfinite;
    four_deck_pad_on = 0;
    echo_enabled = 0;
    pioneer_fx_mode = 0;
    hifi_enabled = 0;
    gain_enabled = 0;
    lim_enabled = 0;
    bt_send_orig = NULL;
    live_tap.on = 0;

    float b0[16], b1[16], b2[16], b3[16];
    float *out[4] = {b0, b1, b2, b3};
    assert(hook_master_output(NULL, NULL, 0, NULL, 4, out, 16) == 0);

    for (int ch = 0; ch < 4; ch++) {
        for (int i = 0; i < 16; i++) {
            assert(isfinite(out[ch][i]));
        }
    }
    assert(out[0][1] == 0.0f && out[0][2] == 0.0f && out[0][3] == 0.0f);
    assert(out[1][4] == 0.0f && out[1][5] == 0.0f);
    assert(out[2][0] == 0.0f && out[3][7] == 0.0f);
    /* Finite samples must remain bit-exact when limiter/gain/FX are off. */
    assert(out[0][0] == 0.25f && out[1][0] == 0.50f && out[2][1] == 0.75f && out[3][0] == 1.0f);
    puts("S13 master sanitizer (limiter/gain/FX off): PASS");
}

static void test_master_sanitizes_before_echo_and_hifi(void) {
    dsp_originals[DSP_MASTER] = (void *)fake_master_with_nonfinite;
    echo_enabled = 1;
    hifi_enabled = 1;
    gain_enabled = 0;
    lim_enabled = 0;
    memset(master_echo, 0, sizeof master_echo);
    memset(master_hifi, 0, sizeof master_hifi);

    float b0[16], b1[16];
    float *out[2] = {b0, b1};
    for (int block = 0; block < 4; block++) {
        assert(hook_master_output(NULL, NULL, 0, NULL, 2, out, 16) == 0);
        for (int ch = 0; ch < 2; ch++)
            for (int i = 0; i < 16; i++)
                assert(isfinite(out[ch][i]));
    }
    echo_enabled = 0;
    hifi_enabled = 0;
    puts("S13 master sanitizer (before echo/hifi): PASS");
}

struct mock_pcm {
    int state;
    long first_rc;
    long retry_rc;
    int prepare_rc;
    int resume_rc;
    int write_calls;
    int prepare_calls;
    int resume_calls;
};

static long mock_writei(void *pcm, const void *buf, unsigned long size) {
    (void)buf; (void)size;
    struct mock_pcm *m = (struct mock_pcm *)pcm;
    m->write_calls++;
    return m->write_calls == 1 ? m->first_rc : m->retry_rc;
}
static int mock_state(void *pcm) { return ((struct mock_pcm *)pcm)->state; }
static int mock_prepare(void *pcm) {
    struct mock_pcm *m = (struct mock_pcm *)pcm;
    m->prepare_calls++;
    if (m->prepare_rc >= 0) m->state = 2; /* PREPARED */
    return m->prepare_rc;
}
static int mock_resume(void *pcm) {
    struct mock_pcm *m = (struct mock_pcm *)pcm;
    m->resume_calls++;
    if (m->resume_rc >= 0) m->state = 3; /* RUNNING */
    return m->resume_rc;
}

static void test_alsa_ebadfd_recovery(void) {
    /* 1. XRUN/SETUP/DRAINING + -EBADFD -> prepare once, retry once, succeeds. */
    int recoverable[] = {1, 4, 5};
    for (unsigned i = 0; i < sizeof recoverable / sizeof *recoverable; i++) {
        struct mock_pcm m = {.state = recoverable[i], .first_rc = -EBADFD, .retry_rc = 256, .prepare_rc = 0};
        long rc = alsa_writei_recover(&m, "x", 256, mock_writei, mock_state, mock_prepare, mock_resume);
        assert(rc == 256 && m.prepare_calls == 1 && m.write_calls == 2 && m.resume_calls == 0);
    }
    /* 2. SUSPENDED + -EBADFD: resume succeeds -> retry once without prepare. */
    {
        struct mock_pcm m = {.state = 7, .first_rc = -EBADFD, .retry_rc = 128, .resume_rc = 0};
        long rc = alsa_writei_recover(&m, "x", 128, mock_writei, mock_state, mock_prepare, mock_resume);
        assert(rc == 128 && m.resume_calls == 1 && m.prepare_calls == 0 && m.write_calls == 2);
    }
    /* 3. SUSPENDED + -EBADFD: resume returns -ENOSYS -> fall back to prepare and retry once. */
    {
        struct mock_pcm m = {.state = 7, .first_rc = -EBADFD, .retry_rc = 128, .resume_rc = -ENOSYS, .prepare_rc = 0};
        long rc = alsa_writei_recover(&m, "x", 128, mock_writei, mock_state, mock_prepare, mock_resume);
        assert(rc == 128 && m.resume_calls == 1 && m.prepare_calls == 1 && m.write_calls == 2);
    }
    /* 4. DISCONNECTED (8), PAUSED (6), OPEN (0) -> never prepare, never retry. */
    int untouched[] = {0, 6, 8};
    for (unsigned i = 0; i < sizeof untouched / sizeof *untouched; i++) {
        struct mock_pcm m = {.state = untouched[i], .first_rc = -EBADFD, .retry_rc = 256};
        long rc = alsa_writei_recover(&m, "x", 256, mock_writei, mock_state, mock_prepare, mock_resume);
        assert(rc == -EBADFD && m.prepare_calls == 0 && m.resume_calls == 0 && m.write_calls == 1);
    }
    /* 5. Persistent prepare failure -> single attempt, no loop. */
    {
        struct mock_pcm m = {.state = 4, .first_rc = -EBADFD, .prepare_rc = -EIO};
        long rc = alsa_writei_recover(&m, "x", 256, mock_writei, mock_state, mock_prepare, mock_resume);
        assert(rc == -EBADFD && m.prepare_calls == 1 && m.write_calls == 1);
    }
    /* 6. Normal write and -EPIPE pass straight through untouched. */
    {
        struct mock_pcm ok = {.state = 3, .first_rc = 256};
        assert(alsa_writei_recover(&ok, "x", 256, mock_writei, mock_state, mock_prepare, mock_resume) == 256);
        assert(ok.write_calls == 1 && ok.prepare_calls == 0);
        struct mock_pcm ep = {.state = 4, .first_rc = -EPIPE};
        assert(alsa_writei_recover(&ep, "x", 256, mock_writei, mock_state, mock_prepare, mock_resume) == -EPIPE);
        assert(ep.write_calls == 1 && ep.prepare_calls == 0);
    }
    puts("S13 ALSA -EBADFD recovery: PASS");
}

int main(void) {
    test_master_sanitizes_with_limiter_off();
    test_master_sanitizes_before_echo_and_hifi();
    test_alsa_ebadfd_recovery();
    puts("test_s13_audio_guards: all passed");
    return 0;
}
