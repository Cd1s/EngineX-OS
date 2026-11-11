// Native ARMv7 test using the stock 5.3.0 Engine as the anchor fixture.
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>

int main(int argc, char **argv) {
    assert(argc == 2);
    const size_t page = (size_t)sysconf(_SC_PAGESIZE), site = 0x16fbc38, size = site + 16;
    uchar *image = mmap(NULL, size, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(image != MAP_FAILED);
    int fd = open(argv[1], O_RDONLY);
    assert(fd >= 0 && pread(fd, image + site, 16, site) == 16);
    close(fd);
    for (unsigned i = 0; i < sizeof engine_builds / sizeof *engine_builds; i++)
        if (!strcmp(engine_builds[i].name, "5.3.0")) build = &engine_builds[i];
    assert(build);
    const struct engine_build *stock = build;
    prepare_shadow_sync_play((uintptr_t)image, page);
    assert(!shadow_sync_play_jump[1]);
    four_deck_pad_on = 1;
    build = &engine_builds[0];
    assert(build != stock);
    prepare_shadow_sync_play((uintptr_t)image, page);
    assert(!shadow_sync_play_jump[1]);
    build = stock;
    for (unsigned i = 0; i < 16; i += 4) {
        image[site + i] ^= 1;
        prepare_shadow_sync_play((uintptr_t)image, page);
        assert(!shadow_sync_play_jump[1]);
        image[site + i] ^= 1;
    }
    prepare_shadow_sync_play((uintptr_t)image, page);
    assert(shadow_sync_play_jump[1]);
    munmap((void *)(uintptr_t)shadow_sync_play_jump[1], page);

    // Save the output pointer, simulate stock's entry (one pushed word), run
    // the actual stub, then capture the native continuation's arguments and Z.
    uint32_t code[] = {
        0xe92d4010, 0xe1a04002, 0xe52db004, 0xe1a03000,
        0xe51ff004, 0,
        0, 0, // native ADD and CMP, taken directly from the fixture
        0xe10fc000, 0xe884100f, 0xe49db004, 0xe8bd8010,
    };
    uint32_t *entry = mmap(NULL, page, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(entry != MAP_FAILED);
    void *stub = shadow_sync_play_code((uintptr_t)(entry + 6), page);
    assert(stub);
    code[5] = (uint32_t)(uintptr_t)stub;
    memcpy(code + 6, image + site + 8, 8);
    memcpy(entry, code, sizeof code);
    assert(!mprotect(entry, page, PROT_READ | PROT_EXEC));
    __builtin___clear_cache((char *)entry, (char *)entry + sizeof code);
    typedef void (*test_fn)(uint32_t *, uint32_t **, uint32_t *);
    test_fn run = (test_fn)entry;

    char local[] = "player-one", same[] = "player-one", peer[] = "player-two";
    uint32_t host[] = {(uintptr_t)same, strlen(same)};
    uint32_t controller[20] = {[18] = (uintptr_t)local, [19] = strlen(local)};
    uint32_t *c = controller;
    uint32_t event[9] = {[3] = (uintptr_t)host | 1};
    unsigned checks = 0;
    const int routes[][2] = {{0,2}, {1,3}, {0,1}, {2,3}, {0,-1}, {1,-1}};
    for (unsigned r = 0; r < sizeof routes / sizeof *routes; r++) {
        four_deck_route[0] = routes[r][0]; four_deck_route[1] = routes[r][1];
        for (unsigned deck = 0; deck < 6; deck++) for (unsigned remote = 0; remote < 2; remote++)
            for (unsigned play = 0; play < 2; play++) {
                event[4] = deck; event[8] = play; host[0] = (uintptr_t)(remote ? peer : same);
                uint32_t out[5] = {0};
                unsigned want = play && (remote || deck >= 4 ||
                    (int)deck == routes[r][0] || (int)deck == routes[r][1]);
                run(event, &c, out);
                assert(out[0] == (uintptr_t)controller && out[1] == (uintptr_t)(event + 1));
                assert(out[2] == want && out[3] == (uintptr_t)event);
                assert(!!(out[4] & 0x40000000u) == !want);
                checks++;
            }
    }
    munmap(stub, page); munmap(entry, page); munmap(image, size);
    printf("shadow MASTER: %u native play/stop, host, slot and ABI cases; anchor/build/mode guards PASS\n", checks);
    return 0;
}
