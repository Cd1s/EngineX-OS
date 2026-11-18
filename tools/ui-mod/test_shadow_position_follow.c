// The paused shadow deck's waveform line (HF_SHADOW_PAUSED_FOLLOW): the reader of
// stream-sync's position feed that replaces WaveFormPositionItem::currentPosition.
// arm-linux-gnueabihf-gcc -O1 -g -Wall -Wextra -Werror -fsanitize=address,undefined -fno-sanitize-recover=undefined -static-libasan -static-libubsan -o test_shadow_position_follow test_shadow_position_follow.c -ldl -lpthread -lm
// ./test_shadow_position_follow   (native armhf on the build host)
#define ENGINE_UI_HOOK_TEST
#define FEED_DIR "/tmp/enginex-shadow-position-test"
#define SHADOW_POSITION_FEED_PATH FEED_DIR "/shadow-position"
#include "engine_ui_hook.c"
#include <assert.h>
#include <stddef.h>
#include <sys/stat.h>

int hook_test_engine;

// The layout pkg/webui/shadow_position_feed.go writes.
_Static_assert(offsetof(struct shadow_position_feed_record, active) == 4, "active");
_Static_assert(offsetof(struct shadow_position_feed_record, deck) == 8, "deck");
_Static_assert(offsetof(struct shadow_position_feed_record, track_hash) == 16, "key");
_Static_assert(offsetof(struct shadow_position_feed_record, position_samples) == 24, "position");
_Static_assert(offsetof(struct shadow_position_feed_record, rate_samples_per_sec) == 32, "rate");
_Static_assert(offsetof(struct shadow_position_feed_record, mono_ns) == 40, "time");
_Static_assert(offsetof(struct shadow_position_feed_record, loop_in_samples) == 48, "loop in");
_Static_assert(offsetof(struct shadow_position_feed_record, loop_out_samples) == 56, "loop out");

#define STOCK 77
#define MS UINT64_C(1000000)

static int stock_position(const void *self) {
    (void)self;
    return STOCK;
}

// PlanckGUI as far as the hook reads it: the vector of the decks' items at +0x244.
static struct {
    unsigned char pad[0x244];
    uintptr_t *begin, *end;
} planck;
static uintptr_t items[4];
static struct shadow_position_feed_record feed[4];

static void reset(unsigned decks) {
    memset(feed, 0, sizeof feed);
    memset(items, 0, sizeof items);
    shadow_feed = feed;
    shadow_position_original = stock_position;
    planck.begin = items;
    planck.end = items + decks;
    shadow_planck_owner = 0;
    shadow_capture_waveform_item(&planck);
    build = NULL;
    for (size_t i = 0; i < sizeof engine_builds / sizeof *engine_builds; i++)
        if (!strcmp(engine_builds[i].name, "5.3.0")) build = &engine_builds[i];
    assert(build && build->shadow_array_offset == offsetof(__typeof__(planck), begin));
}

static struct shadow_position_feed_record *publish(unsigned deck, double position, double rate, uint64_t age_ns) {
    struct shadow_position_feed_record *r = &feed[deck - 1];
    r->seq |= 1;
    r->active = 1;
    r->deck = deck;
    r->position_samples = position;
    r->rate_samples_per_sec = rate;
    r->mono_ns = shadow_now_ns() - age_ns;
    r->loop_in_samples = r->loop_out_samples = 0;
    r->seq++;
    return r;
}

// The line is read a moment after the record was dated: it lies in [at, at + 500 ms of rate].
static void assert_line(void *item, double at, double rate) {
    int got = shadow_position_hook(item), slack = (int)(rate < 0 ? -rate : rate) / 2 + 1;
    int low = rate < 0 ? (int)at - slack : (int)at, high = rate < 0 ? (int)at : (int)at + slack;
    if (got < low || got > high) {
        fprintf(stderr, "line %d, want %d..%d\n", got, low, high);
        abort();
    }
}

