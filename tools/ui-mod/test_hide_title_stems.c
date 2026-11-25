// Native armhf / AArch64 regression for #336 S20 (t/71377) against the stock
// Engine ELFs, never qemu. Executes the real Deck::updateTrackMetadata
// HideTitleArtist Stemmed-clear machine code before and after apply_engine_edits.
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>

static volatile int fake_feat_enabled;
static volatile int fake_track_demo;
static volatile int fake_track_stemmed;
static volatile int fake_set_calls;
static volatile int fake_set_last;
static void *volatile fake_set_last_this;

static _Bool fake_feat_is_enabled(void *self) {
    (void)self;
    return fake_feat_enabled != 0;
}

static _Bool fake_track_is_demo(void *self) {
    (void)self;
    return fake_track_demo != 0;
}

static _Bool fake_track_is_stemmed(void *self) {
    (void)self;
    return fake_track_stemmed != 0;
}

static _Bool trap_wrong_slot(void *self) {
    (void)self;
    fprintf(stderr, "FAIL: called wrong Track vtable slot\n");
    abort();
}

static void fake_translator_set_state(void *self, _Bool state) {
    fake_set_calls++;
    fake_set_last = state ? 1 : 0;
    fake_set_last_this = self;
}

struct fake_obj {
    void **vtable;
};

#if defined(__aarch64__)
struct a64_site_io {
    uintptr_t entry;
    uintptr_t x22_val;
    uintptr_t track;
    uintptr_t translator;
    uintptr_t out_x19;
    uintptr_t out_x22;
    uintptr_t out_x24;
};

static __attribute__((noinline)) void run_site_a64(struct a64_site_io *io) {
    __asm__ volatile(
        "stp x19, x20, [sp, #-48]!\n\t"
        "stp x21, x22, [sp, #16]\n\t"
        "stp x24, x30, [sp, #32]\n\t"
        "mov x21, %[io]\n\t"
        "ldr x16, [x21, #0]\n\t"
        "ldr x22, [x21, #8]\n\t"
        "ldr x19, [x21, #16]\n\t"
        "ldr x24, [x21, #24]\n\t"
        "adr x20, 1f\n\t"
        "br x16\n"
        "1:\n\t"
        "str x19, [x21, #32]\n\t"
        "str x22, [x21, #40]\n\t"
        "str x24, [x21, #48]\n\t"
        "ldp x24, x30, [sp, #32]\n\t"
        "ldp x21, x22, [sp, #16]\n\t"
        "ldp x19, x20, [sp], #48\n\t"
        :
        : [io] "r"(io)
        : "x0", "x1", "x2", "x3", "x4", "x5", "x6", "x7", "x8", "x9",
          "x10", "x11", "x12", "x13", "x14", "x15", "x16", "x17", "x18",
          "memory", "cc"
    );
}
#else
struct arm_site_io {
    uintptr_t entry;
    uintptr_t track;
    uintptr_t translator;
    uintptr_t out_r4;
    uintptr_t out_r7;
};

static __attribute__((noinline, target("arm"))) void run_site_arm(struct arm_site_io *io) {
    __asm__ volatile(
        "push {r4, r5, r7, r8, r9, lr}\n\t"
        "mov r5, %[io]\n\t"
        "ldr r9, [r5, #0]\n\t"
        "ldr r4, [r5, #4]\n\t"
        "ldr r7, [r5, #8]\n\t"
        "adr r8, 1f\n\t"
        "bx r9\n"
        "1:\n\t"
        "str r4, [r5, #12]\n\t"
        "str r7, [r5, #16]\n\t"
        "pop {r4, r5, r7, r8, r9, lr}\n\t"
        :
        : [io] "r"(io)
        : "r0", "r1", "r2", "r3", "r12", "memory", "cc"
    );
}
#endif

