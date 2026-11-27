// AArch64 (AZ04: PRIME 4 G2, SYSTEM ONE) checks for engine_ui_hook.c, native on an arm64 host:
//   gcc -O2 -o test_az04_hook test_az04_hook.c -ldl -lpthread && ./test_az04_hook <AZ04 rootfs>/usr/Engine/Engine
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
static char stems_bundle_file[4096], stems_bin_file[4096];
#define STEMS_BUNDLE_PROCESSOR stems_bundle_file
#define STEMS_BUNDLE_BIN_PROCESSOR stems_bin_file
// Never the build host's /data/enginex (BH-GF4-4 looks there first).
static char stems_no_enginex[1];
#define STEMS_ENGINEX_PROCESSOR stems_no_enginex
#define STEMS_ENGINEX_BIN_PROCESSOR stems_no_enginex
#include "engine_ui_hook.c"
#include <assert.h>

static void *named_policy_worker(void *arg) {
    const char *name = arg;
    int before = getpriority(PRIO_PROCESS, 0);
    long io_before = syscall(SYS_ioprio_get, 1, 0);
    assert(!pthread_setname_np(pthread_self(), name));
    int kind = thread_class(name);
    assert(getpriority(PRIO_PROCESS, 0) == (kind == 1 ? before : 10));
    assert(syscall(SYS_ioprio_get, 1, 0) == (kind == 2 ? ((2 << 13) | 7) : io_before));
    cpu_set_t cpus;
    assert(!sched_getaffinity(0, sizeof cpus, &cpus));
    assert(!CPU_ISSET(1, &cpus) && !CPU_ISSET(2, &cpus));
    if (kind == 2) assert(sched_getscheduler(0) == SCHED_OTHER);
    return NULL;
}

static void test_thread_policy(void) {
    const char *jobs[] = {"J:I:Takingfreez", "J:N:Searching o", "J:I:Collecting", "Taking freeze", "Collecting anal", "Freeze Checker "};
    for (size_t i = 0; i < sizeof jobs / sizeof *jobs; i++) assert(thread_class(jobs[i]) == 0);
    assert(thread_class("Freeze Checker ") == 0);
    assert(thread_class("Analysis Queue ") == 2);
    assert(thread_class("OfflineAnalyzer") == 2);
    assert(thread_class("QQuickPixmapRea") == 1);
    assert(thread_class("FileInfoThread") == 3);
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
    const char *workers[] = {"QQuickPixmapRea", "FileInfoThread", "Analysis Queue", "Dropbox"};
    hook_test_engine = 1;
    for (size_t i = 0; i < sizeof workers / sizeof *workers; i++) {
        pthread_t worker;
        assert(!pthread_create(&worker, NULL, named_policy_worker, (void *)workers[i]));
        assert(!pthread_join(worker, NULL));
    }
    hook_test_engine = 0;
    assert(getpriority(PRIO_PROCESS, 0) == nice && syscall(SYS_ioprio_get, 1, 0) == io);
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
        assert(dsp_site_active(i));
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
    assert(active == 21);
    // the GOT slot the master stub loads is the one the stock adrp/ldr pair addressed
    assert(dsp_originals[DSP_MASTER] && quad(&((const uint32_t *)dsp_originals[DSP_MASTER])[6]) == base + 0x2db8860);
    // handler owner: x5 = +24, x3 = +40 (this = owner + (adjustment >> 1))
    uint64_t handler[8] = {0}; handler[3] = 0x1000; handler[5] = 6;
    assert(dsp_bound_owner(handler) == (void *)0x1003);
    munmap(image, len); free(stock);
    puts("AZ04 DSP hooks: 21 sites redirected, stubs for AutoGate/sweep/master rewritten, changed prologue refused: PASS");
}

static unsigned guard_event_calls;
static void *guard_last_handler;
static const void *guard_last_event;
static void count_dsp_event(void *h, const void *e) {
    guard_event_calls++;
    guard_last_handler = h;
    guard_last_event = e;
}

