// the build host native armhf; executes A32 trampolines against the real stock Engines.
// arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -o test_sclive_aux test_sclive_aux.c -ldl -lpthread
// ./test_sclive_aux <5.3.0 Engine> <5.3.0-sclive4 Engine>
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>
#if !defined(__arm__)
#error Native armhf test required
#endif

static const char *board_file;
static unsigned calls, mask;
static int selection, expect_selecting = 1;
static float **original_io;
static float *raw_left, *raw_right;
static void *fake_init(void *c, void *definition, void *mic2, void *aux, uintptr_t flag) {
    assert(definition == (void *)0x1234 && mic2 == (void *)0x2345 && aux == (void *)0x3456 && flag == 1);
    // Model a restored selector without emitting ANY property callback.
    *(int *)((char *)c + 0x88) = selection;
    return c;
}
static void test_startup(void) {
    _Alignas(8) uchar c[0xa0] = {0};
    aux_init_native = fake_init;
    for (selection = 0; selection <= 1; selection++) {
        aux_input_select = 0;
        assert(sclive_aux_construct(c, (void *)0x1234, (void *)0x2345, (void *)0x3456, 1) == c);
        assert(aux_input_select == selection);
    }
    puts("Boot selector already AUX: publish native +0x88 without any InputSelect/ON event: PASS");
}
static void fake_process(void *m, void *b) {
    (void)m;
    float **io = b;
    unsigned frames = *(unsigned *)((char *)b + 0x140);
    calls++;
    if (!mask) {
        assert(!memcmp(io, original_io, 0x148));
        return;
    }
    for (unsigned i = 0x2e; i < 0x36; i++) {
        if (i < 0x30 || (i < 0x32 && selection != 1)) {
            assert(io[i] == original_io[i]); // MIC2, and AUX outside AUX mode
        } else if (i >= 0x32 && selection == 1 && raw_left && raw_right) {
            assert(io[i] == (i & 1 ? raw_right : raw_left));
        } else {
            assert(io[i]);
            for (unsigned j = 0; j < frames; j++) assert(io[i][j] == 0);
        }
    }
    assert(io[0x2c] == original_io[0x2c] && io[0x2d] == original_io[0x2d]); // Mic1 untouched
}

