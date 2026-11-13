/* Bluetooth output: while a speaker listens (the encoder runs), hook_master_output keeps the pair bt_out= selects (brought
 * from the render rate to the A2DP stream rate), and hook_bt_send, which Engine calls inside the next callback, hands
 * exactly that to BluetoothSignalSender::audio, which otherwise gets silence. With nobody listening nothing runs.
 * arm-linux-gnueabihf-gcc -O2 -mthumb -march=armv7-a -mfpu=neon -mfloat-abi=hard tools/ui-mod/test_bt_out.c -ldl -lpthread -lm -o /tmp/test_bt_out
 * Also native aarch64: gcc -O2 tools/ui-mod/test_bt_out.c -ldl -lpthread -lm. No Engine or rootfs needed.
 * The tests below the "regressions" line are written for the first versions of this feature too (-DBT_OUT_OLD with
 * 6e62523's engine_ui_hook.c for clip, blocks, rate and source; -DBT_OUT_FB with fb33354's for deck; the test name as the argument):
 * each must fail there. The callbacks run on a simulated clock (the hook measures their rate on it). */
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>
#include <math.h>

/* Pair p of the device carries p+1 tenths on L and -(p+1) tenths on R, times level. With tone set, pair 0 is a continuous
 * sine (amplitude 0.5) at device_rate: tone Hz on L and, in antiphase, tone_r (default tone) Hz on R. wave 1 is a +-1
 * square, wave 2 a quiet signal with a NaN and both infinities in it. */
static float level = 1.0f, tone, tone_r, device_rate;
static int wave, nested = 1;
static unsigned long long played;
static double sim_t, sim_noise, sim_stall; /* ns on the callbacks' clock; noise +-ns on each reading; a one-off stall */
#if !defined(BT_OUT_OLD) && !defined(BT_OUT_FB)
static unsigned long long sim_clock(void) {
    static unsigned x = 1;
    x = x * 1664525u + 1013904223u;
    return (unsigned long long)(sim_t + sim_noise * ((double)(x >> 8) / 8388608.0 - 1.0) + 2e6);
}
#endif

static const float *engine_feed[2];
static float silence[2][4096];
static int stock_calls;

/* The original master callback; Engine calls the sender inside it, before its output exists. */
static uint32_t device(void *host, void *context, int ic, const float *const *in, int oc, float *const *out, int frames) {
    (void)host; (void)context; (void)ic; (void)in;
    sim_t += (double)frames * 1e9 / (double)device_rate + sim_stall;
    sim_stall = 0;
    if (nested) hook_bt_send(NULL, engine_feed, frames);
    for (int c = 0; c < oc; c++)
        for (int i = 0; i < frames; i++) {
            unsigned long long t = played + (unsigned long long)i;
            float v;
            if (wave == 1) v = ((t / 32) & 1) ? 1.0f : -1.0f;
            else if (wave == 2) v = t == 200 ? NAN : t == 300 ? INFINITY : t == 400 ? -INFINITY : 0.1f;
            else if (tone && c < 2) v = 0.5f * (float)sin(2 * M_PI * (double)(c && tone_r ? tone_r : tone) * (double)t / (double)device_rate) * (c ? -1.0f : 1.0f);
            else v = (c % 2 ? -0.1f : 0.1f) * (float)(c / 2 + 1) * level;
            out[c][i] = v;
        }
    played += (unsigned long long)frames;
    return (uint32_t)oc;
}

static int rt_queue; /* the speaker's queue in the real-time test: frames the sender gave it and its encoder has not taken */
static float got[2][24576];
static int got_n, longest_call, collect = 1; /* collect: keep the samples; otherwise only count them */
static long long delivered; /* stream frames the sender was given */
static void sender(void *self, const float *const *in, int frames) {
    (void)self;
    if (in == engine_feed) {
        stock_calls++; /* Engine's own feed: counted, not collected */
        return;
    }
    if (frames > longest_call) longest_call = frames;
    delivered += frames;
    __atomic_add_fetch(&rt_queue, frames, __ATOMIC_RELAXED);
    if (!collect) return;
    assert(got_n + frames <= (int)(sizeof got[0] / sizeof got[0][0]));
    for (int c = 0; c < 2; c++) memcpy(got[c] + got_n, in[c], (size_t)frames * sizeof(float));
    got_n += frames;
}

static float b[10][4096];
static float *out[10];
static unsigned long sbc_fake[2];
static int listening = 1, last_frames;