static void test_dsp_event_guards(void) {
    struct {
        unsigned slot;
        void (*hook)(void *, const void *);
        size_t off64, off32;
        int is_double;
    } cases[] = {
        {DSP_MIX, hook_fx_mix, 0x18, 0x0c, 0},
        {DSP_PARAMETER, hook_fx_parameter, 0x30, 0x20, 0},
        {DSP_NUDGE, hook_playback_nudge, 0x10, 0x08, 1},
        {DSP_TEMPO, hook_playback_tempo, 0x10, 0x08, 1},
        {DSP_JUMP, hook_time_stretch_jump, 0x10, 0x08, 1},
        {DSP_RAMP_TIME, hook_ramp_time, 0x10, 0x08, 1},
        {DSP_MOTOR_RAMP_TIME, hook_motor_ramp_time, 0x10, 0x08, 1},
    };
    unsigned char handler[64] = {0};
    for (size_t i = 0; i < sizeof cases / sizeof *cases; i++) {
        unsigned slot = cases[i].slot;
        dsp_originals[slot] = (void *)count_dsp_event;
        unsigned char ev[64];
        memset(ev, 0, sizeof ev);
        guard_event_calls = 0;

        // NULL event must be dropped.
        cases[i].hook(handler, NULL);
        assert(guard_event_calls == 0);

        // Finite 64-bit value passes even when the old 32-bit offset holds NaN.
        if (cases[i].is_double) {
            double ok = 1.25, bad = (double)NAN;
            memcpy(ev + cases[i].off64, &ok, sizeof ok);
            memcpy(ev + cases[i].off32, &bad, sizeof bad);
        } else {
            float ok = 0.5f, bad = (float)NAN;
            memcpy(ev + cases[i].off64, &ok, sizeof ok);
            memcpy(ev + cases[i].off32, &bad, sizeof bad);
        }
        cases[i].hook(handler, ev);
        assert(guard_event_calls == 1 && guard_last_handler == handler && guard_last_event == ev);

        // NaN, +Inf, -Inf at the 64-bit offset must all be rejected.
        double bad64[] = {(double)NAN, (double)INFINITY, -(double)INFINITY};
        for (size_t b = 0; b < 3; b++) {
            memset(ev, 0, sizeof ev);
            if (cases[i].is_double) {
                memcpy(ev + cases[i].off64, &bad64[b], sizeof bad64[b]);
            } else {
                float f = (float)bad64[b];
                memcpy(ev + cases[i].off64, &f, sizeof f);
            }
            cases[i].hook(handler, ev);
            assert(guard_event_calls == 1);
        }

        // Domain-specific rejections / bounds at 64-bit double offset.
        if (slot == DSP_TEMPO) {
            double vals[] = {0.0, -1.0};
            for (size_t k = 0; k < 2; k++) {
                memcpy(ev + cases[i].off64, &vals[k], sizeof vals[k]);
                cases[i].hook(handler, ev);
                assert(guard_event_calls == 1);
            }
        } else if (slot == DSP_RAMP_TIME || slot == DSP_MOTOR_RAMP_TIME) {
            double neg = -0.1, zero = 0.0;
            memcpy(ev + cases[i].off64, &neg, sizeof neg);
            cases[i].hook(handler, ev);
            assert(guard_event_calls == 1);
            memcpy(ev + cases[i].off64, &zero, sizeof zero);
            cases[i].hook(handler, ev);
            assert(guard_event_calls == 2);
        }
    }
    memset(dsp_originals, 0, sizeof dsp_originals);
    puts("AZ04 DSP event guards: all 7 handlers validate verified 64-bit struct offsets (NaN/Inf/bounds rejected): PASS");
}