static void test_follows_its_own_deck_only(void) {
    reset(2);
    void *a = (void *)0x101, *b = (void *)0x202;
    items[0] = (uintptr_t)a;
    items[1] = (uintptr_t)b;
    assert(shadow_position_hook(a) == STOCK && shadow_position_hook(b) == STOCK); // nothing published
    publish(2, 1000.0, 0.0, 0);
    assert(shadow_position_hook(b) == 1000);
    assert(shadow_position_hook(a) == STOCK); // the local deck keeps its own line
    assert(shadow_position_hook((void *)0x303) == STOCK); // no deck's item
    publish(2, -500.25, 0.0, 0);
    assert(shadow_position_hook(b) == -500); // a cue in the lead-in
    puts("a deck's item follows its own record, every other item keeps Engine's line: PASS");
}

static void test_carries_the_line_at_the_rate(void) {
    reset(2);
    void *b = (void *)0x202;
    items[1] = (uintptr_t)b;
    publish(2, 100000.0, 44100.0, 200 * MS);
    assert_line(b, 100000.0 + 8820.0, 44100.0);
    publish(2, 100000.0, -88200.0, 100 * MS); // a scratch backwards
    assert_line(b, 100000.0 - 8820.0, -88200.0);
    publish(2, 1000.0, 3e12, 1000 * MS);
    assert(shadow_position_hook(b) == INT32_MAX);
    publish(2, 1000.0, -3e12, 1000 * MS);
    assert(shadow_position_hook(b) == INT32_MIN);
    puts("the line runs on at the record's rate, forwards and backwards, inside int: PASS");
}

static void test_age(void) {
    reset(2);
    void *b = (void *)0x202;
    items[1] = (uintptr_t)b;
    // A stream-sync that is busy for seconds: the line neither stops nor jumps back.
    publish(2, 5000.0, 0.0, 4800 * MS);
    assert(shadow_position_hook(b) == 5000);
    publish(2, 5000.0, 44100.0, 4000 * MS);
    assert_line(b, 5000.0 + 4 * 44100.0, 44100.0);
    // A writer that is gone: the deck's own line, standing or not.
    publish(2, 5000.0, 0.0, 5200 * MS);
    assert(shadow_position_hook(b) == STOCK);
    publish(2, 5000.0, 44100.0, 5200 * MS);
    assert(shadow_position_hook(b) == STOCK);
    publish(2, 5000.0, 0.0, 0)->mono_ns = shadow_now_ns() + 50 * MS; // dated a little ahead: now
    assert(shadow_position_hook(b) == 5000);
    puts("a record is followed for five seconds, then the deck's own line returns: PASS");
}

static void test_loop(void) {
    reset(2);
    void *b = (void *)0x202;
    items[1] = (uintptr_t)b;
    struct shadow_position_feed_record *r = publish(2, 19000.0, 10000.0, 350 * MS);
    r->seq |= 1;
    r->loop_in_samples = 10000.0;
    r->loop_out_samples = 20000.0;
    r->seq++;
    assert_line(b, 12500.0, 10000.0); // 22500 without the loop
    r->seq |= 1;
    r->loop_in_samples = 20000.0; // out before in: no loop
    r->loop_out_samples = 10000.0;
    r->mono_ns = shadow_now_ns() - 350 * MS;
    r->seq++;
    assert_line(b, 22500.0, 10000.0);
    puts("between two records the line wraps at the source's loop out: PASS");
}

static void test_unusable_records(void) {
    reset(2);
    void *b = (void *)0x202;
    items[1] = (uintptr_t)b;
    struct shadow_position_feed_record *r = publish(2, 1234.0, 0.0, 0);
    assert(shadow_position_hook(b) == 1234);
    r->active = 0;
    assert(shadow_position_hook(b) == STOCK);
    r->active = 1;
    r->seq |= 1; // being written
    assert(shadow_position_hook(b) == STOCK);
    r->seq++;
    r->deck = 1; // another deck's record in this slot
    assert(shadow_position_hook(b) == STOCK);
    r->deck = 2;
    r->mono_ns = 0;
    assert(shadow_position_hook(b) == STOCK);
    r->mono_ns = shadow_now_ns();
    r->position_samples = NAN;
    assert(shadow_position_hook(b) == STOCK);
    r->position_samples = 1234.0;
    r->rate_samples_per_sec = INFINITY;
    assert(shadow_position_hook(b) == STOCK);
    r->rate_samples_per_sec = 0.0;
    assert(shadow_position_hook(b) == 1234);
    r->seq = 0; // never written
    assert(shadow_position_hook(b) == STOCK);
    puts("an inactive, half-written, foreign, undated or non-finite record leaves Engine's line: PASS");
}