static void set_stream(unsigned hz) {
    ((uint8_t *)sbc_fake)[sizeof(unsigned long)] = hz == 44100 ? 2 : hz == 48000 ? 3 : 200;
    stream_rate = hz;
}

/* One master callback of `frames` while the speaker's encoder runs. */
static void block(int channels, int frames) {
    if (listening) sbc_encode(sbc_fake, NULL, 0, NULL, 0, NULL);
    last_frames = frames;
    assert(hook_master_output(NULL, NULL, 0, NULL, channels, out, frames) == (uint32_t)channels);
}

/* A call without outputs, which a player makes as often as one with. */
static void block0(int frames) {
    if (listening) sbc_encode(sbc_fake, NULL, 0, NULL, 0, NULL);
    assert(hook_master_output(NULL, NULL, 0, NULL, 0, NULL, frames) == 0);
}

/* The sender call Engine makes inside the next callback: it takes the last block. */
static void drain(void) {
    hook_bt_send(NULL, engine_feed, last_frames);
#ifdef BT_OUT_OLD
    bt_frames = -1; /* that version did not consume the block it sent */
#endif
}

static void fresh(void) {
#ifdef BT_OUT_OLD
    bt_phase = 0; bt_ip = -2; bt_rem = 0; bt_frames = -1;
    memset(bt_x, 0, sizeof bt_x); memset(bt_y, 0, sizeof bt_y);
#else
    bt_off();
#endif
}

/* A render at render_hz sent to a stream at stream_hz (0: none yet), from a clean start. */
static void reset(unsigned render_hz, unsigned stream_hz) {
    got_n = 0; delivered = 0; collect = 1; played = 0; longest_call = 0; stock_calls = 0; wave = 0; tone = tone_r = 0; level = 1.0f; listening = 1; nested = 1;
    device_rate = (float)render_hz;
    sim_t = sim_noise = sim_stall = 0;
#if defined(BT_OUT_OLD) || defined(BT_OUT_FB)
    render_rate[4] = render_rate[10] = render_hz;
#else
    bt_render = render_hz;
#endif
    bt_out_pair = 0;
    set_stream(stream_hz);
    fresh();
#ifndef BT_OUT_OLD
    bt_idle = BT_IDLE; bt_seen = bt_encodes;
#endif
}

/* Peak of the sine of freq Hz in the last 2000 stream frames, by correlation (handles any phase). */
static double amplitude(const float *x, int n, double freq, double rate) {
    double s = 0, c = 0;
    for (int i = n - 2000; i < n; i++) {
        s += x[i] * sin(2 * M_PI * freq * i / rate);
        c += x[i] * cos(2 * M_PI * freq * i / rate);
    }
    return 2 * sqrt(s * s + c * c) / 2000.0;
}

/* total render frames of a tone at tone_hz, in blocks of `cut`, to a stream at stream_hz; returns the stream frames */
static int run(unsigned render_hz, unsigned stream_hz, float tone_hz, int total, int cut) {
    reset(render_hz, stream_hz);
    tone = tone_hz;
    for (int done = 0; done < total; done += cut) block(4, cut);
    drain();
    tone = 0;
    return got_n;
}

static double max_step(const float *x, int from, int to) {
    double m = 0;
    for (int i = from + 1; i < to; i++) m = fmax(m, fabs((double)x[i] - (double)x[i - 1]));
    return m;
}

#ifndef BT_OUT_OLD
/* The sum of everything the pipeline keeps: it does not move while nobody listens. */
static double bt_sum(void) {
    double s = bt_pend;
    for (int c = 0; c < 2; c++) {
        for (unsigned i = 0; i < sizeof bt_x[0] / sizeof(float); i++) s += fabs(bt_x[c][i]);
        for (unsigned i = 0; i < sizeof bt_h[0] / sizeof(float); i++) s += fabs(bt_h[c][i]);
        for (unsigned i = 0; i < sizeof bt_y[0] / sizeof(float); i++) s += fabs(bt_y[c][i]);
        for (unsigned i = 0; i < sizeof bt_buf[0] / sizeof(float); i++) s += fabs(bt_buf[c][i]);
    }
    return s;
}
#endif

/* ---- regressions ---- */

/* The filters overshoot a +-1 signal (1.08 at 96 -> 48 kHz, 1.15 at 48 -> 44.1 kHz) and the sender wraps what is past
 * full scale into the opposite polarity: nothing past +-1 may reach it, and a signal that is not a number is silence. */
