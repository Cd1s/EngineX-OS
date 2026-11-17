// Hook feature switches (hook_features.h): build option, hook-disable.conf,
// skipped Engine edits and the getConfig lines they leave stock.
// Built with zoom30 switched off at build time, as build.sh does it:
// arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -DENGINEX_HOOK_DISABLE='"zoom30"' -o test_hook_features test_hook_features.c -ldl -lpthread
// ./test_hook_features   (native armhf; elsewhere: qemu-arm -L /usr/arm-linux-gnueabihf ./test_hook_features)
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>
#if !defined(__arm__)
#error armhf test: the getConfig trampoline is A32 code
#endif

static char dir[] = "/tmp/hook-features-XXXXXX";
static char conf[sizeof dir + 32], board[sizeof dir + 32], state[sizeof dir + 32];

static void write_file(const char *path, const char *text) {
    FILE *f = fopen(path, "w");
    assert(f);
    fputs(text, f);
    fclose(f);
}

static void use_conf(const char *text) {
    if (text) write_file(conf, text);
    else unlink(conf);
    hf_path = conf;
    hf_reload();
}

static void test_parse(void) {
    unsigned unknown = 0;
    const char text[] = "gateway, stems\n# zoom30 is a comment\n  nope\tsqlite-tuning#tail\r\n";
    uint32_t mask = hf_parse(text, sizeof text - 1, &unknown);
    assert(mask == (1u << HF_GATEWAY | 1u << HF_STEMS | 1u << HF_SQLITE_TUNING));
    assert(unknown == 1);
    for (int f = 1; f < HF_COUNT; f++) {
        unknown = 0;
        assert(hf_parse(hf_names[f], strlen(hf_names[f]), &unknown) == 1u << f && !unknown);
    }
    assert(hf_parse("stems-eqx stem", 14, &unknown) == 0); // whole names only
    // The services the console switches off are known names: the hook has nothing to
    // skip for them and logs no unknown feature.
    unknown = 0;
    const char services[] = "rb-bridge\ndeemixd # services\n";
    assert(hf_parse(services, sizeof services - 1, &unknown) == (1u << HF_RB_BRIDGE | 1u << HF_DEEMIXD) && !unknown);
    puts("hook-disable names: separators, comments, whole names, unknown counted: PASS");
}

static void test_masks(void) {
    use_conf(NULL);
    assert(hf_on(HF_ALWAYS));
    assert(!hf_on(HF_ZOOM30)); // -DENGINEX_HOOK_DISABLE
    for (int f = 1; f < HF_COUNT; f++)
        if (f != HF_ZOOM30) assert(hf_on(f));
    char text[256];
    hf_describe(text, sizeof text);
    assert(!strcmp(text, "zoom30"));
    use_conf("stems\nsqlite-tuning # keep Engine's own pragmas\n");
    assert(!hf_on(HF_STEMS) && !hf_on(HF_SQLITE_TUNING) && !hf_on(HF_ZOOM30));
    assert(hf_on(HF_GATEWAY) && hf_on(HF_ALWAYS));
    hf_describe(text, sizeof text);
    assert(!strcmp(text, "stems,zoom30,sqlite-tuning"));
    use_conf(""); // an empty file switches nothing off; the build still does
    assert(hf_on(HF_STEMS) && !hf_on(HF_ZOOM30));
    // A commented file longer than 4 KiB: the last name still counts (it once read 4 KiB only).
    static char big[20000];
    size_t used = 0;
    while (used < 16000) used += (size_t)snprintf(big + used, sizeof big - used, "# %s\n", "a long description line of a feature, as the template has it");
    snprintf(big + used, sizeof big - used, "ui-packs # last line\n");
    use_conf(big);
    assert(!hf_on(HF_UI_PACKS) && hf_on(HF_STEMS));
    // Larger than the read buffer: refused whole rather than read in part.
    static char huge[70000];
    memset(huge, '#', sizeof huge - 1);
    memcpy(huge, "stems\n", 6);
    use_conf(huge);
    assert(hf_on(HF_STEMS) && !hf_on(HF_ZOOM30));
    puts("build mask and hook-disable.conf combine; empty or missing file keeps the build: PASS");
}

