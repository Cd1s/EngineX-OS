// LD_PRELOAD hook for /usr/Engine/Engine.
//
// Engine registers its QML through qRegisterResourceData. This hook swaps
// individual resource files for the ones in the active UI pack, so the UI can
// be changed without touching the read-only rootfs or the Engine binary.
// Removing the pack, or creating the safe-mode file, gives the stock UI back.
//
// Only libc is used. Static init runs before Qt is usable.
#define _GNU_SOURCE
#include <dlfcn.h>
#include <fcntl.h>
#include <link.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

#define UI_ROOT "/data/sc5000-custom/ui"
#define FLAG_COMPRESSED 0x01
#define FLAG_DIRECTORY 0x02
#define FLAG_ZSTD 0x04

typedef unsigned char uchar;
typedef int (*register_fn)(int, const uchar *, const uchar *, const uchar *);

static uint32_t be32(const uchar *p) { return (uint32_t)p[0] << 24 | p[1] << 16 | p[2] << 8 | p[3]; }
static uint16_t be16(const uchar *p) { return (uint16_t)(p[0] << 8 | p[1]); }
static void put32(uchar *p, uint32_t v) { p[0] = v >> 24; p[1] = v >> 16; p[2] = v >> 8; p[3] = v; }

struct walk {
    uchar *tree;
    const uchar *names, *data;
    size_t node_size;
    char pack[128];
    int replaced;
};

static int is_engine(void) {
    char exe[64];
    ssize_t n = readlink("/proc/self/exe", exe, sizeof exe - 1);
    if (n <= 0)
        return 0;
    exe[n] = 0;
    return strcmp(exe, "/usr/Engine/Engine") == 0;
}

// Edits to Engine in memory, the same as unlock_jp07_features.py makes offline, for
// a deck whose read-only root has the older binary. Each is only made if the bytes
// are as expected, so another Engine build is left alone.
struct engine_edit {
    uint32_t offset;
    const char *expected, *replacement;
    uint32_t size;
};

static const struct engine_edit engine_edits[] = {
    // TIDAL's client names a downloaded file after its codec and knows FLAC and AAC,
    // so an MP3 from the gateway would be saved with no extension and fail to load.
    // Its one "aac" string is both what it compares the codec with and the extension
    // it then uses. TIDAL's own AAC is labelled mp4a.40.x and keeps working.
    {0x1DB8E90u, "aac", "mp3", 4},
    // JP07's device config never sets hasBluetooth at +588, so Planck.bluetooth stays
    // null and the UI has no Bluetooth entry. add r2, r4, #592 -> strb r3, [r4, #588].
    {0xF65A50u, "\x25\x2e\x84\xe2", "\x4c\x32\xc4\xe5", 4},
    // That took the r2 this used: strh r3, [r2] -> strh r3, [r0, #-76]. r0 was just
    // set to r4 + 668, so this still writes +592. strh's immediate cannot reach it from r4.
    {0xF65A58u, "\xb0\x30\xc2\xe1", "\xbc\x34\x40\xe1", 4},
};

static int engine_base(struct dl_phdr_info *info, size_t size, void *out) {
    (void)size;
    *(uintptr_t *)out = info->dlpi_addr;
    return 1; // the first object is the program itself
}

__attribute__((constructor)) static void edit_engine(void) {
    if (!is_engine()) return;
    uintptr_t base = 0;
    dl_iterate_phdr(engine_base, &base);
    if (!base) return;
    long page = sysconf(_SC_PAGESIZE);
    for (size_t i = 0; i < sizeof engine_edits / sizeof *engine_edits; i++) {
        const struct engine_edit *e = &engine_edits[i];
        char *at = (char *)(base + e->offset);
        if (memcmp(at, e->expected, e->size)) continue;
        char *start = (char *)((uintptr_t)at & ~(uintptr_t)(page - 1));
        if (mprotect(start, (size_t)page, PROT_READ | PROT_WRITE)) continue;
        memcpy(at, e->replacement, e->size);
        mprotect(start, (size_t)page, PROT_READ | PROT_EXEC);
        __builtin___clear_cache(at, at + e->size);
    }
}

// Unknown or unsafe values fall back to the English pack.
static void pack_dir(char *out, size_t size) {
    char lang[16] = "en";
    FILE *f = fopen(UI_ROOT "/settings.conf", "r");
    if (f) {
        char line[64];
        while (fgets(line, sizeof line, f)) {
            if (strncmp(line, "lang=", 5))
                continue;
            line[strcspn(line, "\r\n")] = 0;
            if (!strcmp(line + 5, "zh-Hans") || !strcmp(line + 5, "zh-Hant"))
                strcpy(lang, line + 5);
        }
        fclose(f);
    }
    snprintf(out, size, UI_ROOT "/packs/%s", lang);
}

