/* #237: the hook's own audio-thread functions call nothing that allocates, locks, waits or
 * does I/O, and each block stays inside its real-time duration with everything switched on:
 * the four-deck pad, Echo Out, the master effects with their mode changing, auto gain, the
 * limiter, the live tap, a Bluetooth speaker, and the DSP guards and event hooks around them.
 *
 * test_rt_budget.c bans those calls in the DSP headers at build time. engine_ui_hook.c uses
 * them outside the audio path, so here the linker wraps them and a wrapper reached while an
 * audio function runs fails the test. What this does not see: calls made inside libc, and
 * the file calls the hook interposes itself (open, stat, access); their first use is caught
 * through dlsym. clock_gettime is allowed: one read a master block measures its rate.
 *
 *   W=-Wl,--wrap=malloc,--wrap=calloc,--wrap=realloc,--wrap=free,--wrap=pthread_mutex_lock
 *   W=$W,--wrap=pthread_cond_wait,--wrap=read,--wrap=write,--wrap=close,--wrap=usleep
 *   W=$W,--wrap=nanosleep,--wrap=poll,--wrap=fwrite,--wrap=fputs,--wrap=puts,--wrap=fprintf
 *   W=$W,--wrap=vfprintf,--wrap=printf,--wrap=fopen,--wrap=readlink,--wrap=dlsym
 *   gcc -std=gnu11 -O2 -U_FORTIFY_SOURCE tools/ui-mod/test_rt_callback.c $W -ldl -lpthread -lm -o rt-callback
 *   ./rt-callback && ./rt-callback selftest
 * Also armhf: arm-linux-gnueabihf-gcc -mthumb -march=armv7-a -mfpu=neon -mfloat-abi=hard.
 * "selftest" sleeps inside an audio block on purpose and passes only when the guard saw it.
 * Host-side like test_rt_budget.c: the budget is the block's duration at 96 kHz, on the
 * build host's CPU, and a block's time is the least it took in three identical passes (a
 * shared host preempts a block now and then; a wait in the code costs every pass).
 * No Engine, rootfs or device is needed. */
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>
#include <math.h>
#include <poll.h>

static __thread int in_audio; /* an audio function of the hook is on the stack */
static unsigned banned;
static const char *banned_name = "";
static void seen(const char *name) {
    if (in_audio) { banned++; banned_name = name; }
}

#define WRAP(ret, name, params, args) \
    ret __real_##name params;         \
    ret __wrap_##name params { seen(#name); return __real_##name args; }
WRAP(void *, malloc, (size_t n), (n))
WRAP(void *, calloc, (size_t n, size_t m), (n, m))
WRAP(void *, realloc, (void *p, size_t n), (p, n))
WRAP(void, free, (void *p), (p))
WRAP(int, pthread_mutex_lock, (pthread_mutex_t *m), (m))
WRAP(int, pthread_cond_wait, (pthread_cond_t *c, pthread_mutex_t *m), (c, m))
WRAP(ssize_t, read, (int fd, void *b, size_t n), (fd, b, n))
WRAP(ssize_t, write, (int fd, const void *b, size_t n), (fd, b, n))
WRAP(int, close, (int fd), (fd))
WRAP(int, usleep, (useconds_t u), (u))
WRAP(int, nanosleep, (const struct timespec *a, struct timespec *b), (a, b))
WRAP(int, poll, (struct pollfd *f, nfds_t n, int t), (f, n, t))
WRAP(size_t, fwrite, (const void *p, size_t s, size_t n, FILE *f), (p, s, n, f))
WRAP(int, fputs, (const char *s, FILE *f), (s, f))
WRAP(int, puts, (const char *s), (s))
WRAP(int, vfprintf, (FILE *f, const char *fmt, va_list ap), (f, fmt, ap))
WRAP(FILE *, fopen, (const char *p, const char *m), (p, m))
WRAP(ssize_t, readlink, (const char *p, char *b, size_t n), (p, b, n))
WRAP(void *, dlsym, (void *h, const char *n), (h, n))
int __wrap_fprintf(FILE *f, const char *fmt, ...) {
    seen("fprintf");
    va_list ap;
    va_start(ap, fmt);
    int rc = __real_vfprintf(f, fmt, ap);
    va_end(ap);
    return rc;
}
int __wrap_printf(const char *fmt, ...) {
    seen("printf");
    va_list ap;
    va_start(ap, fmt);
    int rc = __real_vfprintf(stdout, fmt, ap);
    va_end(ap);
    return rc;
}

enum { MAX_FRAMES = 1024, WAVE = 1024 };
static float wave[WAVE], buffers[8][MAX_FRAMES];
static float *out[8], *pair[2] = {buffers[0], buffers[1]};
static unsigned char sender_self[128], owner[0x400], handler[64], event[64];
static unsigned long long clock_ns = 1;
static unsigned phase;
static int trip; /* selftest: make one banned call inside an audio block */

static unsigned long long fake_clock(void) { return clock_ns; }
static void fake_sender(void *self, const float *const *in, int frames) { (void)self; (void)in; (void)frames; }
static void fake_event(void *h, const void *e) { (void)h; (void)e; }
static void fake_process(void *self, float *const *in, float *const *o, uint32_t frames) {
    (void)self; (void)in; (void)o; (void)frames;
}
/* Engine fills every output and calls its Bluetooth sender inside the callback. */
static uint32_t fake_master(void *host, void *context, int ic, const float *const *in, int oc, float *const *o, int frames) {
    (void)host; (void)context; (void)ic; (void)in;
    for (int ch = 0; ch < oc; ch++)
        for (int i = 0; i < frames; i++) o[ch][i] = 0.4f * wave[(phase + (unsigned)i * (unsigned)(ch + 3)) & (WAVE - 1)];
    hook_bt_send(sender_self, NULL, frames);
    return (uint32_t)oc;
}