static void run_case(uintptr_t base, uintptr_t site_rva, uintptr_t feat_slot_rva,
                     struct fake_obj *feat, int feat_en,
                     struct fake_obj *track, int demo, int stemmed,
                     struct fake_obj *translator, int expected_state) {
    void **slot = (void **)(base + feat_slot_rva);
    *slot = feat;
    fake_feat_enabled = feat_en;
    fake_track_demo = demo;
    fake_track_stemmed = stemmed;
    fake_set_calls = 0;
    fake_set_last = -1;
    fake_set_last_this = NULL;
#if defined(__aarch64__)
    struct a64_site_io io = {
        .entry = base + site_rva,
        .x22_val = base + 0x16a7000u,
        .track = (uintptr_t)track,
        .translator = (uintptr_t)translator,
    };
    run_site_a64(&io);
    assert(io.out_x19 == (uintptr_t)track);
    assert(io.out_x22 == base + 0x16a7000u);
    assert(io.out_x24 == (uintptr_t)translator);
#else
    struct arm_site_io io = {
        .entry = base + site_rva,
        .track = (uintptr_t)track,
        .translator = (uintptr_t)translator,
    };
    run_site_arm(&io);
    assert(io.out_r4 == (uintptr_t)track);
    assert(io.out_r7 == (uintptr_t)translator);
#endif
    assert(fake_set_calls == 1);
    assert(fake_set_last_this == translator);
    assert(fake_set_last == expected_state);
}