// Returns {be32 size, bytes} as rcc lays out an uncompressed file, or NULL.
static uchar *load_blob(const char *path) {
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0)
        return NULL;
    struct stat st;
    uchar *blob = NULL;
    if (fstat(fd, &st) == 0 && S_ISREG(st.st_mode) && st.st_size > 0 && st.st_size < 16 * 1024 * 1024 &&
        (blob = malloc((size_t)st.st_size + 4))) {
        put32(blob, (uint32_t)st.st_size);
        size_t got = 0;
        while (got < (size_t)st.st_size) {
            ssize_t n = read(fd, blob + 4 + got, (size_t)st.st_size - got);
            if (n <= 0) {
                free(blob);
                blob = NULL;
                break;
            }
            got += (size_t)n;
        }
    }
    close(fd);
    return blob;
}

// Node name in ASCII, or 0 for a name a pack could not use.
static int node_name(const struct walk *w, uint32_t offset, char *out, size_t size) {
    const uchar *p = w->names + offset;
    size_t len = be16(p);
    if (len == 0 || len + 1 > size)
        return 0;
    for (size_t i = 0; i < len; i++) {
        if (p[6 + i * 2] || p[7 + i * 2] < 0x20 || p[7 + i * 2] > 0x7e || p[7 + i * 2] == '/')
            return 0;
        out[i] = (char)p[7 + i * 2];
    }
    out[len] = 0;
    return 1;
}

static uint32_t max_node(const struct walk *w, uint32_t node) {
    const uchar *e = w->tree + node * w->node_size;
    uint32_t top = node;
    if (be16(e + 4) & FLAG_DIRECTORY) {
        uint32_t count = be32(e + 6), first = be32(e + 10);
        for (uint32_t i = 0; i < count; i++) {
            uint32_t m = max_node(w, first + i);
            if (m > top)
                top = m;
        }
    }
    return top;
}

static void apply(struct walk *w, uint32_t node, const char *prefix, int depth) {
    uchar *e = w->tree + node * w->node_size;
    uint16_t flags = be16(e + 4);
    char name[128], path[512];
    if (node == 0) {
        path[0] = 0;
    } else {
        if (!node_name(w, be32(e), name, sizeof name))
            return;
        if (snprintf(path, sizeof path, "%s/%s", prefix, name) >= (int)sizeof path)
            return;
    }
    if (flags & FLAG_DIRECTORY) {
        if (depth > 32)
            return;
        uint32_t count = be32(e + 6), first = be32(e + 10);
        for (uint32_t i = 0; i < count; i++)
            apply(w, first + i, path, depth + 1);
        return;
    }
    char file[700];
    snprintf(file, sizeof file, "%s%s", w->pack, path);
    uchar *blob = load_blob(file);
    if (!blob)
        return;
    // Qt adds the offset to the data pointer. On this 32-bit target it wraps,
    // so the blob can live anywhere in the address space.
    put32(e + 10, (uint32_t)((uintptr_t)blob - (uintptr_t)w->data));
    e[4] = (uchar)((flags & ~(FLAG_COMPRESSED | FLAG_ZSTD)) >> 8);
    e[5] = (uchar)(flags & ~(FLAG_COMPRESSED | FLAG_ZSTD));
    w->replaced++;
}

int _Z21qRegisterResourceDataiPKhS0_S0_(int version, const uchar *tree, const uchar *names, const uchar *data) {
    static register_fn real;
    static int active = -1;
    if (!real)
        real = (register_fn)dlsym(RTLD_NEXT, "_Z21qRegisterResourceDataiPKhS0_S0_");
    if (!real)
        return 0;
    if (active < 0)
        active = is_engine() && access(UI_ROOT "/safe-mode", F_OK) != 0;
    if (!active || version < 1 || version > 3 || sizeof(void *) != 4)
        return real(version, tree, names, data);

    struct walk w = {.tree = (uchar *)tree, .names = names, .data = data, .node_size = version >= 2 ? 22 : 14};
    pack_dir(w.pack, sizeof w.pack);
    size_t bytes = (size_t)(max_node(&w, 0) + 1) * w.node_size;
    uchar *copy = malloc(bytes);
    if (!copy)
        return real(version, tree, names, data);
    memcpy(copy, tree, bytes);
    w.tree = copy;
    apply(&w, 0, "", 0);
    if (!w.replaced) {
        free(copy);
        return real(version, tree, names, data);
    }
    fprintf(stderr, "engine-ui-hook: %s: %d files\n", w.pack, w.replaced);
    return real(version, copy, names, data);
}