static void test_clip(void) {
    static const unsigned rates[][2] = {{96000, 48000}, {48000, 44100}, {96000, 44100}};
    for (unsigned r = 0; r < 3; r++) {
        reset(rates[r][0], rates[r][1]);
        wave = 1;
        for (int i = 0; i < 10; i++) block(4, 480);
        drain();
        float peak = 0;
        for (int i = 0; i < got_n; i++) peak = fmaxf(peak, fabsf(got[0][i]));
        assert(got_n > 0 && peak <= 1.0f && peak > 0.9f); /* saturated, not attenuated */
    }
    reset(96000, 44100);
    wave = 2;
    for (int i = 0; i < 4; i++) block(4, 480);
    drain();
    for (int i = 0; i < got_n; i++) assert(isfinite(got[0][i]) && isfinite(got[1][i]) && fabsf(got[0][i]) <= 1.0f);
    /* and does not stay: the last bad frame (400) is out of every filter 43 frames later, the rest is the steady 0.1 */
    assert(got_n > 600);
    for (int i = 260; i < got_n; i++) assert(fabsf(got[0][i] - 0.1f) < 0.002f && fabsf(got[1][i] - 0.1f) < 0.002f);
}

/* Engine calls the sender inside the callback, so the block it takes was kept by the callback before, whatever
 * length that was: a run of different lengths never falls back to Engine's own feed, and loses and repeats nothing. */
static void test_blocks(void) {
    static const int cuts[] = {450, 451, 449, 450, 451, 449, 450, 450, 451, 449, 450, 451, 449, 450, 451, 449};
    reset(96000, 44100);
    tone = 1000;
    int total = 0, calls_after_warmup;
    for (unsigned i = 0; i < sizeof cuts / sizeof *cuts; i++) {
        if (i == 1) stock_calls = 0;
        block(4, cuts[i]);
        total += cuts[i];
    }
    calls_after_warmup = stock_calls;
    drain();
    tone = 0;
    assert(calls_after_warmup == 0 && stock_calls == 0);
    assert(abs(got_n - (int)((long long)total * 44100 / 96000)) <= 2);
    assert(fabs(amplitude(got[0], got_n, 1000, 44100) - 0.5) < 0.005);
}

/* Switching to another speaker's stream rate drops the old state: from the second the new stream starts, it is the
 * same as one that had begun there, and the join is a smooth fade, not a step. */
static void test_rate(void) {
    enum { M = 12, BLK = 450 };
    static float a[4096], fresh_run[4096];
    reset(96000, 44100);
    tone = 1000;
    for (int i = 0; i < M; i++) block(4, BLK);
    drain();
    unsigned long long at = played;
    int before = got_n;
    set_stream(48000);
    for (int i = 0; i < M; i++) block(4, BLK);
    drain();
    int n = got_n - before;
    assert(n > 2000 && n < 4096 && before > 1000);
    memcpy(a, got[0] + before, (size_t)n * sizeof(float));
    /* a new stream beginning at the same place in the music */
    int split = got_n;
    played = at;
    fresh();
    for (int i = 0; i < M; i++) block(4, BLK);
    drain();
    int n2 = got_n - split;
    memcpy(fresh_run, got[0] + split, (size_t)n2 * sizeof(float));
    assert(abs(n - n2) <= 1);
    for (int i = 200; i < n && i < n2; i++) assert(fabsf(a[i] - fresh_run[i]) < 1e-6f);
    /* the join: no step beyond the tone's own (0.5 * 2 pi * 1 kHz / 44.1 kHz = 0.071 per frame) */
    assert(max_step(got[0], before - 50, before + 400) < 0.09);
}

/* A new source (another pair) is faded in from where the last one ended, not switched or smeared through the old history. */
static void test_source(void) {
    reset(96000, 44100);
    for (int i = 0; i < 8; i++) block(4, 450); /* pair 0: +0.1 */
    drain();
    int at = got_n;
    bt_out_pair = 1; /* +0.2 */
    for (int i = 0; i < 8; i++) block(4, 450);
    drain();
    assert(fabsf(got[0][at - 1] - 0.1f) < 0.001f && fabsf(got[0][got_n - 1] - 0.2f) < 0.001f);
    assert(max_step(got[0], at - 20, got_n) < 0.003); /* 0.1 over 128 frames is 0.0008 a frame */
}

