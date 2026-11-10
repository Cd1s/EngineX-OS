// AArch64 (AZ04: PRIME 4 G2, SYSTEM ONE) checks for engine_ui_hook.c, native on an arm64 host:
//   gcc -O2 -o test_az04_hook test_az04_hook.c -ldl -lpthread && ./test_az04_hook <AZ04 rootfs>/usr/Engine/Engine
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>

static void test_thread_policy(void) {
    const char *jobs[] = {"J:I:Takingfreez", "J:N:Searching o", "J:I:Collecting", "Taking freeze", "Collecting anal", "Freeze Checker "};
    for (size_t i = 0; i < sizeof jobs / sizeof *jobs; i++) assert(thread_class(jobs[i]) == 0);
    assert(thread_class("Freeze Checker ") == 0);
    assert(thread_class("Analysis Queue ") == 2);
    assert(thread_class("OfflineAnalyzer") == 2);
    assert(thread_class("QQuickPixmapRea") == 1);
    assert(thread_class("AudioOut") == 0);
    assert(thread_class("AudioRenderer2") == 0);
    assert(thread_class("AudioWatcher") == 0);
    // Exercise the naming guard too: a job must retain CPU, I/O and scheduling policy.
    cpu_set_t before, after;
    assert(!sched_getaffinity(0, sizeof before, &before));
    int policy = sched_getscheduler(0), nice = getpriority(PRIO_PROCESS, 0);
    long io = syscall(SYS_ioprio_get, 1, 0);
    hook_test_engine = 1;
    guard_named_thread(pthread_self(), "J:I:Takingfreez");
    hook_test_engine = 0;
    assert(!sched_getaffinity(0, sizeof after, &after) && CPU_EQUAL(&before, &after));
    assert(sched_getscheduler(0) == policy && getpriority(PRIO_PROCESS, 0) == nice);
    assert(syscall(SYS_ioprio_get, 1, 0) == io);
    puts("JobController: transient job names retain scheduling, affinity and I/O priority: PASS");
}

// Qt reads a file at data + a 32-bit offset: the 64-bit hook must place the pack blob there.
static void test_resources(void) {
    char dir[] = "/tmp/hook-qrc-XXXXXX", path[64];
    assert(mkdtemp(dir));
    snprintf(path, sizeof path, "%s/a.qml", dir);
    FILE *f = fopen(path, "w"); assert(f && fputs("Item {}", f) >= 0 && !fclose(f));
    static const uchar names[] = {0, 5, 0, 0, 0, 0, 0, 'a', 0, '.', 0, 'q', 0, 'm', 0, 'l'};
    static const uchar data[] = {0, 0, 0, 4, 's', 't', 'o', 'k'}; // in the image, like Engine's rodata
    for (int round = 0; round < 2; round++) { // the second blob goes after the first
        uchar tree[44] = {0};
        tree[5] = FLAG_DIRECTORY; tree[9] = 1; tree[13] = 1;
        tree[27] = FLAG_COMPRESSED; tree[43] = 1;
        struct walk w = {.tree = tree, .names = names, .data = data, .node_size = 22};
        snprintf(w.pack, sizeof w.pack, "%s", dir);
        apply(&w, 0, "", 0);
        assert(w.replaced == 1 && !w.failed);
        const uchar *e = tree + 22;
        int32_t offset = (int32_t)be32(e + 10);
        assert(offset > 0); // Qt sign-extends with sxtw on aarch64; offset must be positive
        const uchar *blob = (const uchar *)((intptr_t)data + offset); // as Qt does: signed 32-bit sxtw offset
        assert(be32(blob) == 7 && !memcmp(blob + 4, "Item {}", 7));
        assert(!(be16(e + 4) & (FLAG_COMPRESSED | FLAG_ZSTD)));
        assert(((uint64_t)be32(e + 14) << 32 | be32(e + 18)) == content_stamp((const uchar *)"Item {}", 7));
    }
    unlink(path); rmdir(dir);
    puts("qrc (AArch64): pack blob mapped within positive signed 32-bit offset above data: PASS");
}