static void test_sources(void) {
    _Alignas(8) uchar mixer[0x160] = {0}, channel[4][0x100] = {{0}};
    _Alignas(8) uchar input[4][0x100] = {{0}}, graph[4][0x20] = {{0}}, buffers[4][0x20] = {{0}};
    void *channels[] = {channel[0], channel[1], channel[2], channel[3]};
    float samples[0x54][8], output[4][2][8];
    float *io[0x52], *saved[0x52], *pairs[4][2];
    *(void ***)(mixer + 0x40) = channels;
    *(unsigned *)(mixer + 0x18) = 4;
    for (unsigned i = 0; i < 4; i++) {
        *(void **)(mixer + 0x9c + i * 4) = input[i];
        *(void **)(input[i] + 0x2c) = graph[i];
        *(void **)(graph[i] + 8) = buffers[i];
        *(float ***)(buffers[i] + 0x0c) = pairs[i];
        pairs[i][0] = output[i][0]; pairs[i][1] = output[i][1];
    }
    for (unsigned i = 0; i < 0x50; i++) {
        io[i] = samples[i];
        for (unsigned j = 0; j < 8; j++) samples[i][j] = (float)(i * 8 + j + 1);
    }
    *(unsigned *)((char *)io + 0x140) = 8;
    *(unsigned *)((char *)io + 0x144) = 0;
    memcpy(saved, io, sizeof saved);
    original_io = saved; raw_left = io[0x30]; raw_right = io[0x31];
    aux_process_native = fake_process;
    for (mask = 0; mask < 4; mask++) {
        channel[2][0x7a] = mask & 1; channel[3][0x7a] = !!(mask & 2);
        for (selection = -2; selection <= 2; selection++) {
            for (unsigned i = 0; i < sizeof output / sizeof(float); i++) ((float *)output)[i] = 123;
            aux_input_select = selection;
            unsigned before = calls;
            sclive_aux_process(mixer, io);
            assert(calls == before + 1 && !memcmp(saved, io, sizeof saved));
            for (unsigned i = 0; i < 0x50; i++)
                for (unsigned j = 0; j < 8; j++) assert(samples[i][j] == (float)(i * 8 + j + 1));
            if (!mask) for (unsigned i = 0; i < sizeof output / sizeof(float); i++) assert(((float *)output)[i] == 123);
        }
    }
    channel[2][0x7a] = 1; channel[3][0x7a] = 0;
    mask = 0; // every incomplete graph must call native with byte-identical inputs
    *(unsigned *)(mixer + 0x18) = 3;
    sclive_aux_process(mixer, io);
    *(unsigned *)(mixer + 0x18) = 4;
    *(void ***)(mixer + 0x40) = NULL;
    sclive_aux_process(mixer, io);
    *(void ***)(mixer + 0x40) = channels;
    channels[2] = NULL; sclive_aux_process(mixer, io); channels[2] = channel[2];
    channels[3] = NULL; sclive_aux_process(mixer, io); channels[3] = channel[3];
    for (unsigned i = 0; i < 4; i++) {
        *(void **)(mixer + 0x9c + i * 4) = NULL;
        sclive_aux_process(mixer, io);
        *(void **)(mixer + 0x9c + i * 4) = input[i];
        *(void **)(input[i] + 0x2c) = NULL;
        sclive_aux_process(mixer, io);
        *(void **)(input[i] + 0x2c) = graph[i];
        *(void **)(graph[i] + 8) = NULL;
        sclive_aux_process(mixer, io);
        *(void **)(graph[i] + 8) = buffers[i];
        *(float ***)(buffers[i] + 0x0c) = NULL;
        sclive_aux_process(mixer, io);
        *(float ***)(buffers[i] + 0x0c) = pairs[i];
        for (unsigned side = 0; side < 2; side++) {
            pairs[i][side] = NULL; sclive_aux_process(mixer, io); pairs[i][side] = output[i][side];
        }
    }
    assert(!memcmp(saved, io, sizeof saved));
    mask = 1; selection = aux_input_select = 1;
    for (unsigned side = 0; side < 2; side++) {
        io[0x30 + side] = NULL;
        memcpy(saved, io, sizeof saved);
        raw_left = io[0x30]; raw_right = io[0x31];
        sclive_aux_process(mixer, io);
        assert(!memcmp(saved, io, sizeof saved));
        io[0x30 + side] = samples[0x30 + side];
    }
    memcpy(saved, io, sizeof saved);
    raw_left = io[0x30]; raw_right = io[0x31];
    for (unsigned i = 0; i < 4; i++) pairs[i][1] = pairs[i][0];
    selection = aux_input_select = 0;
    sclive_aux_process(mixer, io); // a mono output pair may alias itself
    assert(!memcmp(saved, io, sizeof saved));
    puts("AUX source matrix incl. transition -2: MIC2 unchanged; AUX dry silence only in AUX mode; capture immutable; exact restore: PASS");
    puts("Incomplete graph: short channel count, null channel/vector/output/graph/buffer/pair/side fallbacks unchanged: PASS");
    puts("Missing AUX capture is denied; aliased stereo silence pairs are supported: PASS");
}

static void fake_control(void *c, void *p, uintptr_t a, uintptr_t b) {
    assert(aux_input_select == (expect_selecting ? -2 : 1) && p == (void *)0x1234 && a == 9 && b == 11);
    *(int *)((char *)c + 0x88) = selection;
}
static void test_selector(void) {
    _Alignas(8) uchar c[0xa0] = {0};
    aux_control_native = fake_control;
    *(void **)(c + 0x78) = (void *)0x1234;
    for (selection = -1; selection <= 2; selection++) {
        aux_input_select = 1;
        sclive_aux_changed(c, (void *)0x1234, 9, 11);
        assert(aux_input_select == (selection == 1));
    }
    expect_selecting = 0;
    *(void **)(c + 0x78) = (void *)0x4321;
    selection = aux_input_select = 1;
    sclive_aux_changed(c, (void *)0x1234, 9, 11);
    assert(aux_input_select == 1);
    puts("InputSelect: revoke before native event; publish Aux only after native update; invalid states denied, ON event uninterrupted: PASS");
}

