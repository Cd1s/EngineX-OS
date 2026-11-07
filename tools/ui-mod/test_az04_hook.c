// AArch64 (AZ04: PRIME 4 G2, SYSTEM ONE) checks for engine_ui_hook.c, native on an arm64 host:
//   gcc -O2 -o test_az04_hook test_az04_hook.c -ldl -lpthread && ./test_az04_hook <AZ04 rootfs>/usr/Engine/Engine
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>

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

int main(int argc, char **argv) {
    assert(sizeof engine_builds / sizeof *engine_builds == 1);
    test_resources();
    for (int i = 1; i < argc; i++) test_engine(argv[i]);
    return 0;
}