static const struct engine_edit test_edits[] = {
    {0x10, "AAAA", "aaaa", 4, EDIT_ONE, HF_GATEWAY},
    {0x20, "BBBB", "bbbb", 4, EDIT_ONE, HF_ALWAYS},
    {0x30, "CCCC", "cccc", 4, EDIT_REC_STREAM, HF_REC_STREAMING},
    {0x40, "DDDD", "dddd", 4, EDIT_REC_STREAM, HF_REC_STREAMING},
    {0x50, "EEEE", "eeee", 4, EDIT_STEMS, HF_STEMS},
    {0x60, "FFFF", "ffff", 4, EDIT_STEMS, HF_ALWAYS},
};
static const struct engine_build test_build = {.name = "test", .edits = test_edits,
                                               .edit_count = sizeof test_edits / sizeof *test_edits};

static uchar *fresh_engine(size_t page) {
    uchar *m = mmap(NULL, page, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(m != MAP_FAILED);
    for (unsigned i = 0; i < test_build.edit_count; i++) memcpy(m + test_edits[i].offset, test_edits[i].expected, 4);
    return m;
}

static void test_edits_skip(void) {
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    const struct engine_build *saved = build;
    build = &test_build;
    use_conf("gateway rec-streaming stems");
    uchar *m = fresh_engine(page);
    apply_engine_edits((uintptr_t)m, page);
    assert(!memcmp(m + 0x10, "AAAA", 4)); // gateway off
    assert(!memcmp(m + 0x20, "bbbb", 4)); // HF_ALWAYS
    assert(!memcmp(m + 0x30, "CCCC", 4) && !memcmp(m + 0x40, "DDDD", 4)); // whole group stays stock
    assert(!memcmp(m + 0x50, "EEEE", 4)); // stems off
    assert(!memcmp(m + 0x60, "ffff", 4)); // its group's getConfig-like site still applies
    munmap(m, page);
    use_conf(NULL);
    m = fresh_engine(page);
    apply_engine_edits((uintptr_t)m, page);
    for (unsigned i = 0; i < test_build.edit_count; i++)
        assert(!memcmp(m + test_edits[i].offset, test_edits[i].replacement, 4));
    munmap(m, page);
    build = saved;
    puts("switched-off features leave their Engine sites stock, the rest is applied: PASS");
}

// #207's longer Reverb tail is a switch of its own: every Reverb decay site carries it,
// so switching it off leaves Engine's own decay formula (applied as one group).
static void test_reverb_tail_switch(void) {
    unsigned sites = 0;
    for (unsigned b = 0; b < sizeof engine_builds / sizeof *engine_builds; b++)
        for (unsigned i = 0; i < engine_builds[b].edit_count; i++) {
            const struct engine_edit *e = &engine_builds[b].edits[i];
            if (e->group != EDIT_REVERB) continue;
            assert(e->feature == HF_REVERB_TAIL);
            sites++;
        }
    assert(sites == 3); // 5.0.4, 5.3.0 and 5.3.0-sclive4
    assert(!strcmp(hf_names[HF_REVERB_TAIL], "reverb-tail"));
    puts("the longer Reverb tail (#207) has its own switch on every build: PASS");
}

static const uint32_t *config_code(size_t page) {
    // prepare_config reads only the two literal words at config_r2 / config_r3.
    size_t span = ((size_t)build->config_r3 + 8 + page - 1) / page * page;
    uchar *m = mmap(NULL, span, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS | MAP_NORESERVE, -1, 0);
    assert(m != MAP_FAILED);
    prepare_config((uintptr_t)m, page);
    munmap(m, span);
    return (const uint32_t *)(uintptr_t)config_jump[1];
}

static void test_config_lines(void) {
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    const struct engine_build *saved = build;
    for (unsigned i = 0; i < sizeof engine_builds / sizeof *engine_builds; i++)
        if (!strcmp(engine_builds[i].name, "5.3.0-sclive4")) build = &engine_builds[i];
    assert(build != saved || !strcmp(saved->name, "5.3.0-sclive4"));
    const uint32_t nop = 0xe320f000;
    aux_ready = 0;
    use_conf(NULL);
    const uint32_t *code = config_code(page);
    assert(code[1] != nop && code[2] != nop && code[3] != nop && code[4] != nop); // stems, bluetooth
    assert(code[6] == 0xe5c02054);                  // #152 stems-eq
    assert(code[8] != nop);                         // disableHfsPlus = 0
    assert(code[15] == nop && code[16] == nop);     // zoom30 off in this build
    assert(code[17] == 0xe5d0212e);                 // #166 sweep-fx
    use_conf("stems bluetooth filesystems stems-eq sweep-fx");
    code = config_code(page);
    for (unsigned i = 1; i <= 8; i++) if (i != 7) assert(code[i] == nop);
    assert(code[7] == 0xe3a02000);                  // mov r2,#0 stays, nothing stores it
    for (unsigned i = 9; i <= 23; i++) assert(code[i] == nop);
    assert(code[24] == nop && code[25] == nop);    // no Bluetooth output site on this build
    assert(code[26] == 0xe59f2004 && code[28] == 0xe59ff004); // the displaced loads and return remain
    use_conf(NULL);
    build = saved;
    puts("getConfig keeps Engine's values for switched-off features, the return path stays: PASS");
}

static void test_runtime_hooks(void) {
    hook_test_engine = 1;
    use_conf(NULL);
    assert(analytics_db("/data/x/analytics/posthog.db") && crashpad("/usr/Engine/crashpad_handler"));
    struct addrinfo *res = (void *)1;
    assert(getaddrinfo("autoupdate.airmusictech.com", "443", NULL, &res) == EAI_NONAME && !res);
    use_conf("telemetry-block");
    assert(!analytics_db("/data/x/analytics/posthog.db") && !crashpad("/usr/Engine/crashpad_handler"));
    char mapped[PATH_MAX];
    const char *events = "/data/x/analytics/events.db";
    assert(volatile_path(AT_FDCWD, events, mapped) == events); // no redirect into /tmp
    use_conf(NULL);
    hook_test_engine = 0;
    puts("telemetry-block and vendor-update-block gate their runtime hooks: PASS");
}

static void read_state(char *text, size_t size) {
    FILE *f = fopen(state, "r");
    assert(f);
    size_t n = fread(text, 1, size - 1, f);
    fclose(f);
    text[n] = 0;
}

static void test_publish(void) {
    char text[256], want[256], tmp[sizeof state + 8];
    hf_state_path = state;
    use_conf("stems\n");
    hf_publish();
    read_state(text, sizeof text);
    snprintf(want, sizeof want, "pid=%d\nbuild=zoom30\noff=stems,zoom30\n", (int)getpid());
    assert(!strcmp(text, want));
    snprintf(tmp, sizeof tmp, "%s.tmp", state);
    assert(access(tmp, F_OK) != 0);
    use_conf(NULL);
    hf_publish();
    read_state(text, sizeof text);
    snprintf(want, sizeof want, "pid=%d\nbuild=zoom30\noff=zoom30\n", (int)getpid());
    assert(!strcmp(text, want));
    puts("the running Engine's feature state is published whole for stream-sync: PASS");
}

int main(void) {
    assert(mkdtemp(dir));
    snprintf(conf, sizeof conf, "%s/hook-disable.conf", dir);
    snprintf(board, sizeof board, "%s/product-code", dir);
    snprintf(state, sizeof state, "%s/hook-features", dir);
    write_file(board, "JP21");
    product_code_path = board;
    test_parse();
    test_masks();
    test_edits_skip();
    test_config_lines();
    test_reverb_tail_switch();
    test_runtime_hooks();
    test_publish();
    unlink(state);
    unlink(conf);
    unlink(board);
    rmdir(dir);
    return 0;
}
