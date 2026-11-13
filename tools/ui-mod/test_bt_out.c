/* Bluetooth output: hook_master_output keeps the pair bt_out= selects, and hook_bt_send hands exactly that pair
 * to Engine's BluetoothSignalSender::audio (the stock call otherwise carries silence).
 * arm-linux-gnueabihf-gcc -O2 -mthumb -march=armv7-a -mfpu=neon -mfloat-abi=hard tools/ui-mod/test_bt_out.c -ldl -lpthread -lm -o /tmp/test_bt_out
 * Runs natively on the armhf build host; no Engine or rootfs needed. */
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>

/* Pair p of the device carries p+1 tenths on L and -(p+1) tenths on R; the 4-deck pad (8 channels) the same. */
static float level = 1.0f;
static uint32_t device(void *host, void *context, int ic, const float *const *in, int oc, float *const *out, int frames) {
    (void)host; (void)context; (void)ic; (void)in;
    for (int c = 0; c < oc; c++)
        for (int i = 0; i < frames; i++) out[c][i] = (c % 2 ? -0.1f : 0.1f) * (float)(c / 2 + 1) * level;
    return (uint32_t)oc;
}

static float seen_l, seen_r;
static int seen_frames;
static const float *seen_in;
static void sender(void *self, const float *const *in, int frames) {
    (void)self;
    seen_in = (const float *)in;
    seen_l = in[0][frames - 1];
    seen_r = in[1][frames - 1];
    seen_frames = frames;
}

static void feed(const float *const *engine_feed, int frames) {
    seen_in = NULL;
    hook_bt_send(NULL, engine_feed, frames);
}

int main(void) {
    dsp_originals[DSP_MASTER] = (void *)device;
    bt_send_orig = sender;
    static float b[8][480], silence[2][480];
    float *out[4] = {b[0], b[1], b[2], b[3]};
    const float *engine_feed[2] = {silence[0], silence[1]};

    /* Off (default): Engine's own feed goes through untouched. */
    assert(hook_master_output(NULL, NULL, 0, NULL, 4, out, 480) == 4);
    feed(engine_feed, 480);
    assert(seen_in == (const float *)engine_feed);

    /* An output pair of the device. */
    for (int pair = 0; pair < 2; pair++) {
        bt_out_pair = pair;
        assert(hook_master_output(NULL, NULL, 0, NULL, 4, out, 480) == 4);
        feed(engine_feed, 480);
        assert(seen_frames == 480 && seen_l == 0.1f * (float)(pair + 1) && seen_r == -0.1f * (float)(pair + 1));
    }

    /* A call without outputs (a player makes them too) between the kept block and the sender changes nothing. */
    bt_out_pair = 1;
    assert(hook_master_output(NULL, NULL, 0, NULL, 4, out, 480) == 4);
    assert(hook_master_output(NULL, NULL, 0, NULL, 0, NULL, 480) == 0);
    feed(engine_feed, 480);
    assert(seen_l == 0.2f && seen_r == -0.2f);

    /* The sender wraps what is past full scale: the kept pair is clipped to +-1. */
    level = 20.0f;
    bt_out_pair = 1;
    assert(hook_master_output(NULL, NULL, 0, NULL, 4, out, 480) == 4);
    feed(engine_feed, 480);
    assert(seen_l == 1.0f && seen_r == -1.0f);
    level = 1.0f;

    /* A block the callback has not produced (other length) is never replaced by stale audio. */
    feed(engine_feed, 256);
    assert(seen_in == (const float *)engine_feed);

    /* 4-deck layout: the device has two pairs but Engine renders four decks; pair n is deck n+1. */
    four_deck_pad_on = 1;
    bt_out_pair = 3;
    assert(hook_master_output(NULL, NULL, 0, NULL, 4, out, 480) == 8);
    feed(engine_feed, 480);
    assert(seen_l == 0.4f && seen_r == -0.4f);
    four_deck_pad_on = 0;

    /* A pair the device does not have, or the setting removed: back to Engine's feed, never a repeated old block. */
    for (int pair = 2; pair >= -1; pair -= 3) {
        bt_out_pair = pair;
        assert(hook_master_output(NULL, NULL, 0, NULL, 4, out, 480) == 4);
        feed(engine_feed, 480);
        assert(seen_in == (const float *)engine_feed);
    }

    puts("bt out: PASS");
    return 0;
}