static int transition_entered, transition_finish;
static void paused_control(void *c, void *p, uintptr_t a, uintptr_t b) {
    (void)p; (void)a; (void)b;
    assert(__atomic_load_n(&aux_input_select, __ATOMIC_ACQUIRE) == -2);
    __atomic_store_n(&transition_entered, 1, __ATOMIC_RELEASE);
    while (!__atomic_load_n(&transition_finish, __ATOMIC_ACQUIRE)) sched_yield();
    *(int *)((char *)c + 0x88) = 1;
}
static void *select_on_control_thread(void *c) {
    sclive_aux_changed(c, *(void **)((char *)c + 0x78), 0, 0);
    return NULL;
}
static void test_selector_threads(void) {
    _Alignas(8) uchar c[0xa0] = {0};
    *(void **)(c + 0x78) = (void *)0x1234;
    aux_input_select = 0; aux_control_native = paused_control;
    transition_entered = transition_finish = 0;
    pthread_t control;
    assert(!pthread_create(&control, NULL, select_on_control_thread, c));
    while (!__atomic_load_n(&transition_entered, __ATOMIC_ACQUIRE)) sched_yield();
    for (unsigned i = 0; i < 10000; i++)
        assert(__atomic_load_n(&aux_input_select, __ATOMIC_ACQUIRE) == -2);
    __atomic_store_n(&transition_finish, 1, __ATOMIC_RELEASE);
    assert(!pthread_join(control, NULL));
    assert(__atomic_load_n(&aux_input_select, __ATOMIC_ACQUIRE) == 1);
    puts("Concurrent selector transition: 10000 audio-side snapshots revoked until native update completes: PASS");
}

static void board(const char *s) {
    FILE *f = fopen(board_file, "wb"); assert(f);
    assert(fwrite(s, 1, strlen(s) + 1, f) == strlen(s) + 1 && !fclose(f));
}