// A blob placed where something else is mapped must go after that mapping, not 16 MiB steps on.
static void test_collision(void) {
    size_t page = (size_t)sysconf(_SC_PAGESIZE), len = 64 * page;
    uchar *busy = mmap(NULL, len, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(busy != MAP_FAILED);
    assert(skip_mapping((uintptr_t)busy + page, page, page) + page == (uintptr_t)busy + len);
    // a candidate starting inside busy but extending past its end overlaps it too
    assert(skip_mapping((uintptr_t)busy + len - page, 4 * page, page) + page == (uintptr_t)busy + len);
    const uchar *data = busy - 0x20000000; // near_data only does arithmetic on it: first try lands in busy
    uchar *blob = malloc(8); assert(blob);
    memcpy(blob, "\0\0\0\4abcd", 8);
    near_next = 0;
    uchar *p = near_data(blob, data);
    assert(p && p >= busy + len && p < busy + 0x1000000 && !memcmp(p + 4, "abcd", 4));
    munmap(p, page); munmap(busy, len);
    puts("qrc (AArch64): blob placed right after a colliding mapping: PASS");
}

static void decode(const char *image, uint32_t q, uint32_t d, const uchar *tail, size_t n, int key, char *out) {
    uchar b[32];
    memcpy(b, image + q, 16); memcpy(b + 16, image + d, 8); memcpy(b + 24, tail, n);
    for (size_t i = 0; i < 24 + n; i++) out[i] = (char)(b[i] ^ (uchar)(key + i));
    out[24 + n] = 0;
}

static void test_engine(const char *filename) {
    FILE *f = fopen(filename, "rb"); assert(f);
    assert(!fseek(f, 0, SEEK_END)); long size = ftell(f); assert(size > 0);
    rewind(f); char *image = malloc((size_t)size); assert(image);
    assert(fread(image, 1, (size_t)size, f) == (size_t)size); fclose(f);
    ElfW(Ehdr) *header = (void *)image;
    struct dl_phdr_info info = {.dlpi_addr = (uintptr_t)image,
        .dlpi_phdr = (void *)(image + header->e_phoff), .dlpi_phnum = header->e_phnum};
    uintptr_t found = 0;
    engine_base(&info, sizeof info, &found);
    assert(found == (uintptr_t)image && !strcmp(build->name, "5.3.0-az04"));
    char *at = memmem(image, (size_t)size, build->id, 20); assert(at);
    *at ^= 1; found = 0;
    engine_base(&info, sizeof info, &found);
    assert(found == 0);
    *at ^= 1;
    char url[40];
    decode(image, 0x1dbff30, 0x1dc0ee0, (const uchar *)"\x13", 1, 0x24, url);
    assert(!strcmp(url, "https://api.tidal.com/v1/"));
    // The literal-pool image VAs equal file offsets: the first LOAD maps offset 0 at 0.
    for (size_t i = 0; i < build->edit_count; i++) {
        const struct engine_edit *e = &build->edits[i];
        assert(e->offset + e->size <= (size_t)size);
        assert(edit_state((uintptr_t)image, e) == 1);
        memcpy(image + e->offset, e->replacement, e->size);
        assert(edit_state((uintptr_t)image, e) == 2);
    }
    decode(image, 0x1dbff30, 0x1dc0ee0, (const uchar *)"\x13", 1, 0x24, url);
    assert(!strcmp(url, "http://127.0.0.1:8080/v1/"));
    uint32_t mov; memcpy(&mov, image + 0x4ecde0, 4);
    assert((mov & 0xffe0001f) == 0x52800000); // still mov w0, #imm16
    const uchar suffix[2] = {(uchar)(mov >> 5), (uchar)(mov >> 13)};
    decode(image, 0x1dbff40, 0x1dc0ee8, suffix, 2, 0x57, url);
    assert(!strcmp(url, "http://127.0.0.1:8080/au//"));
    assert(!memcmp(image + 0x1d40f28, "mp3", 4));
    image[build->edits[0].offset] ^= 0xff;
    assert(edit_state((uintptr_t)image, &build->edits[0]) == 0);
    free(image);
    printf("AZ04 Engine %s: BuildID, %u edits stock/patched/mismatched, gateway URLs decode: PASS\n",
           filename, build->edit_count);
}

// install_sqlite_hooks on the real image: mapped page-aligned, the relocated api slots filled in.
static void test_sqlite(const char *filename) {
    FILE *f = fopen(filename, "rb"); assert(f);
    assert(!fseek(f, 0, SEEK_END)); size_t size = (size_t)ftell(f); rewind(f);
    size_t page = (size_t)sysconf(_SC_PAGESIZE), len = (size + page - 1) / page * page;
    uchar *image = mmap(NULL, len, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(image != MAP_FAILED && fread(image, 1, size, f) == size); fclose(f);
    uintptr_t base = (uintptr_t)image;
    uint64_t *api = (uint64_t *)(image + build->sqlite_api);
    api[55] = base + build->sqlite_exec; api[70] = base + build->sqlite_open; api[135] = base + build->sqlite_open_v2;
    uchar entry[16]; memcpy(entry, image + build->sqlite_opendb, 16);
    install_sqlite_hooks(base, page);
    uint32_t *at = (uint32_t *)(image + build->sqlite_opendb), *tramp = (uint32_t *)sqlite_open_v2_original;
    uint64_t to; memcpy(&to, &at[2], 8);
    assert(at[0] == 0x58000050 && at[1] == 0xd61f0200 && to == (uintptr_t)tuned_sqlite_open_v2);
    assert(!memcmp(tramp, entry, 16) && tramp[4] == 0x58000050 && tramp[5] == 0xd61f0200);
    memcpy(&to, &tramp[6], 8);
    assert(to == base + build->sqlite_opendb + 16 && sqlite_exec_original == (sqlite_exec_fn)(base + build->sqlite_exec));
    // a changed prologue must be left alone
    mprotect(image, len, PROT_READ | PROT_WRITE);
    memcpy(image + build->sqlite_opendb, entry, 16); image[build->sqlite_opendb] ^= 1;
    sqlite_open_v2_original = NULL;
    install_sqlite_hooks(base, page);
    assert(!sqlite_open_v2_original);
    munmap(image, len);
    puts("AZ04 SQLite open_v2 hook: trampoline + redirect built, changed prologue refused: PASS");
}

static uchar *map_image(const char *filename, size_t *len_out) {
    FILE *f = fopen(filename, "rb"); assert(f);
    assert(!fseek(f, 0, SEEK_END)); size_t size = (size_t)ftell(f); rewind(f);
    size_t page = (size_t)sysconf(_SC_PAGESIZE), len = (size + page - 1) / page * page;
    uchar *image = mmap(NULL, len, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(image != MAP_FAILED && fread(image, 1, size, f) == size); fclose(f);
    *len_out = len;
    return image;
}

static uint64_t quad(const void *p) { uint64_t v; memcpy(&v, p, 8); return v; }

// install_dsp_hooks on the real image: every active site redirected, every stub continues where the
// displaced prologue ends, the PC-relative ones (AutoGate cbz, master adrp) rewritten, a changed prologue refused.
static void test_dsp(const char *filename) {
    size_t len, page = (size_t)sysconf(_SC_PAGESIZE);
    uchar *image = map_image(filename, &len);
    uintptr_t base = (uintptr_t)image;
    select_build(&engine_builds[0]);
    assert(DSP_HOOK_COUNT == 21 && dsp_hooks[DSP_MASTER].offset == 0xd30e68 && dsp_hooks[DSP_WASH].offset == 0xff08c0);
    uchar *stock = malloc(len); assert(stock);
    memcpy(stock, image, len);
    // a changed prologue in any one site refuses the whole group and patches nothing
    for (unsigned i = 0; i < DSP_HOOK_COUNT; i++) {
        if (!dsp_site_active(i)) continue;
        image[dsp_hooks[i].offset + 4] ^= 1;
        int refused = !install_dsp_hooks(base, page);
        image[dsp_hooks[i].offset + 4] ^= 1;
        assert(refused && !memcmp(image, stock, len));
    }
    assert(install_dsp_hooks(base, page));
    unsigned active = 0;
    for (unsigned i = 0; i < DSP_HOOK_COUNT; i++) {
        const uint32_t *at = (const uint32_t *)(image + dsp_hooks[i].offset);
        if (!dsp_site_active(i)) { // event layouts unverified: stock
            assert(!memcmp(at, stock + dsp_hooks[i].offset, 16) && (i >= DSP_MIX && i <= DSP_MOTOR_RAMP_TIME));
            continue;
        }
        active++;
        assert(at[0] == 0x58000050 && at[1] == 0xd61f0200 && quad(&at[2]) == (uint64_t)(uintptr_t)dsp_hooks[i].hook);
        const uint32_t *orig = dsp_originals[i], *old = (const uint32_t *)(stock + dsp_hooks[i].offset);
        uint64_t back = base + dsp_hooks[i].offset + 16;
        if (i == DSP_AUTOGATE) { // mov x2,x1; cbnz x3,+12; ldr x16,zero; br x16; ldp x7,x6,[x1]; ldr x16,back; br x16
            assert(orig[0] == old[1] && orig[1] == 0xb5000063 && orig[4] == old[3] && orig[3] == 0xd61f0200 && orig[6] == 0xd61f0200);
            assert(quad(&orig[8]) == base + 0xff5cc8 && quad(&orig[10]) == back);
            assert(orig[2] == 0x580000d0 && orig[5] == 0x580000b0 && !(((uintptr_t)&orig[8]) & 7));
        } else if (i == DSP_DUB_ECHO || i == DSP_WASH) { // mov x2,x1; add x0,x0,#imm; ldr x16; br x16; the shared body
            assert(orig[0] == old[1] && orig[1] == old[2] && orig[2] == 0x58000050 && orig[3] == 0xd61f0200);
            assert(quad(&orig[4]) == base + (i == DSP_DUB_ECHO ? 0x17474cc : 0x17478c0));
        } else if (i == DSP_MASTER) { // paciasp; sub sp; ldr x7,got; ldr x7,[x7]; ldr x16; br x16; got; back
            assert(orig[0] == old[0] && orig[1] == old[1] && orig[2] == 0x58000087 && orig[3] == 0xf94000e7);
            assert(orig[4] == 0x58000090 && orig[5] == 0xd61f0200 && quad(&orig[6]) == base + 0x2db8860 && quad(&orig[8]) == back);
        } else {
            assert(!memcmp(orig, old, 16) && orig[4] == 0x58000050 && orig[5] == 0xd61f0200 && quad(&orig[6]) == back);
        }
    }
    assert(active == 14);
    // the GOT slot the master stub loads is the one the stock adrp/ldr pair addressed
    assert(dsp_originals[DSP_MASTER] && quad(&((const uint32_t *)dsp_originals[DSP_MASTER])[6]) == base + 0x2db8860);
    // handler owner: x5 = +24, x3 = +40 (this = owner + (adjustment >> 1))
    uint64_t handler[8] = {0}; handler[3] = 0x1000; handler[5] = 6;
    assert(dsp_bound_owner(handler) == (void *)0x1003);
    munmap(image, len); free(stock);
    puts("AZ04 DSP hooks: 14 sites redirected (7 event guards stay stock), stubs for AutoGate/sweep/master rewritten, changed prologue refused: PASS");
}

// The OfflineAnalyzer .perf replay hook: the build identifier and the entry's prologue.
static void test_offline_analyzer(const char *filename) {
    size_t len;
    uchar *image = map_image(filename, &len);
    assert(!memcmp(image + OA_PROCESS_FN, oa_process_prefix, sizeof oa_process_prefix));
    munmap(image, len);
    puts("AZ04 OfflineAnalyzer: process() prologue matches the .perf replay hook: PASS");
}

// LED Brightness: SysEx RGB / NoteOn are scaled after gamma and replayed when the level changes.
static uchar sent[32][SEQ_EV_SIZE], sent_sx[32][16];
static int nsent;
static int fake_direct(void *h, void *ev) {
    (void)h;
    memcpy(sent[nsent], ev, SEQ_EV_SIZE);
    struct sc_seq_ext x; memcpy(&x, (uchar *)ev + 16, sizeof x);
    if (sent[nsent][0] == SEQ_SYSEX) memcpy(sent_sx[nsent], x.ptr, x.len);
    return nsent++;
}
static void test_led(void) {
    assert(scale_led_pwm(0, 20) == 0 && scale_led_pwm(127, 100) == 127 && scale_led_pwm(127, 20) == 5 && scale_led_pwm(1, 10) == 1);
    uchar msg[15] = {0xF0, 0, 0x02, 0x0B, 0, 0, 0x03, 0, 0x05, 2, 7, 127, 0, 64, 0xF7}, screen[15] = {0xF0, 0, 0x02, 0x0B, 0, 0, 0x7C};
    int ch, idx, rgb;
    assert(led_sysex(msg, 15, &ch, &idx, &rgb) && ch == 2 && idx == 7 && rgb == 11 && !led_sysex(screen, 15, &ch, &idx, &rgb));
    uchar ev[SEQ_EV_SIZE] = {SEQ_SYSEX, 0x04};
    struct sc_seq_ext x = {15, msg};
    memcpy(ev + 16, &x, sizeof x);
    seq_direct_cache = fake_direct;
    hook_test_engine = 1;
    led_pct = 100;
    snd_seq_event_output_direct((void *)1, ev);
    assert(sent_sx[0][11] == 127 && sent_sx[0][13] == 64);
    uchar note[SEQ_EV_SIZE] = {SEQ_NOTEON, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5, 100};
    snd_seq_event_output_direct((void *)1, note);
    assert(sent[1][18] == 100);
    uchar note_hi[SEQ_EV_SIZE] = {SEQ_NOTEON, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x6f, 127};
    snd_seq_event_output_direct((void *)1, note_hi);
    assert(sent[2][18] == 127);
    led_replay(20); // resends all cached LEDs (including notes >= 100), dimmed, from the raw cache
    assert(nsent == 6 && sent_sx[3][11] == 4 && sent_sx[3][12] == 0 && sent_sx[3][13] == 2 && sent[4][18] == 1 && sent[5][18] == 1 && msg[11] == 127);
    // a dim hot cue (gamma'd 30/10/0) and unselected mode key / dim pad (1/2/0) stay visible (never 0) and keep hue; off (0/0/0) stays 0
    uchar out[16], cue[15] = {0xF0, 0, 0x02, 0x0B, 0, 0, 0x03, 0, 0x05, 0, 0, 30, 10, 0, 0xF7};
    uchar dim_pad[15] = {0xF0, 0, 0x02, 0x0B, 0, 0, 0x03, 0, 0x05, 0, 0, 1, 2, 0, 0xF7};
    uchar off_pad[15] = {0xF0, 0, 0x02, 0x0B, 0, 0, 0x03, 0, 0x05, 0, 0, 0, 0, 0, 0xF7};
    led_scaled_sysex(cue, 15, 11, 20, out);
    assert(out[11] == 2 && out[12] == 1 && out[13] == 0);
    led_scaled_sysex(dim_pad, 15, 11, 20, out);
    assert(out[11] == 1 && out[12] == 1 && out[13] == 0);
    led_scaled_sysex(dim_pad, 15, 11, 80, out);
    assert(out[11] == 1 && out[12] == 1 && out[13] == 0);
    led_scaled_sysex(off_pad, 15, 11, 20, out);
    assert(out[11] == 0 && out[12] == 0 && out[13] == 0);
    led_scaled_sysex(cue, 15, 11, 50, out);
    assert(out[11] == 6 && out[12] == 2);
    // monotonic: no brightness step may be brighter than the one above it, and no lit LED may drop to 0
    for (unsigned m = 1; m <= 127; m++) {
        uchar px[15] = {0xF0, 0, 0x02, 0x0B, 0, 0, 0x03, 0, 0x05, 0, 0, (uchar)m, 0, 0, 0xF7};
        unsigned prev = m;
        for (int pct = 100; pct >= 20; pct -= 20) {
            led_scaled_sysex(px, 15, 11, pct, out);
            assert(out[11] >= 1 && out[11] <= prev);
            prev = out[11];
        }
    }
    uchar c16[15] = {0xF0, 0, 0x02, 0x0B, 0, 0, 0x03, 0, 0x05, 0, 0, 16, 0, 0, 0xF7};
    led_scaled_sysex(c16, 15, 11, 80, out); assert(out[11] == 12);
    led_scaled_sysex(c16, 15, 11, 60, out); assert(out[11] == 8);
    led_scaled_sysex(c16, 15, 11, 40, out); assert(out[11] == 5);
    led_scaled_sysex(c16, 15, 11, 20, out); assert(out[11] == 2);
    led_scaled_sysex(cue, 15, 11, 100, out);
    assert(out[11] == 30 && out[12] == 10);
    // single-colour keys (including Vinyl 19): dim state (1) at <= 80% brightness; JP21 6-bit RGB (2..63) scales per 2-bit channel; control notes pass through
    assert(led_scaled_note(5, 100, 20) == 1 && led_scaled_note(5, 100, 80) == 1 && led_scaled_note(5, 100, 100) == 100 && led_scaled_note(5, 1, 20) == 1 && led_scaled_note(5, 0, 20) == 0);
    assert(led_scaled_note(19, 127, 80) == 1 && led_scaled_note(19, 127, 20) == 1 && led_scaled_note(1, 127, 20) == 1 && led_scaled_note(2, 127, 60) == 1);
    assert(led_scaled_note(15, 0x3c, 100) == 0x3c && led_scaled_note(15, 0x3c, 80) == 0x28 && led_scaled_note(15, 0x3c, 20) == 0x14 && led_scaled_note(15, 0x10, 20) == 0x10);
    assert(led_scaled_note(0x38, 3, 20) == 3 && led_scaled_note(0x3d, 127, 20) == 127 && led_scaled_note(117, 127, 20) == 127 && led_scaled_note(127, 127, 20) == 127);
    note[18] = 1; // dim state: passes through and is forgotten
    snd_seq_event_output_direct((void *)1, note);
    note_hi[0] = SEQ_NOTEOFF;
    snd_seq_event_output_direct((void *)1, note_hi);
    led_replay(100);
    assert(nsent == 9 && sent_sx[8][11] == 127);
    hook_test_engine = 0;
    puts("LED brightness: scale after gamma, replay from cache: PASS");
}

int main(int argc, char **argv) {
    assert(sizeof engine_builds / sizeof *engine_builds == 1);
    test_thread_policy();
    test_resources();
    test_collision();
    test_led();
    for (int i = 1; i < argc; i++) { test_engine(argv[i]); test_sqlite(argv[i]); test_dsp(argv[i]); }
    const char *oa = getenv("AZ04_OFFLINE_ANALYZER");
    if (oa) test_offline_analyzer(oa);
    return 0;
}
