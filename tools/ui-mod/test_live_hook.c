/* #50 Live Stream through the real hook_master_output: a stand-in device callback
 * renders 4 channels at 96 kHz in real time; a FIFO reader must get the first pair
 * (master) as 48 kHz s16le, and nothing from pair 2.
 * gcc -O2 tools/ui-mod/test_live_hook.c -ldl -lpthread -lm -o /tmp/test_live_hook && /tmp/test_live_hook
 * Also armhf: arm-linux-gnueabihf-gcc -O2 -mthumb -march=armv7-a -mfpu=neon -mfloat-abi=hard ... */
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#define SC_TAP_PATH "/tmp/test-live-hook.pcm"
#include "engine_ui_hook.c"
#include <assert.h>
#include <math.h>

static uint64_t rendered;
static uint32_t device(void *host, void *context, int ic, const float *const *in,
                       int oc, float *const *out, int frames) {
    (void)host; (void)context; (void)ic; (void)in;
    for (int i = 0; i < frames; i++, rendered++) {
        float v = (float)(0.25 * sin(2.0 * M_PI * (double)((440 * rendered) % 96000) / 96000.0));
        out[0][i] = v;      /* master L */
        out[1][i] = -v;     /* master R */
        out[2][i] = 0.9f;   /* booth/cue: must not reach the stream */
        out[3][i] = 0.9f;
    }
    return (uint32_t)oc;
}

int main(void) {
    unlink(SC_TAP_PATH);
    dsp_originals[DSP_MASTER] = (void *)device;
    static float b[4][480];
    float *out[4] = {b[0], b[1], b[2], b[3]};
    /* No reader: the hook leaves the tap untouched. */
    assert(hook_master_output(NULL, NULL, 0, NULL, 4, out, 480) == 4);
    assert(live_tap.head == 0);
    assert(sc_tap_drain_once(&live_tap, SC_TAP_PATH) == 0 && !live_tap.on);
    /* A reader appears (stream-sync opens the FIFO). */
    int rfd = open(SC_TAP_PATH, O_RDONLY | O_NONBLOCK);
    assert(rfd >= 0);
    sigset_t set;
    sigemptyset(&set);
    sigaddset(&set, SIGPIPE);
    pthread_sigmask(SIG_BLOCK, &set, NULL);
    static int16_t got[48000 * 3 * 2];
    size_t frames = 0;
    uint64_t t0 = sc_tap_now_ns();
    for (uint64_t n = 1; n <= 500; n++) { /* 2.5 s of 5 ms callbacks */
        assert(hook_master_output(NULL, NULL, 0, NULL, 4, out, 480) == 4);
        if (n % 2 == 0) sc_tap_drain_once(&live_tap, SC_TAP_PATH);
        ssize_t r = read(rfd, got + frames * 2, sizeof got - frames * 4);
        if (r > 0) frames += (size_t)r / 4;
        uint64_t due = t0 + n * 5000000u;
        struct timespec ts = {(time_t)(due / 1000000000u), (long)(due % 1000000000u)};
        clock_nanosleep(CLOCK_MONOTONIC, TIMER_ABSTIME, &ts, NULL);
    }
    double sum = 0;
    int peak = 0;
    for (size_t i = 0; i < frames; i++) {
        sum += (double)got[2 * i] * got[2 * i];
        if (abs(got[2 * i]) > peak) peak = abs(got[2 * i]);
        assert(abs(got[2 * i] + got[2 * i + 1]) <= 2); /* R = -L: pair 0, not pair 1 */
    }
    double rms = frames ? sqrt(sum / frames) / 32767.0 : 0;
    printf("hook_master_output -> FIFO: device %u Hz, %zu frames at 48 kHz, RMS %.3f (0.25 sine -> 0.177), peak %d\n",
           live_tap.rate, frames, rms, peak);
    assert(live_tap.rate == 96000);
    assert(frames > 48000 && rms > 0.16 && rms < 0.19 && peak < 0.26 * 32767); /* pair 2 (0.9) never leaked */
    close(rfd);
    unlink(SC_TAP_PATH);
    puts("live hook: PASS");
    return 0;
}