/* The shape of a Gen-1 player: the callback is 225 frames 213 times a second (48 kHz) while ALSA, which the first version
 * read the render rate from, says 96 kHz. A second of it must make a second of the stream (44.1 kHz), the same tone. */
static void test_deck(void) {
    reset(48000, 44100);
#if defined(BT_OUT_OLD) || defined(BT_OUT_FB)
    render_rate[4] = 96000;
#else
    bt_render = 0; /* not measured yet */
#endif
    tone = 1000;
    collect = 0;
    for (int i = 0; i < 213 * 3; i++) block(4, 225);
    long long before = delivered;
    for (int i = 0; i < 213; i++) block(4, 225);
    drain();
    assert(llabs((delivered - before) - 44100) < 441); /* a second, within 1 % */
    collect = 1;
    got_n = 0;
    for (int i = 0; i < 53; i++) block(4, 225);
    drain();
    tone = 0;
    assert(fabs(amplitude(got[0], got_n, 1000, 44100) - 0.5) < 0.01 && amplitude(got[0], got_n, 2000, 44100) < 0.01);
}

#if !defined(BT_OUT_OLD) && !defined(BT_OUT_FB)
/* Real time, on the wall clock (opt-in, a few seconds: `test_bt_out realtime`): Engine's master callback of `frames` frames at
 * `hz` on this thread, and the speaker's encoder on another, taking 128 frames at the 44.1 kHz stream rate and calling
 * sbc_encode each time. The hook finds the callback's rate by itself, the encoder does not starve and the queue does not grow. */
static int rt_count;
static unsigned rt_periods, rt_starved;
static volatile int rt_stop;
static void add_ns(struct timespec *t, long long ns) {
    long long n = t->tv_nsec + ns;
    t->tv_sec += (time_t)(n / 1000000000ll);
    t->tv_nsec = (long)(n % 1000000000ll);
}
static void *rt_encoder(void *arg) {
    (void)arg;
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    while (!rt_stop) {
        add_ns(&t, 128ll * 1000000000ll / 44100);
        clock_nanosleep(CLOCK_MONOTONIC, TIMER_ABSTIME, &t, NULL);
        sbc_encode(sbc_fake, NULL, 0, NULL, 0, NULL);
        int have = __atomic_load_n(&rt_queue, __ATOMIC_RELAXED), counting = __atomic_load_n(&rt_count, __ATOMIC_RELAXED);
        if (counting) rt_periods++;
        if (have >= 128) __atomic_sub_fetch(&rt_queue, 128, __ATOMIC_RELAXED);
        else if (counting) rt_starved++;
    }
    return NULL;
}
static void test_realtime(unsigned hz, int frames) {
    reset(hz, 44100);
    bt_render = 0; /* not measured yet */
    collect = 0;
    rt_queue = 0; rt_count = 0; rt_periods = rt_starved = 0; rt_stop = 0;
    hook_test_clock = NULL;
    pthread_t th;
    assert(pthread_create(&th, NULL, rt_encoder, NULL) == 0);
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    long long period = (long long)frames * 1000000000ll / hz;
    for (long i = 0; i * period < 5500000000ll; i++) {
        if (i * period >= 2000000000ll) __atomic_store_n(&rt_count, 1, __ATOMIC_RELAXED); /* after the rate is found */
        add_ns(&t, period);
        clock_nanosleep(CLOCK_MONOTONIC, TIMER_ABSTIME, &t, NULL);
        assert(hook_master_output(NULL, NULL, 0, NULL, 4, out, frames) == 4);
    }
    rt_stop = 1;
    pthread_join(th, NULL);
    hook_test_clock = sim_clock;
    int queued = __atomic_load_n(&rt_queue, __ATOMIC_RELAXED);
    fprintf(stderr, "realtime %u Hz x %d: bt_render %u, encoder periods %u starved %u, queue %d, delivered %lld\n", hz, frames, bt_render, rt_periods, rt_starved, queued, delivered);
    assert(bt_render == hz);
    assert(rt_periods > 1000 && rt_starved * 50 < rt_periods); /* under 2 % */
    assert(queued < 3 * 4096);
}
#endif

