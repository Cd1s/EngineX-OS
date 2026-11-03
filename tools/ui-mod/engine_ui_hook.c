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
#include <pthread.h>
#include <sched.h>
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
    // Stems remote processor: exact Engine build from the verified patch manifest.
    {0x85ef9cu, "\xbc\x26\x9f\xe5\xbc\x36\x9f\xe5\xf4\x42\x6d\xe1\xf8\xa1\xcd\xe1\xf8\x60\xcd\xe1\xf0\x81\xcd\xe1\x20\xe0\x8d\xe5\x20\xb0\x8d\xe2\xe4\xd0\x4d\xe2\x02\x20\x8f\xe0\x3c\x10\x91\xe5\x00\x01\x0b\xe5\x94\x46\x9f\xe5\x03\x30\x92\xe7\x00\x00\x51\xe3\x04\x40\x8f",
        "\x10\x40\x2d\xe9\x00\x40\xa0\xe1\x1e\x10\xa0\xe3\x0c\x20\x8f\xe2\x04\xd5\xe3\xeb\x04\x00\xa0\xe1\x10\x80\xbd\xe8\x00\xf0\x20\xe3\x2f\x75\x73\x72\x2f\x6c\x6f\x63\x61\x6c\x2f\x62\x69\x6e\x2f\x73\x74\x65\x6d\x73\x2d\x70\x72\x6f\x63\x65\x73\x73\x6f\x72\x00", 63},
    {0x85f000u, "\x08\x30\x93\xe5\x33\xff\x2f\xe1\x5c\x16\x9f\xe5\x05\x20\xa0\xe1\x08\x00\xa0\xe1",
        "\x01\x30\xa0\xe3\x76\x32\xc0\xe5\x00\x20\x9f\xe5\x0b\x10\x1c\xea\x6c\x69\xa3\x01", 20},
    {0x85e5f8u, "\x90\xc7\x9f\xe5",
        "\x1e\xff\x2f\xe1", 4},
    {0xf6303cu, "\xe0\x20\x9f\xe5",
        "\xef\xef\xe3\xea", 4},
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
    long page_size = sysconf(_SC_PAGESIZE);
    if (page_size <= 0) return;
    uintptr_t page = (uintptr_t)page_size;
    // These four edits branch into each other; enable them only as a complete set.
    int stems_ready = 1;
    for (size_t i = 0; i < 4; i++) {
        const struct engine_edit *e = &engine_edits[i];
        if (memcmp((const void *)(base + e->offset), e->expected, e->size))
            stems_ready = 0;
    }
    for (size_t i = 0; i < sizeof engine_edits / sizeof *engine_edits; i++) {
        const struct engine_edit *e = &engine_edits[i];
        if (i < 4 && !stems_ready) continue;
        char *at = (char *)(base + e->offset);
        if (memcmp(at, e->expected, e->size)) continue;
        uintptr_t first = (uintptr_t)at - (uintptr_t)at % page;
        if (e->size > UINTPTR_MAX - (uintptr_t)at) continue;
        uintptr_t end = (uintptr_t)at + e->size;
        uintptr_t tail = end % page;
        if (tail && page - tail > UINTPTR_MAX - end) continue;
        if (tail) end += page - tail;
        char *start = (char *)first;
        size_t span = (size_t)(end - first);
        if (mprotect(start, span, PROT_READ | PROT_WRITE)) {
            // A partial Stems trampoline set must never reach Engine startup.
            if (i < 4) _exit(127);
            continue;
        }
        memcpy(at, e->replacement, e->size);
        if (mprotect(start, span, PROT_READ | PROT_EXEC)) {
            // Do not execute Engine code with an incomplete protection transition.
            static const char error[] = "engine-ui-hook: restoring RX protection failed\n";
            write(STDERR_FILENO, error, sizeof error - 1);
            _exit(127);
        }
        __builtin___clear_cache(at, at + e->size);
        fprintf(stderr, "engine-ui-hook: applied 0x%x (%u bytes)\n", e->offset, e->size);
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

// Engine's RenderStemsJob sets thread affinity to CPU cores that exist on 6/8-core models
// (e.g. CPU 4/5), but on the 4-core SC5000 (cores 0..3) pthread_setaffinity_np fails with
// EINVAL, causing Engine to call QMessageLogger::fatal("Failed setting CPU Affinity") and abort.
int pthread_setaffinity_np(pthread_t thread, size_t cpusetsize, const cpu_set_t *cpuset) {
    static int (*real_fn)(pthread_t, size_t, const cpu_set_t *);
    if (!real_fn)
        real_fn = (int (*)(pthread_t, size_t, const cpu_set_t *))dlsym(RTLD_NEXT, "pthread_setaffinity_np");
    if (!real_fn)
        return 0;
    int res = real_fn(thread, cpusetsize, cpuset);
    if (res != 0) {
        long n = sysconf(_SC_NPROCESSORS_ONLN);
        if (n <= 0) n = 4;
        cpu_set_t fallback;
        CPU_ZERO(&fallback);
        for (long i = 0; i < n; i++)
            CPU_SET((int)i, &fallback);
        res = real_fn(thread, sizeof fallback, &fallback);
        if (res != 0)
            return 0; // Prevent fatal abort
    }
    return 0;
}

int pthread_getaffinity_np(pthread_t thread, size_t cpusetsize, cpu_set_t *cpuset) {
    static int (*real_fn)(pthread_t, size_t, cpu_set_t *);
    if (!real_fn)
        real_fn = (int (*)(pthread_t, size_t, cpu_set_t *))dlsym(RTLD_NEXT, "pthread_getaffinity_np");
    if (!real_fn)
        return 0;
    int res = real_fn(thread, cpusetsize, cpuset);
    if (res != 0) {
        long n = sysconf(_SC_NPROCESSORS_ONLN);
        if (n <= 0) n = 4;
        CPU_ZERO(cpuset);
        for (long i = 0; i < n; i++)
            CPU_SET((int)i, cpuset);
        return 0;
    }
    return 0;
}