// The OfflineAnalyzer .perf replay hook: the build identifier and the entry's prologue.
// install_bt_send on the real image: Engine's A2DP sender (BluetoothSignalSender::audio) goes to hook_bt_send, the
// displaced prologue continues after it, and a changed prologue is left alone.
static void test_bt_send(const char *filename) {
    size_t len, page = (size_t)sysconf(_SC_PAGESIZE);
    uchar *image = map_image(filename, &len);
    uintptr_t base = (uintptr_t)image;
    select_build(&engine_builds[0]);
    assert(build->bt_send == 0x102eb20);
    uchar entry[16];
    memcpy(entry, image + build->bt_send, 16);
    image[build->bt_send] ^= 1;
    bt_send_orig = NULL;
    install_bt_send(base, page);
    assert(!bt_send_orig && !memcmp(image + build->bt_send + 1, entry + 1, 15));
    image[build->bt_send] ^= 1;
    install_bt_send(base, page);
    const uint32_t *at = (const uint32_t *)(image + build->bt_send), *tramp = (const uint32_t *)bt_send_orig;
    assert(bt_send_orig && at[0] == 0x58000050 && at[1] == 0xd61f0200 && quad(&at[2]) == (uint64_t)(uintptr_t)hook_bt_send);
    assert(!memcmp(tramp, entry, 16) && tramp[4] == 0x58000050 && tramp[5] == 0xd61f0200 && quad(&tramp[6]) == base + build->bt_send + 16);
    munmap(image, len);
    puts("AZ04 Bluetooth sender hook: redirect + trampoline built, changed prologue refused: PASS");
}

static void test_offline_analyzer(const char *filename) {
    size_t len;
    uchar *image = map_image(filename, &len);
    assert(!memcmp(image + OA_PROCESS_FN, oa_process_prefix, sizeof oa_process_prefix));
    munmap(image, len);
    puts("AZ04 OfflineAnalyzer: process() prologue matches the .perf replay hook: PASS");
}

