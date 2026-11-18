#define ENGINE_UI_HOOK_TEST
#define SHADOW_POSITION_FEED_PATH "/tmp/enginex-shadow-position-test"
#include "engine_ui_hook.c"
#include <assert.h>
#include <sys/stat.h>

int hook_test_engine;

static int stock_position(const void *self) {
    (void)self;
    return 77;
}

struct fake_planck {
    unsigned char pad[0x244];
    uintptr_t *items;
    uintptr_t *end;
};

static uint64_t test_now_ns(void) {
    struct timespec ts;
    assert(!clock_gettime(CLOCK_MONOTONIC_RAW, &ts));
    return (uint64_t)ts.tv_sec * UINT64_C(1000000000) + (uint64_t)ts.tv_nsec;
}

static void reset_feed(void) {
    unlink(SHADOW_POSITION_FEED_PATH);
    shadow_feed = mmap(NULL, sizeof(struct shadow_position_feed_record) * 4, PROT_READ | PROT_WRITE,
                       MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(shadow_feed != MAP_FAILED);
    memset(shadow_feed, 0, sizeof(struct shadow_position_feed_record) * 4);
    memset(shadow_item_hash, 0, sizeof shadow_item_hash);
    shadow_planck_owner = 0;
    shadow_map_seq = 0;
    shadow_position_original = stock_position;
    for (size_t i = 0; i < sizeof engine_builds / sizeof *engine_builds; i++)
        if (!strcmp(engine_builds[i].name, "5.3.0")) build = &engine_builds[i];
}

static void publish(unsigned deck, uint32_t seq, uint32_t active, double pos, double rate, uint64_t age_ns) {
    struct shadow_position_feed_record *r = &shadow_feed[deck - 1];
    r->active = active;
    r->deck = deck;
    r->position_samples = pos;
    r->rate_samples_per_sec = rate;
    r->mono_ns = test_now_ns() - age_ns;
    __atomic_store_n(&r->seq, seq, __ATOMIC_RELEASE);
}

static void test_fresh_feed_and_mapping(void) {
    reset_feed();
    struct fake_planck planck;
    uintptr_t items[4];
    memset(&planck, 0, sizeof planck);
    memset(items, 0, sizeof items);
    void *deck_a = (void *)0x101, *deck_b = (void *)0x202;
    planck.items = items;
    items[0] = (uintptr_t)deck_a;
    items[1] = (uintptr_t)deck_b;
    shadow_capture_waveform_item(&planck, 0);
    publish(1, 2, 1, 1000.0, 0.0, 0);
    assert(shadow_position_hook(deck_a) == 1000);
    assert(shadow_position_hook(deck_a) == 1000);
    assert(shadow_position_hook(deck_b) == 77); // deck 2 has no active feed
    puts("fresh feed, stable object mapping and stock fallback: PASS");
}

static void test_inactive_stale_and_seqlock_fallback(void) {
    reset_feed();
    void *deck_a = (void *)0x303;
    struct fake_planck planck;
    uintptr_t items[4];
    memset(&planck, 0, sizeof planck);
    memset(items, 0, sizeof items);
    planck.items = items;
    items[0] = (uintptr_t)deck_a;
    shadow_capture_waveform_item(&planck, 0);
    publish(1, 2, 0, 1234.0, 0.0, 0);
    assert(shadow_position_hook(deck_a) == 77);
    publish(1, 4, 1, 1234.0, 0.0, UINT64_C(800000000));
    assert(shadow_position_hook(deck_a) == 77);
    publish(1, 5, 1, 1234.0, 0.0, 0); // odd seqlock: writer in progress
    assert(shadow_position_hook(deck_a) == 77);
    puts("inactive, stale and torn seqlock records fall through: PASS");
}

static void test_verified_mapping_recreation_and_order_swap(void) {
    reset_feed();
    struct fake_planck planck;
    uintptr_t items[4];
    memset(&planck, 0, sizeof planck);
    memset(items, 0, sizeof items);
    planck.items = items;
    items[0] = (uintptr_t)0x111;
    items[1] = (uintptr_t)0x222;
    publish(2, 2, 1, 3000.0, 0.0, 0);
    shadow_capture_waveform_item(&planck, 1);
    assert(shadow_position_hook((void *)0x111) == 77); // local deck never consumes deck 2
    assert(shadow_position_hook((void *)0x222) == 3000);

    // A layout switch recreates both items and changes their order. The live
    // array is authoritative on every lookup, so the old shadow pointer falls
    // through and the new pointer is the only one that follows the feed.
    items[0] = (uintptr_t)0x444;
    items[1] = (uintptr_t)0x333;
    assert(shadow_position_hook((void *)0x222) == 77);
    assert(shadow_position_hook((void *)0x333) == 3000);
    items[2] = (uintptr_t)0x333; // duplicate mapping is ambiguous
    assert(shadow_position_hook((void *)0x333) == 77);
    puts("verified live PlanckGUI mapping survives recreation/order swaps: PASS");
}

int main(void) {
    hf_reload();
    test_fresh_feed_and_mapping();
    test_inactive_stale_and_seqlock_fallback();
    test_verified_mapping_recreation_and_order_swap();
    return 0;
}
