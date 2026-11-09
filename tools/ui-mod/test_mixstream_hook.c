// Native armhf on Batam (no qemu):
// arm-linux-gnueabihf-gcc -O2 -o test_mixstream_hook test_mixstream_hook.c -ldl -lpthread
// ./test_mixstream_hook <5.3.0>/usr/Engine/Engine <5.3.0-sclive4>/usr/Engine/Engine
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>

#if !defined(__arm__)
#error Run this test as native armhf: it executes the actual A32 config trampoline.
#endif

static uint32_t word(const void *p) { uint32_t v; memcpy(&v, p, sizeof v); return v; }

static int stock_processor;
static int fake_access(const char *path, int mode) {
    assert(mode == X_OK);
    return !strcmp(path, "/data/sc5000-custom/bundle/bin/stems-processor") ||
           (stock_processor && !strcmp(path, "/usr/local/bin/stems-processor")) ? 0 : -1;
}

static void test_processor_delivery(void) {
    access_cache = (void *)fake_access;
    assert(!strcmp(stems_processor_path("/usr/local/bin/stems-processor"),
                   "/data/sc5000-custom/bundle/bin/stems-processor"));
    stock_processor = 1;
    assert(!strcmp(stems_processor_path("/usr/local/bin/stems-processor"), "/usr/local/bin/stems-processor"));
    assert(!strcmp(stems_processor_path("/usr/bin/other"), "/usr/bin/other"));
    access_cache = NULL;
    puts("Gen-2 Stems: absent stock processor redirects to executable bundle wrapper; stock retained: PASS");
}

static void test_engine(const char *filename) {
    FILE *f = fopen(filename, "rb"); assert(f);
    assert(!fseek(f, 0, SEEK_END)); long size = ftell(f); assert(size > 0); rewind(f);
    size_t page = (size_t)sysconf(_SC_PAGESIZE), len = ((size_t)size + page - 1) / page * page;
    uchar *image = mmap(NULL, len, PROT_READ | PROT_WRITE | PROT_EXEC, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(image != MAP_FAILED && fread(image, 1, (size_t)size, f) == (size_t)size); fclose(f);
    ElfW(Ehdr) *header = (void *)image;
    assert(header->e_machine == EM_ARM && header->e_ident[EI_CLASS] == ELFCLASS32);
    struct dl_phdr_info info = {.dlpi_addr = (uintptr_t)image,
        .dlpi_phdr = (void *)(image + header->e_phoff), .dlpi_phnum = header->e_phnum};
    uintptr_t found = 0;
    engine_base(&info, sizeof info, &found);
    assert(found == (uintptr_t)image && build->config_zoom == 0x52c);
    uchar *id = memmem(image, (size_t)size, build->id, 20); assert(id);
    *id ^= 1; found = 0; engine_base(&info, sizeof info, &found); assert(!found); *id ^= 1;

    const struct engine_edit *sweep = NULL;
    for (unsigned i = 0; i < build->edit_count; i++) {
        const struct engine_edit *e = &build->edits[i];
        assert(e->offset + e->size <= (size_t)size && edit_state((uintptr_t)image, e) == 1);
        if (e->offset == 0xd1aaf4 || e->offset == 0xd27e14) sweep = e;
    }
    assert(sweep && sweep->group == EDIT_STEMS && sweep->size == 4);
    assert(word(image + sweep->offset - 8) == 0xe5d0312e); // ldrb r3, [r0, #hasNHFx]
    assert(word(image + sweep->offset - 4) == 0xe3530000); // cmp r3, #0
    assert(word(sweep->expected) == 0x0a0003bb && word(sweep->replacement) == 0xea0003bb);
    memcpy(image + sweep->offset, sweep->replacement, 4);
    assert(edit_state((uintptr_t)image, sweep) == 2);
    image[sweep->offset] ^= 1; assert(!edit_state((uintptr_t)image, sweep));
    memcpy(image + sweep->offset, sweep->expected, 4);
    printf("%s: BuildID refusal, all %u stock edits, SweepFX 0x%x stock/patched/mismatch: PASS\n",
           build->name, build->edit_count, sweep->offset);

    prepare_config((uintptr_t)image, page);
    // Return to the test caller after the displaced loads, rather than executing Engine.
    uint32_t ret = 0xe12fff1e; // bx lr
    memcpy(image + build->config_site + 8, &ret, sizeof ret);
    __builtin___clear_cache((char *)image, (char *)image + len);
    void (*configure)(void *) = (void (*)(void *))(uintptr_t)config_jump[1];
    for (unsigned nh = 0; nh <= 1; nh++) {
        for (unsigned seed = 0; seed < 4; seed++) {
            uchar config[0x600], expected[sizeof config];
            for (unsigned i = 0; i < sizeof config; i++) config[i] = (uchar)(i * 31 + seed * 73);
            config[0x12e] = (uchar)nh;
            // Include the important non-NH case where Beat Jump was deliberately disabled.
            config[0xe1] = (uchar)(seed & 1);
            memcpy(expected, config, sizeof config);
            for (unsigned i = 0; i < 5; i++) expected[build->config_field[i]] = i == 4 ? 0 : 1;
            expected[0x54] = 1; // existing #152
            uint32_t zoom = 30; memcpy(expected + build->config_zoom, &zoom, sizeof zoom);
            if (nh) expected[0xe1] = expected[0x19c] = expected[0x19d] = expected[0x4cd] = 1;
            configure(config);
            assert(!memcmp(config, expected, sizeof config));
            assert(config[0x12e] == nh); // Main FX remains NH
        }
    }
    munmap((void *)(uintptr_t)config_jump[1], page);
    munmap(image, len);
    printf("%s: real A32 config trampoline, NH flags, entire non-NH config byte equality: PASS\n", build->name);
}

int main(int argc, char **argv) {
    assert(argc == 3);
    test_processor_delivery();
    for (int i = 1; i < argc; i++) test_engine(argv[i]);
    return 0;
}