// Pad LED Brightness: SysEx RGB is scaled after gamma and replayed when the level changes; single-colour NoteOn passes through untouched.
static uchar sent[64][SEQ_EV_SIZE], sent_sx[64][16];
static int nsent;
static pthread_t sent_by[64];
static int fake_direct(void *h, void *ev) {
    (void)h;
    sent_by[nsent] = pthread_self();
    memcpy(sent[nsent], ev, SEQ_EV_SIZE);
    struct sc_seq_ext x; memcpy(&x, (uchar *)ev + 16, sizeof x);
    if (sent[nsent][0] == SEQ_SYSEX) memcpy(sent_sx[nsent], x.ptr, x.len);
    return nsent++;
}
static void test_led(void) {
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
    uchar note[SEQ_EV_SIZE] = {6, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5, 100};
    snd_seq_event_output_direct((void *)1, note);
    assert(sent[1][18] == 100);
    led_replay(20); // only publishes the level: nothing is sent from the settings thread (BH-O55-4)
    assert(nsent == 2);
    // Engine's next event on that handle first resends only the cached RGB SysEx, dimmed; NoteOn is never cached or altered
    snd_seq_event_output_direct((void *)1, note);
    assert(nsent == 4 && sent_sx[2][11] == 4 && sent_sx[2][12] == 0 && sent_sx[2][13] == 2 && msg[11] == 127);
    assert(sent[3][0] == 6 && sent[3][18] == 100);
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
    // monotonic: no brightness step may be brighter than the one above it, no lit LED drops to 0, and active (m>=4) stays >=2 (above dim=1)
    for (unsigned m = 1; m <= 127; m++) {
        uchar px[15] = {0xF0, 0, 0x02, 0x0B, 0, 0, 0x03, 0, 0x05, 0, 0, (uchar)m, 0, 0, 0xF7};
        unsigned prev = m;
        for (int pct = 100; pct >= 20; pct -= 20) {
            led_scaled_sysex(px, 15, 11, pct, out);
            assert(out[11] >= (m >= 4 ? 2u : 1u) && out[11] <= prev);
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
    // 3 states (Off / Available-Inactive Dim / Active-On) x 5 brightness levels (100/80/60/40/20%):
    // single-colour NoteOn velocities (0, 1, 127) pass through verbatim; RGB SysEx keeps Off < Dim < Active.
    static const int steps[5] = {100, 80, 60, 40, 20};
    static const uchar exp16[5] = {16, 12, 8, 5, 2}, exp127[5] = {127, 64, 28, 12, 4};
    uchar raw_dim1[15] = {0xF0, 0, 0x02, 0x0B, 0, 0, 0x03, 0, 0x05, 0, 0, 1, 1, 1, 0xF7};
    uchar raw_act127[15] = {0xF0, 0, 0x02, 0x0B, 0, 0, 0x03, 0, 0x05, 0, 0, 127, 0, 0, 0xF7};
    for (int i = 0; i < 5; i++) {
        int pct = steps[i];
        led_pct = pct;
        uchar vels[3] = {0, 1, 127}, got[3];
        for (int k = 0; k < 3; k++) {
            note[17] = 19;
            note[18] = vels[k];
            int idx_sent = snd_seq_event_output_direct((void *)1, note);
            got[k] = sent[idx_sent][18];
            assert(got[k] == vels[k]);
        }
        assert(got[0] < got[1] && got[1] < got[2]);
        uchar s_off[16], s_dim1[16], s_dim2[16], s_act16[16], s_act127[16];
        led_scaled_sysex(off_pad, 15, 11, pct, s_off);
        led_scaled_sysex(raw_dim1, 15, 11, pct, s_dim1);
        led_scaled_sysex(dim_pad, 15, 11, pct, s_dim2);
        led_scaled_sysex(c16, 15, 11, pct, s_act16);
        led_scaled_sysex(raw_act127, 15, 11, pct, s_act127);
        assert(s_off[11] == 0 && s_dim1[11] == 1 && s_dim2[12] == (pct == 100 ? 2 : 1));
        assert(s_act16[11] == exp16[i] && s_act127[11] == exp127[i]);
        assert(s_off[11] < s_dim1[11] && s_dim2[12] < s_act16[11] && s_act16[11] < s_act127[11]);
    }
    int before = nsent;
    led_replay(100);
    assert(nsent == before);
    snd_seq_event_output_direct((void *)1, note);
    assert(nsent == before + 2 && sent_sx[before][11] == 127);
    hook_test_engine = 0;
    puts("pad LED brightness: scale after gamma, replay from cache: PASS");
}

// BH-O55-4: a snd_seq_t is not thread-safe. A brightness change on the settings poll thread must never
// write to Engine's sequencer handle; the replay goes out on Engine's thread with its next event.
static void *change_brightness(void *arg) { led_replay(*(int *)arg); return NULL; }
static void test_led_replay_on_engine_thread(void) {
    uchar msg[15] = {0xF0, 0, 0x02, 0x0B, 0, 0, 0x03, 0, 0x05, 1, 9, 127, 127, 127, 0xF7};
    uchar ev[SEQ_EV_SIZE] = {SEQ_SYSEX, 0x04}, note[SEQ_EV_SIZE] = {6};
    struct sc_seq_ext x = {15, msg};
    memcpy(ev + 16, &x, sizeof x);
    seq_direct_cache = fake_direct;
    hook_test_engine = 1;
    nsent = 0;
    snd_seq_event_output_direct((void *)2, ev);
    int pct = 40;
    pthread_t poll;
    assert(!pthread_create(&poll, NULL, change_brightness, &pct) && !pthread_join(poll, NULL));
    assert(nsent == 1); // nothing sent from the poll thread
    snd_seq_event_output_direct((void *)2, note);
    assert(nsent == 3 && sent_sx[1][11] == 12 && sent[2][0] == 6);
    for (int i = 0; i < nsent; i++) assert(pthread_equal(sent_by[i], pthread_self()));
    snd_seq_event_output_direct((void *)2, note); // replayed once per change
    assert(nsent == 4);
    pct = 100;
    assert(!pthread_create(&poll, NULL, change_brightness, &pct) && !pthread_join(poll, NULL));
    hook_test_engine = 0;
    seq_direct_cache = NULL;
    puts("pad LED brightness: a change is replayed on Engine's thread with its next event, never from the poll thread (BH-O55-4): PASS");
}

static void test_stems_processor_fallback(void) {
    char dir[] = "/tmp/stems-fallback-XXXXXX", bin[PATH_MAX];
    assert(mkdtemp(dir));
    assert(snprintf(stems_bundle_file, sizeof stems_bundle_file, "%s/stems-processor", dir) > 0);
    assert(snprintf(bin, sizeof bin, "%s/bin", dir) > 0);
    assert(!mkdir(bin, 0700));
    assert(snprintf(stems_bin_file, sizeof stems_bin_file, "%s/bin/stems-processor", dir) > 0);
    const char *stock = "/usr/local/bin/stems-processor";
    assert(raw_access(stock, F_OK) != 0); // Run on the build host, never a deck.
    hook_test_engine = 1;
    assert(!strcmp(stems_processor_path(stock), stock));
    struct statx stx;
    struct stat st;
    assert(statx(AT_FDCWD, stock, 0, 0xfffu, &stx) == -1);
    assert(open(stock, O_RDONLY) == -1);
    assert(stat(stock, &st) == -1);
    const char *paths[] = {stems_bundle_file, stems_bin_file};
    for (unsigned i = 0; i < sizeof paths / sizeof *paths; i++) {
        FILE *f = fopen(paths[i], "w");
        assert(f && fputs("#!/bin/sh\nexit 0\n", f) >= 0 && !fclose(f));
        assert(!chmod(paths[i], 0644));
        assert(!strcmp(stems_processor_path(stock), stock));
        assert(statx(AT_FDCWD, stock, 0, 0xfffu, &stx) == -1);
        assert(open(stock, O_RDONLY) == -1);
        assert(stat(stock, &st) == -1);
        assert(!chmod(paths[i], 0755));
        // Other processes must still see the missing original path.
        hook_test_engine = 0;
        assert(access(stock, X_OK) == -1);
        assert(statx(AT_FDCWD, stock, 0, 0xfffu, &stx) == -1);
        assert(open(stock, O_RDONLY) == -1);
        assert(stat(stock, &st) == -1);
        hook_test_engine = 1;
        assert(!strcmp(stems_processor_path(stock), paths[i]));
        assert(access(stock, X_OK) == 0);
        memset(&stx, 0, sizeof stx);
        assert(statx(AT_FDCWD, stock, AT_NO_AUTOMOUNT, 0xfffu, &stx) == 0);
        assert(S_ISREG(stx.stx_mode) && stx.stx_size > 0);
        // Engine's Stems launcher checks the processor with std::filesystem::status() (stat).
        memset(&st, 0, sizeof st);
        assert(stat(stock, &st) == 0 && S_ISREG(st.st_mode) && st.st_size > 0);
        int fd = open(stock, O_RDONLY);
        assert(fd >= 0);
        char bytes[2];
        assert(read(fd, bytes, sizeof bytes) == sizeof bytes && !memcmp(bytes, "#!", sizeof bytes));
        assert(!close(fd) && !unlink(paths[i]));
    }
    assert(!strcmp(stems_processor_path("/bin/sh"), "/bin/sh"));
    hook_test_engine = 0;
    assert(!rmdir(bin) && !rmdir(dir));
    puts("stems-processor fallback: isolated access/statx/stat/open, missing/non-executable and non-Engine paths: PASS");
}


// #206: the 8 AArch64 sites apply together on the all-in-one AZ04 decks, never on a
// player board code, and none of them on a 1-site mismatch.
static void test_rec_stream(const char *filename) {
    size_t len, page = (size_t)sysconf(_SC_PAGESIZE);
    size_t first = build->edit_count, count = 0;
    for (size_t i = 0; i < build->edit_count; i++)
        if (build->edits[i].group == EDIT_REC_STREAM) { if (first > i) first = i; count++; }
    assert(count == 8 && first + 8 == build->edit_count && build->edit_count == 28);
    // nop / mov w22,#0 x2 / mov w0,#0 / mov w27,#0 / b <loop tail> / b <menu> / nop
    const uint32_t want[8] = {0xd503201f, 0x52800016, 0x52800016, 0x52800000, 0x5280001b, 0, 0, 0xd503201f};
    const uint32_t target[8] = {0, 0, 0, 0, 0, 0x97afa0, 0x97fa00, 0};
    for (size_t k = 0; k < 8; k++) {
        const struct engine_edit *e = &build->edits[first + k];
        uint32_t ins; memcpy(&ins, e->replacement, 4);
        if (want[k]) assert(ins == want[k]);
        else assert((ins >> 26) == 0x05 && e->offset + ((int64_t)((int32_t)(ins << 6) >> 6) << 2) == target[k]);
    }
    char code_path[] = "/tmp/product-code-XXXXXX";
    int cfd = mkstemp(code_path); assert(cfd >= 0); close(cfd);
    const char *saved = product_code_path;
    product_code_path = code_path;
    const struct { const char *board; int corrupt, patched; } cases[] = {
        {"JC20", 0, 1}, {"JP07", 0, 0}, {"JC20", 1, 0}};
    for (size_t c = 0; c < sizeof cases / sizeof *cases; c++) {
        FILE *f = fopen(code_path, "w");
        assert(f && fputs(cases[c].board, f) >= 0 && !fclose(f));
        uchar *image = map_image(filename, &len);
        ElfW(Ehdr) *header = (void *)image;
        struct dl_phdr_info info = {.dlpi_addr = (uintptr_t)image,
            .dlpi_phdr = (void *)(image + header->e_phoff), .dlpi_phnum = header->e_phnum};
        uintptr_t found = 0;
        engine_base(&info, sizeof info, &found);
        assert(found == (uintptr_t)image);
        // The adjacent PlaylistDeck/PlayState read (ldrb w0, [x22, #450]) stays stock.
        uint32_t pdeck; memcpy(&pdeck, image + build->edits[first + 4].offset - 8, 4);
        assert(pdeck == 0x39470ac0);
        if (cases[c].corrupt) image[build->edits[first + 7].offset] ^= 0xff;
        apply_engine_edits((uintptr_t)image, page);
        apply_engine_edits((uintptr_t)image, page); // idempotent
        for (size_t k = 0; k < 8; k++) {
            int st = edit_state((uintptr_t)image, &build->edits[first + k]);
            if (cases[c].corrupt && k == 7) assert(st == 0);
            else assert(st == (cases[c].patched ? 2 : 1));
        }
        munmap(image, len);
    }
    product_code_path = saved;
    unlink(code_path);
    puts("#206 AZ04 recording with streaming tracks: 8 sites stock->patched, untouched on a player code, none on a 1-site mismatch: PASS");
}

// #336 S17: AZ04 InstantDoubleControl::doLoad() defaultValue for QJsonValue::toInt(int) and
// InstantDoubleControl::doPositionSync() zero/negative guard (0xdb8c14..0xdb8c20).
// Stock mov w1, #0 yields 0 on empty/missing remote TrackLength JSON and stock tbnz w2, #31
// only skips negative values, so both missing JSON and peer IntegerTranslator default "value": 0
// fall through into a false StreamingQualityMismatch.
static int eval_az04_toint_default(const uchar *image, uint32_t off, size_t page) {
    uint32_t ins;
    memcpy(&ins, image + off, 4);
    uint32_t code[] = {ins, 0x2a0103e0u, 0xd65f03c0u}; // mov w1, #0/-1; mov w0, w1; ret
    int (*fn)(void) = rx_code(code, sizeof code, page);
    int rc = fn();
    assert(!munmap((void *)fn, page));
    return rc;
}

static int eval_az04_sync_guard(const uchar *image, size_t page, int32_t remote_len, int64_t local_len) {
    uint32_t guard[3];
    memcpy(guard, image + 0xdb8c18u, sizeof guard);
    uint32_t ins0 = guard[0];
    // Relocate stock tbnz w2, #31, +0x1a4 to skip_mismatch (+5 insns); patched cmp w2, #0 runs verbatim.
    if ((ins0 & 0xff00001fu) == 0x37000002u) ins0 = (ins0 & ~0x0007ffe0u) | (5u << 5);
    assert((guard[2] & 0xff00001fu) == 0x54000000u); // b.eq
    uint32_t ins2 = (guard[2] & ~0x00ffffe0u) | (3u << 5);
    uint32_t code[] = {
        0x2a0103e2u, // mov w2, w1 (zero-extends w1 -> x2, matching ldr w2, [sp] at 0xdb8c14)
        ins0,        // stock: tbnz w2, #31, skip; patched: cmp w2, #0
        guard[1],    // stock: cmp x0, w2, sxtw;   patched: ccmp x0, x2, #4, gt
        ins2,        // b.eq skip
        0x52800020u, // mov w0, #1 (mismatch)
        0xd65f03c0u, // ret
        0x52800000u, // skip: mov w0, #0 (no mismatch)
        0xd65f03c0u, // ret
    };
    int (*fn)(int64_t, int32_t) = rx_code(code, sizeof code, page);
    int rc = fn(local_len, remote_len);
    assert(!munmap((void *)fn, page));
    return rc;
}

static void test_instant_double_track_length(const char *filename) {
    size_t len, page = (size_t)sysconf(_SC_PAGESIZE);
    uchar *image = map_image(filename, &len);
    ElfW(Ehdr) *header = (void *)image;
    struct dl_phdr_info info = {.dlpi_addr = (uintptr_t)image,
        .dlpi_phdr = (void *)(image + header->e_phoff), .dlpi_phnum = header->e_phnum};
    uintptr_t found = 0;
    engine_base(&info, sizeof info, &found);
    assert(found == (uintptr_t)image && !strcmp(build->name, "5.3.0-az04"));

    // Context pins: doLoad() mov x0, x23; mov w1, #0; bl QJsonValue::toInt; str w0, [x19, #424]
    uint32_t ctx[4];
    memcpy(ctx, image + 0xdbb574u, sizeof ctx);
    assert(ctx[0] == 0xaa1703e0u && ctx[1] == 0x52800001u && (ctx[2] >> 26) == 0x25u && ctx[3] == 0xb901aa60u);
    // doPositionSync(): ldr w2, [sp]; tbnz w2, #31, +0x1a4; cmp x0, w2, sxtw; b.eq +0x19c
    uint32_t guard[4];
    memcpy(guard, image + 0xdb8c14u, sizeof guard);
    assert(guard[0] == 0xb94003e2u && guard[1] == 0x37f80d22u && guard[2] == 0xeb22c01fu && guard[3] == 0x54000ce0u);

    // Stock executes both bugs: missing JSON yields 0, and remote=0 triggers false mismatch!
    int stock_def = eval_az04_toint_default(image, 0xdbb578u, page);
    assert(stock_def == 0 && !(stock_def < 0));
    assert(eval_az04_sync_guard(image, page, -1, 240000) == 0);
    assert(eval_az04_sync_guard(image, page, 0, 240000) == 1);
    assert(eval_az04_sync_guard(image, page, 180000, 240000) == 1);
    assert(eval_az04_sync_guard(image, page, 240000, 240000) == 0);

    apply_engine_edits((uintptr_t)image, page);

    // Patched returns -1 on missing JSON, skips <= 0 remote TrackLength, and preserves real mismatches.
    int patched_def = eval_az04_toint_default(image, 0xdbb578u, page);
    assert(patched_def == -1 && (patched_def < 0));
    assert(eval_az04_sync_guard(image, page, -1, 240000) == 0);
    assert(eval_az04_sync_guard(image, page, INT32_MIN, 240000) == 0);
    assert(eval_az04_sync_guard(image, page, 0, 240000) == 0);
    assert(eval_az04_sync_guard(image, page, 0, (int64_t)0x100000000LL) == 0);
    assert(eval_az04_sync_guard(image, page, 180000, 240000) == 1);
    assert(eval_az04_sync_guard(image, page, 180000, (int64_t)0x100000000LL + 180000) == 1);
    assert(eval_az04_sync_guard(image, page, 240000, 240000) == 0);
    munmap(image, len);

    // Switching instant-double off in hook-disable.conf leaves both 0xdbb578 and 0xdb8c18 stock.
    char off_path[] = "/tmp/az04-id-off-XXXXXX";
    int ofd = mkstemp(off_path); assert(ofd >= 0);
    assert(write(ofd, "instant-double\n", 15) == 15);
    close(ofd);
    const char *saved_hf = hf_path;
    hf_path = off_path;
    hf_reload();
    image = map_image(filename, &len);
    apply_engine_edits((uintptr_t)image, page);
    assert(eval_az04_toint_default(image, 0xdbb578u, page) == 0);
    assert(eval_az04_sync_guard(image, page, 0, 240000) == 1);
    munmap(image, len);
    hf_path = saved_hf;
    hf_reload();
    unlink(off_path);
    puts("#336 S17 AZ04 InstantDouble remote TrackLength: toInt(-1) and <=0 guard verified, executed, and switchable: PASS");
}

// HF_SHADOW_PAUSED_FOLLOW on the real image: WaveFormPositionItem::currentPosition and its
// slot in both vtables, PlanckGUI's metaObject() in its own vtable, and the vector of the
// decks' items where PlanckGUI::waveformPositionItem reads it.
static uint64_t relative_at(const uchar *image, uint64_t address) {
    const Elf64_Ehdr *eh = (const void *)image;
    const Elf64_Shdr *sh = (const void *)(image + eh->e_shoff);
    for (unsigned i = 0; i < eh->e_shnum; i++) {
        if (sh[i].sh_type != SHT_RELA) continue;
        const Elf64_Rela *r = (const void *)(image + sh[i].sh_offset);
        for (size_t n = sh[i].sh_size / sizeof *r; n--; r++)
            if (r->r_offset == address && ELF64_R_TYPE(r->r_info) == R_AARCH64_RELATIVE) return (uint64_t)r->r_addend;
    }
    return 0;
}

static void test_shadow_follow_sites(const char *filename) {
    size_t len;
    uchar *image = map_image(filename, &len);
    select_build(&engine_builds[0]);
    assert(build->shadow_position_fn && build->shadow_owner_slot && !build->shadow_capture_site);
    assert(!memcmp(image + build->shadow_position_fn, build->shadow_position_prefix, build->shadow_position_prefix_size));
    assert(relative_at(image, build->shadow_vtable_a + build->shadow_vtable_slot) == build->shadow_position_fn);
    assert(relative_at(image, build->shadow_vtable_b + build->shadow_vtable_slot) == build->shadow_position_fn);
    // The owner's slot is the first of the vtable whose type is named PlanckGUI, and holds
    // moc's metaObject(): bti c; ldr x0, [x0, #8]; ldr x1, [x0, #56]; cbz x1.
    assert(relative_at(image, build->shadow_owner_slot) == build->shadow_owner_fn);
    uint64_t type = relative_at(image, build->shadow_owner_slot - 8);
    uint64_t name = type ? relative_at(image, type + 8) : 0;
    assert(name && !strcmp((const char *)image + name, "9PlanckGUI")); // .rodata: file offset = address
    const uint32_t *meta = (const void *)(image + build->shadow_owner_fn);
    assert(meta[0] == 0xd503245f && meta[1] == 0xf9400400 && meta[2] == 0xf9401c01 && meta[3] >> 24 == 0xb4);
    // ldr x2, [x0, #vector]; ldr x0, [x0, #vector + 8]: begin and end, from x0 = PlanckGUI.
    const uint32_t begin = 0xf9400002 | ((build->shadow_array_offset / 8) << 10), end = 0xf9400000 | ((build->shadow_array_offset / 8 + 1) << 10);
    int found = 0;
    for (size_t at = 0xb0b000; at < 0xb25000; at += 4) {
        uint32_t pair[2];
        memcpy(pair, image + at, sizeof pair);
        found += pair[0] == begin && pair[1] == end;
    }
    assert(found == 1);
    munmap(image, len);
    puts("AZ04 paused shadow follow: currentPosition in both vtables, PlanckGUI's metaObject slot, the deck vector: PASS");
}

int main(int argc, char **argv) {
    assert(sizeof engine_builds / sizeof *engine_builds == 1);
    test_thread_policy();
    test_resources();
    test_collision();
    test_led();
    test_led_replay_on_engine_thread();
    test_stems_processor_fallback();
    test_dsp_event_guards();
    for (int i = 1; i < argc; i++) { test_engine(argv[i]); test_rec_stream(argv[i]); test_instant_double_track_length(argv[i]); test_sqlite(argv[i]); test_dsp(argv[i]); test_bt_send(argv[i]); test_shadow_follow_sites(argv[i]); }
    const char *oa = getenv("AZ04_OFFLINE_ANALYZER");
    if (oa) test_offline_analyzer(oa);
    return 0;
}