static void test_binary(const char *label, const char *filename,
                        uintptr_t site_rva, uintptr_t resume_rva,
                        uintptr_t feat_slot_rva,
                        unsigned demo_slot, unsigned stemmed_slot) {
    FILE *f = fopen(filename, "rb");
    assert(f);
    assert(!fseek(f, 0, SEEK_END));
    long size = ftell(f);
    assert(size > 0);
    rewind(f);
    char *file = malloc((size_t)size);
    assert(file);
    assert(fread(file, 1, (size_t)size, f) == (size_t)size);
    fclose(f);

    ElfW(Ehdr) *eh = (void *)file;
    assert(eh->e_ident[EI_CLASS] == (sizeof(void *) == 8 ? ELFCLASS64 : ELFCLASS32));
    ElfW(Phdr) *ph = (void *)(file + eh->e_phoff);
    size_t len = 0, page = (size_t)sysconf(_SC_PAGESIZE);
    for (unsigned i = 0; i < eh->e_phnum; i++)
        if (ph[i].p_type == PT_LOAD && ph[i].p_vaddr + ph[i].p_memsz > len)
            len = ph[i].p_vaddr + ph[i].p_memsz;
    len = (len + page - 1) & ~(page - 1);
    char *image = mmap(NULL, len, PROT_READ | PROT_WRITE | PROT_EXEC,
                       MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(image != MAP_FAILED);
    for (unsigned i = 0; i < eh->e_phnum; i++)
        if (ph[i].p_type == PT_LOAD)
            memcpy(image + ph[i].p_vaddr, file + ph[i].p_offset, ph[i].p_filesz);

    // Intercept the resume address immediately after the Stemmed-clear block so
    // the real stock/patched instructions return cleanly to our harness.
    uintptr_t base = (uintptr_t)image;
#if defined(__aarch64__)
    uint32_t ret_insn = 0xd61f0280u; /* br x20 */
#else
    uint32_t ret_insn = 0xe12fff18u; /* bx r8 */
#endif
    memcpy((void *)(base + resume_rva), &ret_insn, sizeof ret_insn);
    __builtin___clear_cache(image, image + len);

    build = NULL;
    uintptr_t found = 0;
    struct dl_phdr_info info = {
        .dlpi_addr = base, .dlpi_phdr = ph, .dlpi_phnum = eh->e_phnum
    };
    engine_base(&info, sizeof info, &found);
    assert(found == base && build != NULL);

    void *feat_vt[4] = {(void *)&fake_feat_is_enabled, NULL, NULL, NULL};
    struct fake_obj feat = {.vtable = feat_vt};

    void *track_vt[80];
    for (unsigned i = 0; i < 80; i++) track_vt[i] = (void *)&trap_wrong_slot;
    track_vt[demo_slot] = (void *)&fake_track_is_demo;
    track_vt[stemmed_slot] = (void *)&fake_track_is_stemmed;
    struct fake_obj track = {.vtable = track_vt};

    void *tr_vt[48];
    memset(tr_vt, 0, sizeof tr_vt);
    tr_vt[40] = (void *)&fake_translator_set_state;
    struct fake_obj translator = {.vtable = tr_vt};

    // 1. Reproduce stock bug: HideTitleArtist clears Stemmed even on a stemmed
    //    track with StemsPreRenderedPlayback enabled.
    run_case(base, site_rva, feat_slot_rva, &feat, 1, &track, 0, 1, &translator, 0);

    // 2. Respect HF_STEMS disable: site stays untouched when stems is disabled.
    char dis_file[] = "/tmp/s20-disable-XXXXXX";
    int dfd = mkstemp(dis_file);
    assert(dfd >= 0);
    assert(write(dfd, "stems\n", 6) == 6);
    close(dfd);
    hf_path = dis_file;
    hf_reload();
    uint32_t saved_word0 = 0, saved_word1 = 0;
    memcpy(&saved_word0, (const void *)(base + site_rva), sizeof saved_word0);
    memcpy(&saved_word1, (const void *)(base + site_rva + 4), sizeof saved_word1);
    apply_engine_edits(base, page);
    uint32_t after_disabled = 0;
    memcpy(&after_disabled, (const void *)(base + site_rva), sizeof after_disabled);
    assert(after_disabled == saved_word0);
    run_case(base, site_rva, feat_slot_rva, &feat, 1, &track, 0, 1, &translator, 0);
    unlink(dis_file);
    hf_path = "/nonexistent-s20-hook-disable";
    hf_reload();

    // 3. Reject a mutated/unexpected instruction sequence at site (+4 is exact on both arches).
    uint32_t bad_word1 = saved_word1 ^ 0x00000001u;
    memcpy((void *)(base + site_rva + 4), &bad_word1, sizeof bad_word1);
    __builtin___clear_cache((void *)(base + site_rva + 4), (void *)(base + site_rva + 8));
    apply_engine_edits(base, page);
    uint32_t after_bad = 0;
    memcpy(&after_bad, (const void *)(base + site_rva), sizeof after_bad);
    assert(after_bad == saved_word0);
    memcpy((void *)(base + site_rva + 4), &saved_word1, sizeof saved_word1);
    __builtin___clear_cache((void *)(base + site_rva + 4), (void *)(base + site_rva + 8));

    // 4. Install through the production path (twice to verify idempotency).
    apply_engine_edits(base, page);
    uint32_t patched_words[4];
    memcpy(patched_words, (const void *)(base + site_rva), sizeof patched_words);
    apply_engine_edits(base, page);
    assert(memcmp(patched_words, (const void *)(base + site_rva), sizeof patched_words) == 0);

    // 5. Full truth table matching Deck::updateTrackMetadata's normal branch:
    //    (StemsPreRenderedPlayback::isEnabled() || Track::isDemoContentTrack())
    //      ? Track::isStemmed() : false
    run_case(base, site_rva, feat_slot_rva, &feat, 1, &track, 0, 1, &translator, 1);
    run_case(base, site_rva, feat_slot_rva, &feat, 1, &track, 0, 0, &translator, 0);
    run_case(base, site_rva, feat_slot_rva, &feat, 0, &track, 1, 1, &translator, 1);
    run_case(base, site_rva, feat_slot_rva, &feat, 0, &track, 1, 0, &translator, 0);
    run_case(base, site_rva, feat_slot_rva, &feat, 0, &track, 0, 1, &translator, 0);
    run_case(base, site_rva, feat_slot_rva, NULL,  0, &track, 0, 1, &translator, 0);
    run_case(base, site_rva, feat_slot_rva, NULL,  0, &track, 1, 1, &translator, 1);

    printf("#336 S20 %s: HideTitleArtist preserves Stemmed truth table & registers: PASS\n", label);
    munmap(image, len);
    free(file);
}

int main(int argc, char **argv) {
#if defined(__aarch64__)
    assert(argc == 2);
    test_binary("5.3.0-az04", argv[1], 0xe29f98u, 0xe29fb4u, 0x2ee6f90u, 69, 66);
#else
    assert(argc == 4);
    test_binary("5.0.4", argv[1], 0xca5fc8u, 0xca5fe8u, 0x2a91e9cu, 68, 65);
    test_binary("5.3.0", argv[2], 0xd4c6f8u, 0xd4c718u, 0x2c97bd4u, 69, 66);
    test_binary("5.3.0-sclive4", argv[3], 0xd7edf8u, 0xd7ee18u, 0x2d180c8u, 69, 66);
#endif
    return 0;
}