static void test_image(const char *path, int sclive) {
    FILE *f = fopen(path, "rb"); assert(f);
    assert(!fseek(f, 0, SEEK_END)); size_t size = (size_t)ftell(f); rewind(f);
    size_t page = (size_t)sysconf(_SC_PAGESIZE), len = (size + page - 1) / page * page;
    uchar *image = mmap(NULL, len, PROT_READ | PROT_WRITE | PROT_EXEC, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(image != MAP_FAILED && fread(image, 1, size, f) == size); fclose(f);
    uchar *stock = malloc(size); assert(stock); memcpy(stock, image, size);
    ElfW(Ehdr) *h = (void *)image;
    assert(h->e_machine == EM_ARM && h->e_ident[EI_CLASS] == ELFCLASS32);
    struct dl_phdr_info info = {.dlpi_addr=(uintptr_t)image,
        .dlpi_phdr=(void *)(image + h->e_phoff), .dlpi_phnum=h->e_phnum};
    uintptr_t found = 0; engine_base(&info, sizeof info, &found);
    assert(found == (uintptr_t)image && !strcmp(build->name, sclive ? "5.3.0-sclive4" : "5.3.0"));
    uchar *id = memmem(image, size, build->id, 20); assert(id);
    *id ^= 1; found = 0; engine_base(&info, sizeof info, &found); assert(!found); *id ^= 1;
    unsigned sites = 0;
    for (unsigned i = 0; i < build->edit_count; i++) {
        const struct engine_edit *e = &build->edits[i];
        assert((size_t)e->offset + e->size <= size && edit_state((uintptr_t)image, e) == 1);
        if (e->group == EDIT_AUX) sites++;
    }
    assert(sites == (sclive ? 3u : 0u));
    const char *boards[] = {"JP21", "JP21X", "JP20", "JC11", "JC11S", "JP22", "JP23", "JP24", "JP07", "JP08", "JP11", "JP11S", "JP13", "JP14", "JC16", "NH08", "NH08S", "NH10", "RMZ2", ""};
    for (unsigned b = 0; b < sizeof boards / sizeof *boards; b++) {
        board(boards[b]);
        prepare_aux((uintptr_t)image, page);
        int enabled = sclive && !strcmp(boards[b], "JP21");
        assert(aux_ready == enabled);
        assert(!memcmp(image, stock, size)); // preparation never mutates any Engine byte
        prepare_config((uintptr_t)image, page);
        uint32_t ret = 0xe12fff1e;
        memcpy(image + build->config_site + 8, &ret, 4);
        __builtin___clear_cache((char *)image, (char *)image + len);
        uchar config[0x600] = {0}, expected[0x600] = {0};
        for (unsigned i = 0; i < 5; i++) expected[build->config_field[i]] = i == 4 ? 0 : 1;
        expected[0x54] = 1;
        uint32_t zoom = 30; memcpy(expected + build->config_zoom, &zoom, 4);
        if (enabled) expected[0x26] = expected[0x4d1] = 1;
        ((void (*)(void *))(uintptr_t)config_jump[1])(config);
        assert(!memcmp(config, expected, sizeof config));
        memcpy(image + build->config_site + 8, stock + build->config_site + 8, 4);
        munmap((void *)(uintptr_t)config_jump[1], page);
        if (enabled) { munmap((void *)aux_process_native, page); munmap((void *)aux_control_native, page); munmap((void *)aux_init_native, page); }
    }
    if (sclive) {
        board("JP21");
        for (unsigned i = 0; i < build->edit_count; i++) {
            const struct engine_edit *e = &build->edits[i];
            if (e->group != EDIT_AUX && e->offset != build->config_site) continue;
            image[e->offset] ^= 1; prepare_aux((uintptr_t)image, page);
            assert(!aux_ready && !aux_process_jump[1] && !aux_control_jump[1] && !aux_init_jump[1]);
            image[e->offset] ^= 1;
        }
        prepare_aux((uintptr_t)image, page); assert(aux_ready);
        // Execute the REAL displaced prologues, returning immediately afterwards.
        const uint32_t process_return[] = {0xe5801000, 0xe1cd40d0, 0xe1cd60d8, 0xe28dd024, 0xe12fff1e};
        const uint32_t control_return[] = {0xe580c000, 0xe1cd40d0, 0xe28dd018, 0xe12fff1e};
        const uint32_t init_return[] = {0xe5801000, 0xe1cd40d0, 0xe28dd024, 0xe12fff1e};
        memcpy(image + 0xcbd974, process_return, sizeof process_return);
        memcpy(image + 0xdba45c, control_return, sizeof control_return);
        memcpy(image + 0x1ba13c8, init_return, sizeof init_return);
        __builtin___clear_cache((char *)image, (char *)image + len);
        uintptr_t marker = 0;
        aux_process_native(&marker, (void *)0x1234); assert(marker == 0x1234);
        aux_control_native(&marker, NULL, 0, 0);
        uint32_t literal; memcpy(&literal, stock + 0xdba74c, 4); assert(marker == literal);
        assert(aux_init_native(&marker, (void *)0x5678, NULL, NULL, 0) == &marker && marker == 0x5678);
        munmap((void *)aux_process_native, page); munmap((void *)aux_control_native, page); munmap((void *)aux_init_native, page);
        puts("Real Engine: three A32 prologue trampolines incl. relocated PIC literal; mismatch/config refusal: PASS");
    }
    aux_ready = 0;
    printf("%s: exact BuildID, stock site bytes, JP21-only config, all other tested boards unchanged: PASS\n", build->name);
    free(stock); munmap(image, len);
}
int main(int argc, char **argv) {
    assert(argc == 3);
    char path[] = "/tmp/sclive-aux-board-XXXXXX";
    int fd = mkstemp(path); assert(fd >= 0); close(fd);
    board_file = product_code_path = path;
    test_image(argv[1], 0); test_image(argv[2], 1);
    test_startup(); test_sources(); test_selector(); test_selector_threads();
    unlink(path);
    return 0;
}