int main(int argc, char **argv) {
    const char *only = argc > 1 ? argv[1] : NULL;
    for (int c = 0; c < 10; c++) out[c] = b[c];
    engine_feed[0] = silence[0]; engine_feed[1] = silence[1];
    hook_test_engine = 1;
#if !defined(BT_OUT_OLD) && !defined(BT_OUT_FB)
    hook_test_clock = sim_clock;
#endif
    dsp_originals[DSP_MASTER] = (void *)device;
    bt_send_orig = sender;

    if (only) {
        if (!strcmp(only, "clip")) test_clip();
        else if (!strcmp(only, "blocks")) test_blocks();
        else if (!strcmp(only, "rate")) test_rate();
        else if (!strcmp(only, "source")) test_source();
        else if (!strcmp(only, "deck")) test_deck();
#if !defined(BT_OUT_OLD) && !defined(BT_OUT_FB)
        else if (!strcmp(only, "realtime")) { test_realtime(48000, 225); test_realtime(96000, 450); test_realtime(44100, 225); }
#endif
        else return 2;
        printf("bt out %s: PASS\n", only);
        return 0;
    }
#if defined(BT_OUT_OLD) || defined(BT_OUT_FB)
    return 2;
#else
    /* The stream's sample rate is the encoder's, read from the sbc_t Engine passes to sbc_encode: frequency 2 is 44.1 kHz,
     * 3 is 48 kHz, others are none. Through the real interposer, and only for Engine. */
    for (unsigned f = 0; f < 5; f++) {
        unsigned long sbc[2] = {0, 0};
        ((uint8_t *)sbc)[sizeof(unsigned long)] = (uint8_t)(f == 4 ? 200 : f);
        unsigned counted = bt_encodes;
        stream_rate = 1;
        sbc_encode(sbc, NULL, 0, NULL, 0, NULL);
        assert(bt_encodes == counted + (f == 4 ? 0 : 1)); /* a rate libsbc does not have is not a heartbeat */
        assert(stream_rate == (f == 2 ? 44100u : f == 3 ? 48000u : f == 0 ? 16000u : f == 1 ? 32000u : 1u));
        hook_test_engine = 0;
        stream_rate = 1;
        sbc_encode(sbc, NULL, 0, NULL, 0, NULL);
        assert(bt_encodes == counted + (f == 4 ? 0 : 1) && stream_rate == 1); /* not Engine: untouched */
        hook_test_engine = 1;
    }

    /* Nobody listening (no encoder call): Engine's own feed goes through, and nothing is processed or kept. */
    reset(96000, 44100);
    double before_idle = bt_sum();
    listening = 0;
    for (int i = 0; i < BT_IDLE + 8; i++) block(4, 450);
    assert(stock_calls == BT_IDLE + 8 && bt_pend == 0 && bt_rate == 0 && bt_sum() == before_idle);
    /* a build without the sender hook processes nothing either, and a speaker that has just connected is heard at the next block */
    listening = 1;
    {
        void (*installed)(void *, const float *const *, int) = bt_send_orig;
        int was = nested;
        bt_send_orig = NULL; nested = 0;
        block(4, 450);
        assert(bt_pend == 0 && bt_rate == 0);
        bt_send_orig = installed; nested = was;
    }
    block(4, 450);
    assert(bt_pend > 0 && stock_calls == BT_IDLE + 8);
    /* the speaker goes away: after BT_IDLE callbacks without the encoder it is Engine's feed again, and what waited is gone */
    listening = 0;
    for (int i = 0; i < BT_IDLE; i++) block(4, 450);
    stock_calls = 0;
    block(4, 450);
    assert(stock_calls == 1 && bt_pend == 0);

    /* An output pair of the device, as it is (render and stream rate equal): the default is the first pair,
     * not off. Players are 4 channels, a PRIME 4 has 10. A copy comes two frames late, and the sender's call of the
     * next callback takes it. */
    for (int channels = 4; channels <= 10; channels += 6)
        for (int pair = 0; pair < channels / 2; pair++) {
            reset(48000, 48000);
            bt_out_pair = pair;
            for (int i = 0; i < 2; i++) block(channels, 480);
            drain();
            assert(stock_calls == 0 && got_n == 960 && got[0][959] == 0.1f * (float)(pair + 1) && got[1][959] == -0.1f * (float)(pair + 1));
        }

    /* A pair the output does not have is the first pair (lim_poll never stores a negative one). */
    reset(48000, 48000);
    bt_out_pair = 2;
    for (int i = 0; i < 2; i++) block(4, 480);
    drain();
    assert(stock_calls == 0 && got_n == 960 && got[0][959] == 0.1f && got[1][959] == -0.1f);

    /* A stream whose rate libsbc does not have is no speaker: Engine's own feed goes through. */
    reset(48000, 48000);
    set_stream(0);
    for (int i = 0; i < BT_IDLE + 8; i++) block(4, 450);
    assert(stock_calls == BT_IDLE + 8 && bt_pend == 0 && bt_rate == 0);

    /* The longest block at the highest ratio (44.1 kHz render to a 48 kHz stream) fits and leaves the position in range. */
    reset(44100, 48000);
    block(4, BT_BLOCK);
    assert(bt_ip >= -2 && bt_pend > BT_BLOCK && bt_pend < BT_OUT);

    /* A call without outputs (a player makes them too) between the kept block and the sender changes nothing: the block
     * goes once, whoever's sender call takes it. */
    reset(48000, 48000);
    bt_out_pair = 1;
    block(4, 480);
    unsigned kept_rate = bt_rate;
    block0(480);
    assert(bt_rate == kept_rate && bt_rate != 0); /* nothing restarts */
    block(4, 480);
    block0(480);
    drain();
    assert(got_n == 960 && got[0][959] == 0.2f && got[1][959] == -0.2f && stock_calls == 0);

    /* The sender wraps what is past full scale: whatever the pair carries is clipped to +-1. */
    reset(48000, 48000);
    level = 20.0f;
    for (int i = 0; i < 2; i++) block(4, 480);
    drain();
    assert(got[0][959] == 1.0f && got[1][959] == -1.0f);

    /* The sender takes 512 frames a call: a long block arrives in pieces, whole. */
    reset(48000, 48000);
    block(4, 4000);
    drain();
    assert(got_n == 4000 && longest_call == 512);

    /* The stream rate is not the render rate. Every pair of rates: the tone keeps its pitch and level on its own channel
     * (L and R carry different pitches), and the stream has render frames * stream rate / render rate frames. 1 kHz at
     * 96 kHz / 44.1 kHz is the Gen-1 case. */
    static const unsigned rates[][2] = {{96000, 44100}, {96000, 48000}, {48000, 44100}, {44100, 48000}, {48000, 48000}, {88200, 44100}};
    for (unsigned r = 0; r < sizeof rates / sizeof *rates; r++) {
        unsigned render = rates[r][0], stream = rates[r][1];
        int total = 450 * 40;
        reset(render, stream);
        tone = 1000; tone_r = 3000;
        for (int done = 0; done < total; done += 450) block(4, 450);
        drain();
        int n = got_n;
        assert(abs(n - (int)((long long)total * stream / render)) <= 2);
        assert(fabs(amplitude(got[0], n, 1000, stream) - 0.5) < 0.005 && amplitude(got[0], n, 3000, stream) < 0.005);
        assert(fabs(amplitude(got[1], n, 3000, stream) - 0.5) < 0.005 && amplitude(got[1], n, 1000, stream) < 0.005);
        n = run(render, stream, 10000, total, 450);
        assert(amplitude(got[0], n, 10000, stream) > 0.45); /* top of the band keeps its level to a dB */
    }

    /* The result does not depend on how the render was cut into blocks (odd lengths too). */
    static float whole[16384];
    int one = run(96000, 44100, 1000, 12000, 4000), cuts_total = 0;
    memcpy(whole, got[0], (size_t)one * sizeof(float));
    static const int cuts[] = {450, 451, 449, 1, 2, 3, 450, 777, 4000, 4000, 1};
    reset(96000, 44100);
    tone = 1000;
    for (unsigned i = 0; i < sizeof cuts / sizeof *cuts; i++) { block(4, cuts[i]); cuts_total += cuts[i]; }
    drain();
    assert(got_n > 0 && abs(got_n - (int)((long long)cuts_total * 44100 / 96000)) <= 2);
    for (int i = 20; i < got_n - 20 && i < one - 20; i++) assert(fabs(got[0][i] - whole[i]) < 1e-5);
    tone = 0;

    /* A 40 kHz tone folds to 8 kHz when every second sample is kept and to 4.1 kHz when a 44.1 kHz stream takes samples
     * of the render as they come; the half-band leaves nothing of it, wherever it would fold. */
    int n = run(96000, 44100, 40000, 450 * 40, 450);
    double energy = 0;
    for (int i = n - 2000; i < n; i++) energy += (double)got[0][i] * got[0][i];
    assert(sqrt(energy / 2000) < 0.001);

    /* A 44.1 kHz stream cannot carry what is above 22.05 kHz: it folds to 44.1 kHz minus its frequency, up in the
     * ultrasound at 21.1 kHz for a tone at 23 kHz. The low-pass leaves of it under 0.1 % (it was 27 % from a 48 kHz
     * render and 15 % from 96 kHz), and the band below it keeps its level. */
    static const struct { float hz, limit; } sweep[] = {{22300, 0.02f}, {22600, 0.007f}, {23000, 0.001f}, {23500, 0.001f}, {23900, 0.001f}};
    for (unsigned s = 0; s < sizeof sweep / sizeof *sweep; s++)
        for (unsigned render = 48000; render <= 96000; render *= 2) {
            n = run(render, 44100, sweep[s].hz, 450 * 40, 450);
            assert(amplitude(got[0], n, 44100.0 - (double)sweep[s].hz, 44100) < (double)sweep[s].limit);
        }
    for (unsigned render = 48000; render <= 96000; render *= 2) {
        n = run(render, 44100, 16000, 450 * 40, 450);
        assert(amplitude(got[0], n, 16000, 44100) > 0.35); /* the low-pass leaves the band alone */
    }

    /* Nobody listens, then somebody does: the stream comes in from silence, not from the first sample's level. */
    reset(96000, 44100);
    for (int i = 0; i < 4; i++) block(4, 450);
    drain();
    assert(got[0][0] == 0.0f && max_step(got[0], 0, got_n) < 0.1f / BT_FADE * 1.5f);

    /* The callback's rate is measured on the wall clock from its own frames, whatever the device says: every rate Engine uses,
     * in 225 and 450 frame blocks, with the clock reading a little off each time. It settles within two windows (1 s),
     * from the 48 kHz it starts at, and a stream second is then 44100 frames. */
    for (unsigned r = 0; r < 4; r++)
        for (int blk = 225; blk <= 450; blk += 225) {
            static const unsigned real[] = {44100, 48000, 88200, 96000};
            reset(real[r], 44100);
            bt_render = 0;
            sim_noise = 300e3;
            tone = 1000;
            collect = 0;
            int settle = 0, calls = (int)(real[r] / (unsigned)blk) * 3;
            for (int i = 0; i < calls; i++) {
                block(4, blk);
                if (!settle && bt_render) settle = i;
            }
            drain();
            assert(bt_render == real[r] && settle * blk * 1e9 / real[r] < 1.2e9);
            long long before = delivered;
            for (int i = 0; i < (int)(real[r] / (unsigned)blk); i++) block(4, blk);
            drain();
            tone = 0;
            assert(llabs((delivered - before) - 44100) < 441);
        }
    /* A callback that stalls, once, in a window (44 ms: 48 kHz reads as 44.1 kHz) changes nothing. */
    reset(48000, 44100);
    bt_render = 0;
    collect = 0;
    for (int i = 0; i < 213 * 6; i++) {
        if (i == 213 * 2 + 107) sim_stall = 44e6;
        block(4, 225);
        if (i > 213 * 2) assert(bt_render == 48000);
    }
    /* A stalled window, a window that fits no rate, and another stalled window are not "twice in a row" either. */
    static const double stalls[3] = {44e6, 400e6, 44e6};
    for (int w = 0; w < 3; w++) {
        while (bt_m_ns) block(4, 225); /* to the end of the window under way */
        for (int i = 0; i < 20; i++) block(4, 225);
        sim_stall = stalls[w];
        block(4, 225);
        while (bt_m_ns) block(4, 225);
        assert(bt_render == 48000);
    }
    /* The rate is found whether or not a speaker listens (the first one is then heard from its first block), nothing is kept
     * for nobody, and until it is known a speaker that does listen gets nothing, not a guess. */
    reset(96000, 44100);
    bt_render = 0;
    listening = 0;
    for (int i = 0; i < 213 * 3; i++) block(4, 450);
    assert(bt_render == 96000 && bt_pend == 0 && bt_rate == 0);
    reset(96000, 44100);
    bt_render = 0;
    block(4, 450);
    assert(bt_pend == 0 && bt_rate == 0 && stock_calls == 0);
    drain();
    assert(got_n == 0);

    /* regressions, above */
    test_deck();
    test_clip();
    test_blocks();
    test_rate();
    test_source();

    puts("bt out: PASS");
    return 0;
#endif
}
