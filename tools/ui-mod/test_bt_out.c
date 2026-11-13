/* Bluetooth output: hook_master_output keeps the pair bt_out= selects (brought from the render rate to the A2DP
 * stream rate), and hook_bt_send hands exactly that pair to Engine's BluetoothSignalSender::audio, which otherwise gets silence.
 * arm-linux-gnueabihf-gcc -O2 -mthumb -march=armv7-a -mfpu=neon -mfloat-abi=hard tools/ui-mod/test_bt_out.c -ldl -lpthread -lm -o /tmp/test_bt_out
 * Also native aarch64: gcc -O2 tools/ui-mod/test_bt_out.c -ldl -lpthread -lm. No Engine or rootfs needed. */
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>
#include <math.h>

/* Pair p of the device carries p+1 tenths on L and -(p+1) tenths on R, times level; with tone set, pair 0 is a
 * continuous sine of that many Hz (amplitude 0.5) at device_rate. */
static float level = 1.0f, tone, device_rate;
static unsigned long long played;
static uint32_t device(void *host, void *context, int ic, const float *const *in, int oc, float *const *out, int frames) {
    (void)host; (void)context; (void)ic; (void)in;
    for (int c = 0; c < oc; c++)
        for (int i = 0; i < frames; i++)
            out[c][i] = tone && c < 2 ? 0.5f * (float)sin(2 * M_PI * (double)tone * (double)(played + (unsigned long long)i) / device_rate) * (c ? -1.0f : 1.0f)
                                      : (c % 2 ? -0.1f : 0.1f) * (float)(c / 2 + 1) * level;
    played += (unsigned long long)frames;
    return (uint32_t)oc;
}

static float got[2][24576];
static int got_n, seen_frames, longest_call;
static const float *seen_in;
static void sender(void *self, const float *const *in, int frames) {
    (void)self;
    seen_in = (const float *)in;
    seen_frames = frames;
    if (frames > longest_call) longest_call = frames;
    for (int c = 0; c < 2; c++) memcpy(got[c] + got_n, in[c], (size_t)frames * sizeof(float));
    got_n += frames;
}

static float silence[2][4096], b[10][4096];
static const float *engine_feed[2] = {silence[0], silence[1]};
static float *out[10];

/* One output callback of `frames`, then the sender call Engine makes inside the next one. */
static void block(int channels, int frames) {
    assert(hook_master_output(NULL, NULL, 0, NULL, channels, out, frames) == (uint32_t)channels);
    seen_in = NULL;
    hook_bt_send(NULL, engine_feed, frames);
}

/* A render at render_hz sent to a stream at stream_hz (0: none yet), from a clean start. */
static void reset(unsigned render_hz, unsigned stream_hz) {
    unsigned long sbc[2] = {0, 0};
    got_n = 0; played = 0; longest_call = 0;
    bt_phase = 0; bt_ip = -2; bt_rem = 0;
    memset(bt_x, 0, sizeof bt_x); memset(bt_y, 0, sizeof bt_y);
    render_rate[4] = render_rate[10] = render_hz;
    device_rate = (float)render_hz;
    stream_rate = 0;
    if (stream_hz) {
        ((uint8_t *)sbc)[sizeof(unsigned long)] = stream_hz == 48000 ? 3 : 2;
        bt_stream_rate(sbc);
        assert(stream_rate == stream_hz);
    }
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
    tone = 0;
    return got_n;
}