static void test_deck_mapping(void) {
    reset(2);
    items[0] = 0x111;
    items[1] = 0x222;
    publish(2, 3000.0, 0.0, 0);
    assert(shadow_position_hook((void *)0x222) == 3000);
    // A layout switch makes the items again, in another order: the vector decides at each call.
    items[0] = 0x444;
    items[1] = 0x333;
    assert(shadow_position_hook((void *)0x222) == STOCK);
    assert(shadow_position_hook((void *)0x333) == 3000);
    // What lies behind a two-deck vector is not a deck.
    publish(3, 7000.0, 0.0, 0);
    items[2] = 0x555;
    assert(shadow_position_hook((void *)0x555) == STOCK);
    planck.end = items + 3;
    assert(shadow_position_hook((void *)0x555) == 7000);
    items[2] = 0x333; // one item on two decks
    assert(shadow_position_hook((void *)0x333) == STOCK);
    planck.begin = items + 1; // a vector that is being rebuilt: its end before its start
    planck.end = items;
    assert(shadow_position_hook((void *)0x444) == STOCK);
    planck.begin = NULL;
    assert(shadow_position_hook((void *)0x444) == STOCK);
    puts("the deck is found in PlanckGUI's own vector at every call, never guessed: PASS");
}

static void test_nothing_followed_reads_nothing(void) {
    reset(2);
    items[1] = 0x222;
    // No deck is followed: PlanckGUI is not touched, whatever it points at.
    shadow_planck_owner = 8;
    assert(shadow_position_hook((void *)0x222) == STOCK);
    // Followed, but waveformPositionItem has not run yet.
    shadow_planck_owner = 0;
    publish(2, 3000.0, 0.0, 0);
    assert(shadow_position_hook((void *)0x222) == STOCK);
    puts("with no deck followed the hook only calls Engine's function: PASS");
}

// stream-sync writes while the render thread reads: a record is taken whole or not at all.
static int writer_stop;
static void *writer(void *unused) {
    (void)unused;
    struct shadow_position_feed_record *r = &feed[1];
    for (unsigned n = 0; !__atomic_load_n(&writer_stop, __ATOMIC_RELAXED); n++) {
        __atomic_store_n(&r->seq, r->seq | 1, __ATOMIC_RELEASE);
        r->active = 1;
        r->deck = 2;
        r->rate_samples_per_sec = 0.0;
        r->mono_ns = shadow_now_ns();
        r->position_samples = n & 1 ? 30000.0 : 15000.0;
        r->loop_in_samples = n & 1 ? 0.0 : 10000.0;
        r->loop_out_samples = n & 1 ? 0.0 : 20000.0;
        __atomic_store_n(&r->seq, r->seq + 1, __ATOMIC_RELEASE);
        usleep(20); // far busier than stream-sync's 250 ms, with room for a whole read
    }
    return NULL;
}

static void test_reader_against_a_writer(void) {
    reset(2);
    void *b = (void *)0x202;
    items[1] = (uintptr_t)b;
    pthread_t thread;
    assert(!pthread_create(&thread, NULL, writer, NULL));
    unsigned whole = 0;
    for (unsigned i = 0; i < 300000; i++) {
        int got = shadow_position_hook(b);
        // 30000 with the other record's loop would read 10000.
        assert(got == 30000 || got == 15000 || got == STOCK);
        whole += got != STOCK;
    }
    __atomic_store_n(&writer_stop, 1, __ATOMIC_RELAXED);
    pthread_join(thread, NULL);
    assert(whole > 0);
    puts("a record is read whole or not at all while it is rewritten: PASS");
}