/* Everything the hook runs on Engine's audio and render threads for one block. */
static void audio_block(int channels, int frames) {
    in_audio = 1;
    assert(hook_master_output(NULL, NULL, 0, NULL, channels, out, frames) == (uint32_t)(four_deck_pad_on && channels == 4 ? 8 : channels));
    hook_autogate_process(owner, pair, pair, (uint32_t)frames);
    hook_dub_echo(owner, pair, pair, (uint32_t)frames);
    hook_wash(owner, pair, pair, (uint32_t)frames);
    hook_flanger(owner, pair, pair, (uint32_t)frames);
    hook_fx_common_chain(owner, pair, pair, (uint32_t)frames);
    hook_fx_turn_off(handler, event); hook_fx_turn_on(handler, event); hook_fx_switch(handler, event);
    hook_fx_change(handler, event); hook_fx_enable(handler, event);
    hook_fx_mix(handler, event); hook_fx_parameter(handler, event);
    hook_playback_nudge(handler, event); hook_playback_tempo(handler, event); hook_time_stretch_jump(handler, event);
    hook_ramp_time(handler, event); hook_motor_ramp_time(handler, event);
    hook_sampler_immediate(handler, event); hook_sampler_stop(handler, event); hook_sampler_play(handler, event);
    if (trip) usleep(0);
    in_audio = 0;
}

static unsigned long long now_ns(void) {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (unsigned long long)t.tv_sec * 1000000000ull + (unsigned long long)t.tv_nsec;
}

int main(int argc, char **argv) {
    trip = argc > 1 && !strcmp(argv[1], "selftest");
    for (int i = 0; i < WAVE; i++) wave[i] = sinf((float)i * 6.2831853f / WAVE);
    for (int ch = 0; ch < 8; ch++) out[ch] = buffers[ch];
    for (int slot = 0; slot < DSP_HOOK_COUNT; slot++) dsp_originals[slot] = (void *)fake_event;
    dsp_originals[DSP_AUTOGATE] = dsp_originals[DSP_COMMON] = dsp_originals[DSP_DUB_ECHO] = (void *)fake_process;
    dsp_originals[DSP_WASH] = dsp_originals[DSP_FLANGER] = (void *)fake_process;
    dsp_originals[DSP_MASTER] = (void *)fake_master;
    /* A MemberFunctionHandler bound to `owner` (dsp_bound_owner), and a finite event for every hook. */
    void *bound = owner;
    memcpy(handler + (sizeof(void *) == 8 ? 24 : 0x0c), &bound, sizeof bound);
    const double one = 1.0;
    const float half = 0.5f;
    memcpy(event + 0x08, &one, sizeof one);
    memcpy(event + 0x20, &half, sizeof half);
    hook_test_clock = fake_clock;
    bt_send_orig = fake_sender;
    stream_rate = 44100;
    echo_enabled = gain_enabled = lim_enabled = 1;
    live_tap.on = 1;
    for (unsigned p = 0; p < 4; p++) sc_ms_prepare(&master_ms[p]); /* the polling worker's part */

    static const int sizes[] = {64, 225, 256, MAX_FRAMES};
    enum { SIZES = sizeof sizes / sizeof *sizes, LAYOUTS = 3, BLOCKS = 700, PASSES = 4 };
    static unsigned long long best[SIZES][LAYOUTS][BLOCKS];
    unsigned misses = 0, blocks = 0;
    unsigned long long worst = 0, worst_budget = 1;
    /* Pass 0 makes the state resident, as Engine's locked memory is on a deck; the others are timed. */
    for (int pass = 0; pass < PASSES; pass++)
        for (unsigned n = 0; n < SIZES; n++)
            for (int layout = 0; layout < LAYOUTS; layout++) { /* a player, a player in the four-deck layout, eight outputs */
                int frames = sizes[n], channels = layout == 2 ? 8 : 4;
                four_deck_pad_on = layout == 1;
                unsigned long long budget = (unsigned long long)frames * 1000000000ull / 96000ull;
                for (unsigned block = 0; block < BLOCKS; block++) { /* over 1 s at 48 kHz: the Bluetooth rate is measured */
                    __atomic_store_n(&pioneer_fx_mode, (int)(block / 8 % 6), __ATOMIC_RELAXED);
                    __atomic_store_n(&pfx_time_ms, block / 48 % 2 ? 10u : 0u, __ATOMIC_RELEASE);
                    __atomic_add_fetch(&bt_encodes, 1, __ATOMIC_RELEASE); /* the speaker's encoder ran */
                    clock_ns += (unsigned long long)frames * 1000000000ull / 48000ull;
                    phase += (unsigned)frames;
                    unsigned long long start = now_ns();
                    audio_block(channels, frames);
                    unsigned long long elapsed = now_ns() - start, *least = &best[n][layout][block];
                    if (pass && (pass == 1 || elapsed < *least)) *least = elapsed;
                    if (pass < PASSES - 1) continue;
                    blocks++;
                    if (*least > budget) misses++;
                    if (*least * worst_budget > worst * budget) { worst = *least; worst_budget = budget; }
                }
            }
    assert(bt_render == 48000 && bt_idle < BT_IDLE); /* the Bluetooth path really ran */
    printf("rt-callback blocks=%u misses=%u worst_ns=%llu of budget_ns=%llu banned=%u %s\n", blocks, misses, worst, worst_budget, banned, banned_name);
    if (trip) return banned && !strcmp(banned_name, "usleep") ? EXIT_SUCCESS : EXIT_FAILURE;
    return banned || misses ? EXIT_FAILURE : EXIT_SUCCESS;
}