int main(void) {
    for (int c = 0; c < 10; c++) out[c] = b[c];
    dsp_originals[DSP_MASTER] = (void *)device;
    bt_send_orig = sender;

    /* The sample rate of the stream is the encoder's: sbc_t.frequency 2 is 44.1 kHz, 3 is 48 kHz, others are none. */
    {
        unsigned long sbc[2] = {0, 0};
        for (unsigned f = 0; f < 5; f++) {
            ((uint8_t *)sbc)[sizeof(unsigned long)] = (uint8_t)(f == 4 ? 200 : f);
            bt_stream_rate(sbc);
            assert(stream_rate == (f == 2 ? 44100u : f == 3 ? 48000u : f == 0 ? 16000u : f == 1 ? 32000u : 0u));
        }
    }

    /* Off: Engine's own feed goes through untouched. */
    reset(0, 0);
    bt_out_pair = -1;
    block(4, 480);
    assert(seen_in == (const float *)engine_feed && seen_frames == 480);

    /* An output pair of the device, as it is (render and stream rate unknown or equal); players are 4 channels,
     * a PRIME 4 has 10. A copy comes two frames late. */
    for (int channels = 4; channels <= 10; channels += 6)
        for (int pair = 0; pair < channels / 2; pair++) {
            reset(0, 0);
            bt_out_pair = pair;
            block(channels, 480);
            assert(seen_frames == 480 && got_n == 480 && got[0][479] == 0.1f * (float)(pair + 1) && got[1][479] == -0.1f * (float)(pair + 1));
        }

    /* A block the callback has not produced (other length) is never replaced by stale audio. */
    hook_bt_send(NULL, engine_feed, 256);
    assert(seen_in == (const float *)engine_feed);

    /* A call without outputs (a player makes them too) between the kept block and the sender changes nothing. */
    reset(0, 0);
    bt_out_pair = 1;
    assert(hook_master_output(NULL, NULL, 0, NULL, 4, out, 480) == 4);
    assert(hook_master_output(NULL, NULL, 0, NULL, 0, NULL, 480) == 0);
    hook_bt_send(NULL, engine_feed, 480);
    assert(got_n == 480 && got[0][479] == 0.2f && got[1][479] == -0.2f);

    /* The sender wraps what is past full scale: the kept pair is clipped to +-1. */
    reset(0, 0);
    level = 20.0f;
    block(4, 480);
    assert(got[0][479] == 1.0f && got[1][479] == -1.0f);
    level = 1.0f;

    /* A pair the device does not have, or the setting removed: back to Engine's feed, never a repeated old block. */
    for (int pair = 2; pair >= -1; pair -= 3) {
        bt_out_pair = pair;
        block(4, 480);
        assert(seen_in == (const float *)engine_feed);
    }
    bt_out_pair = 0;

    /* The sender takes 512 frames a call: a long block arrives in pieces, whole. */
    reset(48000, 48000);
    block(4, 4000);
    assert(got_n == 4000 && longest_call == 512);

    /* The stream rate is not the render rate. Every pair of rates: the tone keeps its pitch and level, and the stream
     * has render frames * stream rate / render rate frames. 1 kHz at 96 kHz / 44.1 kHz is the Gen-1 case. */
    static const unsigned rates[][2] = {{96000, 44100}, {96000, 48000}, {48000, 44100}, {44100, 48000}, {48000, 48000}, {88200, 44100}};
    for (unsigned r = 0; r < sizeof rates / sizeof *rates; r++) {
        unsigned render = rates[r][0], stream = rates[r][1];
        int total = 450 * 40, n = run(render, stream, 1000, total, 450);
        assert(abs(n - (int)((long long)total * stream / render)) <= 2);
        assert(fabs(amplitude(got[0], n, 1000, stream) - 0.5) < 0.005);
        assert(fabs(amplitude(got[1], n, 1000, stream) - 0.5) < 0.005);
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
    assert(got_n > 0 && abs(got_n - (int)((long long)cuts_total * 44100 / 96000)) <= 2);
    for (int i = 20; i < got_n - 20 && i < one - 20; i++) assert(fabs(got[0][i] - whole[i]) < 1e-5);
    tone = 0;

    /* A 40 kHz tone folds to 8 kHz when every second sample is kept and to 4.1 kHz when a 44.1 kHz stream takes samples
     * of the render as they come; the half-band leaves nothing of it, wherever it would fold. */
    int n = run(96000, 44100, 40000, 450 * 40, 450);
    double energy = 0;
    for (int i = n - 2000; i < n; i++) energy += (double)got[0][i] * got[0][i];
    assert(sqrt(energy / 2000) < 0.001);

    /* A change of stream rate (another speaker) starts the new rate from a clean fraction, with the same pitch. */
    n = run(96000, 44100, 1000, 450 * 20, 450);
    stream_rate = 0;
    bt_stream_rate((unsigned long[2]){0, 3});
    int before = got_n;
    tone = 1000;
    for (int i = 0; i < 20; i++) block(4, 450);
    tone = 0;
    assert(abs((got_n - before) - 450 * 20 * 48000 / 96000) <= 2);
    assert(fabs(amplitude(got[0], got_n, 1000, 48000) - 0.5) < 0.005);

    puts("bt out: PASS");
    return 0;
}