static void test_install_refuses_unverified_engines(void) {
    hf_reload();
    // Bytes that are not Engine's: nothing is patched and the feature is reported off.
    static uint32_t image[0x1000];
    for (size_t i = 0; i < sizeof engine_builds / sizeof *engine_builds; i++) {
        build = &engine_builds[i];
        shadow_feed = NULL;
        if (build->shadow_capture_site) continue; // a verified site needs Engine's own image
        assert(!install_shadow_position_hooks((uintptr_t)image, 4096) && !shadow_feed);
    }
    for (size_t i = 0; i < sizeof engine_builds / sizeof *engine_builds; i++)
        if (!strcmp(engine_builds[i].name, "5.3.0")) build = &engine_builds[i];
    assert(build->shadow_position_fn == 0x4b9090 && build->shadow_capture_site == 0xa1746c);
    puts("an Engine without a verified site keeps its own function: PASS");
}

// The install on an image that holds Engine 5.3.0's bytes at the sites, on the first
// Engine start after a boot (no /run/enginex yet), and the feed as stream-sync writes it:
// through the file.
static void test_install(void) {
    hf_reload();
    reset(2);
    size_t size = 0x2c00000, page = 4096;
    unsigned char *image = mmap(NULL, size, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(image != MAP_FAILED);
    unsigned char *site = image + build->shadow_capture_site;
    memcpy(image + build->shadow_position_fn, build->shadow_position_prefix, build->shadow_position_prefix_size);
    memcpy(site, build->shadow_capture_prefix, sizeof build->shadow_capture_prefix);
    uintptr_t fn = (uintptr_t)image + build->shadow_position_fn;
    uintptr_t *a = (void *)(image + build->shadow_vtable_a + build->shadow_vtable_slot);
    uintptr_t *b = (void *)(image + build->shadow_vtable_b + build->shadow_vtable_slot);
    unlink(SHADOW_POSITION_FEED_PATH);
    rmdir(FEED_DIR);
    shadow_feed = NULL;

    *a = fn;
    *b = fn + 4; // one slot holds another function: not the Engine the sites are for
    assert(!install_shadow_position_hooks((uintptr_t)image, page));
    assert(*a == fn && *b == fn + 4 && !shadow_feed && !memcmp(site, build->shadow_capture_prefix, 8));
    assert(access(FEED_DIR, F_OK));

    *b = fn;
    assert(install_shadow_position_hooks((uintptr_t)image, page));
    assert(*a == (uintptr_t)shadow_position_hook && *b == (uintptr_t)shadow_position_hook);
    uint32_t jump[2];
    memcpy(jump, site, sizeof jump);
    assert(jump[0] == 0xe51ff004 && jump[1] == (uintptr_t)shadow_capture_waveform_item_stub);
    assert(shadow_capture_return == (uintptr_t)site + 8);
    struct stat st;
    assert(!stat(SHADOW_POSITION_FEED_PATH, &st) && st.st_size == 256);
    assert(shadow_position_original == (int (*)(const void *))fn);

    // The other process: one whole record for deck 2, written to the file.
    shadow_position_original = stock_position;
    void *item = (void *)0x202;
    items[1] = (uintptr_t)item;
    assert(shadow_position_hook(item) == STOCK);
    struct shadow_position_feed_record r = {.seq = 2, .active = 1, .deck = 2, .position_samples = 4242.0, .mono_ns = shadow_now_ns()};
    int fd = open(SHADOW_POSITION_FEED_PATH, O_RDWR);
    assert(fd >= 0 && pwrite(fd, &r, sizeof r, sizeof r) == (ssize_t)sizeof r);
    assert(shadow_position_hook(item) == 4242);
    r.seq = 4;
    r.active = 0;
    assert(pwrite(fd, &r, sizeof r, sizeof r) == (ssize_t)sizeof r);
    assert(shadow_position_hook(item) == STOCK);
    close(fd);
    unlink(SHADOW_POSITION_FEED_PATH);
    rmdir(FEED_DIR);
    puts("the install verifies both slots, makes the feed file on a fresh boot and reads what is written to it: PASS");
}

int main(void) {
    hf_reload();
    test_follows_its_own_deck_only();
    test_carries_the_line_at_the_rate();
    test_age();
    test_loop();
    test_unusable_records();
    test_deck_mapping();
    test_nothing_followed_reads_nothing();
    test_reader_against_a_writer();
    test_install_refuses_unverified_engines();
    test_install();
    return 0;
}
