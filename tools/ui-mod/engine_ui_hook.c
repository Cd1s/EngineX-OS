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
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <link.h>
#include <netdb.h>
#include <pthread.h>
#include <sched.h>
#include <spawn.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <sys/mman.h>
#include <sys/prctl.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>

#define UI_ROOT "/data/sc5000-custom/ui"
#define FLAG_COMPRESSED 0x01
#define FLAG_DIRECTORY 0x02
#define FLAG_ZSTD 0x04

typedef unsigned char uchar;
typedef int (*register_fn)(int, const uchar *, const uchar *, const uchar *);
static cpu_set_t engine_cpus;
static int engine_cpus_ready;

static void initialize_engine_cpus(void) {
    long online = sysconf(_SC_NPROCESSORS_ONLN);
    if (online > 1) {
        CPU_ZERO(&engine_cpus);
        for (long cpu = 0; cpu < online && cpu < CPU_SETSIZE; cpu++) CPU_SET((int)cpu, &engine_cpus);
        engine_cpus_ready = 1;
    } else {
        engine_cpus_ready = sched_getaffinity(0, sizeof engine_cpus, &engine_cpus) == 0;
    }
}

static void *next_symbol(void **cache, const char *name) {
    void *fn = __atomic_load_n(cache, __ATOMIC_ACQUIRE);
    if (!fn) {
        fn = dlsym(RTLD_NEXT, name);
        if (fn) __atomic_store_n(cache, fn, __ATOMIC_RELEASE);
    }
    return fn;
}

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
#ifdef ENGINE_UI_HOOK_TEST
    extern int hook_test_engine;
    return hook_test_engine;
#else
    static int cached = -1;
    int value = __atomic_load_n(&cached, __ATOMIC_RELAXED);
    if (value >= 0) return value;
    char exe[64];
    ssize_t n = readlink("/proc/self/exe", exe, sizeof exe - 1);
    if (n <= 0)
        return 0;
    exe[n] = 0;
    value = strcmp(exe, "/usr/Engine/Engine") == 0;
    __atomic_store_n(&cached, value, __ATOMIC_RELAXED);
    return value;
#endif
}

static void *alsa_sw_cache, *alsa_tstamp_set_cache, *alsa_tstamp_get_cache;

int snd_pcm_sw_params_set_tstamp_type(void *pcm, void *params, int type) {
    int (*real)(void *, void *, int) = next_symbol(&alsa_tstamp_set_cache, "snd_pcm_sw_params_set_tstamp_type");
    if (!real) return -ENOSYS;
    // Engine already requests RAW in ALSADevice; never change an explicit monotonic clock.
    if (is_engine() && type == 0) type = 2; // GETTIMEOFDAY -> MONOTONIC_RAW
    return real(pcm, params, type);
}

int snd_pcm_sw_params(void *pcm, void *params) {
    int (*real)(void *, void *) = next_symbol(&alsa_sw_cache, "snd_pcm_sw_params");
    if (!real) return -ENOSYS;
    if (is_engine()) {
        int (*get)(const void *, int *) = next_symbol(&alsa_tstamp_get_cache, "snd_pcm_sw_params_get_tstamp_type");
        int type;
        if (get && !get(params, &type) && type == 0) {
            int rc = snd_pcm_sw_params_set_tstamp_type(pcm, params, 2);
            if (rc < 0) return rc;
        }
    }
    return real(pcm, params);
}

static int official_update_host(const char *node) {
    static const char host[] = "autoupdate.airmusictech.com";
    if (!node) return 0;
    size_t size = strlen(node);
    if (size && node[size - 1] == '.') size--;
    return size == sizeof host - 1 && !strncasecmp(node, host, size);
}

static void *getaddrinfo_cache;

int getaddrinfo(const char *node, const char *service, const struct addrinfo *hints, struct addrinfo **result) {
    // device.inmusicbrands.com also serves OAuth/unlock; only deny the firmware-update host.
    if (is_engine() && official_update_host(node)) {
        if (result) *result = NULL;
        return EAI_NONAME;
    }
    int (*real)(const char *, const char *, const struct addrinfo *, struct addrinfo **) =
        next_symbol(&getaddrinfo_cache, "getaddrinfo");
    if (real) return real(node, service, hints, result);
    errno = ENOSYS;
    return EAI_SYSTEM;
}

// Edits to Engine in memory, the same as unlock_jp07_features.py makes offline, for
// a deck whose read-only root has the older binary. Each is only made if the bytes
// are as expected, so another Engine build is left alone.
struct engine_edit {
    uint32_t offset;
    const char *expected, *replacement;
    uint32_t size;
};

static uint32_t stems_feature_jump[2];
static uint32_t config_jump[2];

static void *rx_code(const void *code, size_t size, size_t page) {
    void *stub = mmap(NULL, page, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (stub == MAP_FAILED) _exit(127);
    memcpy(stub, code, size);
    if (mprotect(stub, page, PROT_READ | PROT_EXEC)) _exit(127);
    __builtin___clear_cache(stub, (char *)stub + size);
    return stub;
}

static void prepare_config(uintptr_t base, size_t page) {
    // getConfig's common return path runs after every model's constructor.
    uint32_t code[] = {
        0xe3a02001, // mov r2, #1
        0xe5c020db, // hasStems integration
        0xe5c0224c, // hasBluetooth
        0xe5c0224e, // hasBluetoothSendToAssignment
        0xe5c02276, // remote Stems processor support
        0xe3a02000, // mov r2, #0
        0xe5c02289, // disableHfsPlus
        0xe59f2004, // replay the two displaced literal loads
        0xe59f3004,
        0xe59ff004,
        *(const uint32_t *)(base + 0xf63124),
        *(const uint32_t *)(base + 0xf63120),
        (uint32_t)(base + 0xf63044),
    };
    config_jump[0] = 0xe51ff004;
    config_jump[1] = (uint32_t)(uintptr_t)rx_code(code, sizeof code, page);
}

static void prepare_stems_feature(uintptr_t base, size_t page) {
    // The old 0x8429b0 exit misses models configured False; intercept before that split.
    uint32_t code[] = {
        0xe59f3018, // ldr r3, [pc, #24] (Stems feature object)
        0xe1570003, // cmp r7, r3
        0x059f3014, // ldreq r3, [pc, #20] (native true byte)
        0x05873004, // streq r3, [r7, #4]
        0x059ff010, // ldreq pc, [pc, #16] (native enabled-feature registration)
        0xe51b3340, // ldr r3, [fp, #-832] (displaced instruction)
        0xe3540000, // cmp r4, #0 (displaced flags)
        0xe59ff008, // ldr pc, [pc, #8] (original store/branch)
        (uint32_t)(base + 0x29bc83c),
        (uint32_t)(base + 0x258e185),
        (uint32_t)(base + 0x8429b0),
        (uint32_t)(base + 0x84264c),
    };
    stems_feature_jump[0] = 0xe51ff004; // ldr pc, [pc, #-4]
    stems_feature_jump[1] = (uint32_t)(uintptr_t)rx_code(code, sizeof code, page);
}

static const struct engine_edit engine_edits[] = {
    // Stems remote processor: exact Engine build from the verified patch manifest.
    {0x85ef9cu, "\xbc\x26\x9f\xe5\xbc\x36\x9f\xe5\xf4\x42\x6d\xe1\xf8\xa1\xcd\xe1\xf8\x60\xcd\xe1\xf0\x81\xcd\xe1\x20\xe0\x8d\xe5\x20\xb0\x8d\xe2\xe4\xd0\x4d\xe2\x02\x20\x8f\xe0\x3c\x10\x91\xe5\x00\x01\x0b\xe5\x94\x46\x9f\xe5\x03\x30\x92\xe7\x00\x00\x51\xe3\x04\x40\x8f",
        "\x10\x40\x2d\xe9\x00\x40\xa0\xe1\x1e\x10\xa0\xe3\x0c\x20\x8f\xe2\x04\xd5\xe3\xeb\x04\x00\xa0\xe1\x10\x80\xbd\xe8\x00\xf0\x20\xe3\x2f\x75\x73\x72\x2f\x6c\x6f\x63\x61\x6c\x2f\x62\x69\x6e\x2f\x73\x74\x65\x6d\x73\x2d\x70\x72\x6f\x63\x65\x73\x73\x6f\x72\x00", 63},
    {0x85e5f8u, "\x90\xc7\x9f\xe5",
        "\x1e\xff\x2f\xe1", 4},
    {0xf6303cu, "\xe0\x20\x9f\xe5\xd8\x30\x9f\xe5", (const char *)config_jump, 8},
    // TIDAL's client names a downloaded file after its codec and knows FLAC and AAC,
    // so an MP3 from the gateway would be saved with no extension and fail to load.
    // Its one "aac" string is both what it compares the codec with and the extension
    // it then uses. TIDAL's own AAC is labelled mp4a.40.x and keeps working.
    {0x1DB8E90u, "aac", "mp3", 4},
    // Retain the pre-existing +0x24 capability patch.
    {0xF659B8u, "\x24\x30\xc4\xe5", "\x00\xf0\x20\xe3", 4},
    {0x842644u, "\x40\x33\x1b\xe5\x00\x00\x54\xe3", (const char *)stems_feature_jump, 8},
    // Preserve ShowStemsList eligibility in r8 across the extra device feature gate.
    {0x6D3E00u, "\x07\x00\x00\x0a", "\x07\x00\x00\xea", 4},
    // unknown filesystem -> supported
    {0x1897e70u, "\x00\x60\xa0\xe3", "\x04\x60\xa0\xe3", 4},
    // isSupportedFilesystem always true
    {0x1897dccu, "\x00\x40\xa0\x03", "\x01\x40\xa0\x03", 4},
    // hfsplus not rewritten to ufsd
    {0x1678644u, "\xd1\x01\x00\x1a", "\x00\xf0\x20\xe3", 4},
    // TidalAPI base URL -> stream-sync /v1/
    {0x336740u, "\x69\x75\x75\x71\x74\x3b\x30\x30\x62\x71\x6a\x2f\x75\x6a\x65\x62\x6d\x2f\x64\x70\x6e\x30\x77\x32", "\x69\x75\x75\x71\x3b\x30\x30\x32\x33\x38\x2f\x31\x2f\x31\x2f\x32\x3b\x39\x31\x39\x31\x30\x77\x32", 24},
    // TidalAPI auth URL -> stream-sync /au//
    {0x336758u, "\x6f\x7b\x7b\x77\x7a\x41\x36\x36\x68\x7c\x7b\x6f\x35\x7b\x70\x6b\x68\x73\x35\x6a\x76\x74\x36\x7d", "\x6f\x7b\x7b\x77\x41\x36\x36\x38\x39\x3e\x35\x37\x35\x37\x35\x38\x41\x3f\x37\x3f\x37\x36\x68\x7c", 24},
    // TidalAPI auth URL suffix 1/ -> //
    {0x33652cu, "\x38\x36\x03\xe3", "\x36\x36\x03\xe3", 4},
    // read-only volume not auto-ejected
    {0x58af34u, "\x30\x32\xd0\xe5", "\x00\x30\xa0\xe3", 4},
    // DeviceManager::isReadOnly cache -> false
    {0x167295cu, "\x1c\x00\xd4\xe5", "\x00\x00\xa0\xe3", 4},
    // DeviceManager::isReadOnly QFileInfo -> false
    {0x16728c0u, "\x01\x00\x24\xe2", "\x00\x00\xa0\xe3", 4},
};

static int engine_base(struct dl_phdr_info *info, size_t size, void *out) {
    (void)size;
    static const uchar id[] = "\xaf\x07\x38\x65\xa3\x31\x8d\x80\x7a\x83\x03\xf7\x16\x15\x2e\xac\x92\x3a\x4c\x6f";
    for (size_t i = 0; i < info->dlpi_phnum; i++) {
        const ElfW(Phdr) *p = &info->dlpi_phdr[i];
        if (p->p_type != PT_NOTE) continue;
        const uchar *at = (const uchar *)(info->dlpi_addr + p->p_vaddr), *end = at + p->p_memsz;
        while ((size_t)(end - at) >= sizeof(ElfW(Nhdr))) {
            const ElfW(Nhdr) *n = (const void *)at;
            size_t names = ((size_t)n->n_namesz + 3) & ~(size_t)3;
            size_t desc = ((size_t)n->n_descsz + 3) & ~(size_t)3;
            at += sizeof *n;
            if (names > (size_t)(end - at) || desc > (size_t)(end - at) - names) break;
            if (n->n_type == NT_GNU_BUILD_ID && n->n_namesz == 4 && n->n_descsz == 20 &&
                !memcmp(at, "GNU", 4) && !memcmp(at + names, id, 20))
                *(uintptr_t *)out = info->dlpi_addr;
            at += names + desc;
        }
    }
    return 1; // the first object is the program itself
}

static int edit_state(uintptr_t base, const struct engine_edit *e) {
    const void *at = (const void *)(base + e->offset);
    if (!memcmp(at, e->replacement, e->size)) return 2;
    if (!memcmp(at, e->expected, e->size)) return 1;
    // Upgrade the previous processor-only getConfig branch without mixing code paths.
    if (e->offset == 0xf6303c && !memcmp(at, "\xef\xef\xe3\xea\xd8\x30\x9f\xe5", 8)) return 1;
    return 0;
}

static void install_sqlite_hooks(uintptr_t base, size_t page);

static int gateway_site(uint32_t offset) {
    return offset == 0x336740 || offset == 0x336758 || offset == 0x33652c;
}

__attribute__((constructor)) static void edit_engine(void) {
    if (!is_engine()) return;
    initialize_engine_cpus();
    uintptr_t base = 0;
    dl_iterate_phdr(engine_base, &base);
    if (!base) return;
    long page_size = sysconf(_SC_PAGESIZE);
    if (page_size <= 0) return;
    uintptr_t page = (uintptr_t)page_size;
    prepare_config(base, page);
    if (!memcmp((const void *)(base + 0x842644), "\x40\x33\x1b\xe5\x00\x00\x54\xe3", 8))
        prepare_stems_feature(base, page);
    // The processor edits form one unit; accept both stock and already patched sites.
    int stems_ready = 1;
    for (size_t i = 0; i < 3; i++) {
        const struct engine_edit *e = &engine_edits[i];
        if (!edit_state(base, e)) stems_ready = 0;
    }
    int gateway_ready = 1;
    for (size_t i = 0; i < sizeof engine_edits / sizeof *engine_edits; i++)
        if (gateway_site(engine_edits[i].offset) && !edit_state(base, &engine_edits[i])) gateway_ready = 0;
    for (size_t i = 0; i < sizeof engine_edits / sizeof *engine_edits; i++) {
        const struct engine_edit *e = &engine_edits[i];
        if (i < 3 && !stems_ready) continue;
        if (gateway_site(e->offset) && !gateway_ready) continue;
        char *at = (char *)(base + e->offset);
        if (edit_state(base, e) != 1) continue;
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
            _exit(127);
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
    install_sqlite_hooks(base, page);
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
    static void *cache;
    static int active = -1;
    register_fn real = next_symbol(&cache, "_Z21qRegisterResourceDataiPKhS0_S0_");
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

static int thread_class(const char *name) {
    static const char *idle[] = {"Analysis Queue", "OfflineAnalyzer", "Dropbox", "Beatport",
        "J:I:Takingfreez", "Taking freeze", "Freeze Checker", "Collecting anal", "sentry", "CrashHandler"};
    for (size_t i = 0; i < sizeof idle / sizeof *idle; i++)
        if (!strncmp(name, idle[i], strlen(idle[i]))) return 2;
    return !strncmp(name, "QQuickPixmapRea", 15) || !strncmp(name, "FileInfoThread", 14);
}

static int guard_affinity(pthread_t thread, int kind) {
    static void *get_cache, *set_cache;
    int (*get)(pthread_t, size_t, cpu_set_t *) = next_symbol(&get_cache, "pthread_getaffinity_np");
    int (*set)(pthread_t, size_t, const cpu_set_t *) = next_symbol(&set_cache, "pthread_setaffinity_np");
    if (!get || !set) return ENOSYS;
    cpu_set_t allowed;
    if (engine_cpus_ready) allowed = engine_cpus;
    else {
        int rc = get(thread, sizeof allowed, &allowed);
        if (rc) return rc;
    }
    CPU_CLR(0, &allowed);
    if (!CPU_COUNT(&allowed)) return EINVAL; // A single-core target cannot avoid CPU0.
    if (kind == 2) {
        int last = CPU_SETSIZE - 1;
        while (!CPU_ISSET(last, &allowed)) last--;
        CPU_ZERO(&allowed);
        CPU_SET(last, &allowed);
    }
    return set(thread, sizeof allowed, &allowed);
}

static void lower_io_nice(pid_t tid, int nice, int idle_io) {
    setpriority(PRIO_PROCESS, (id_t)tid, nice);
    if (idle_io) syscall(SYS_ioprio_set, 1, tid, 3 << 13);
}

static void guard_named_thread(pthread_t thread, const char *name) {
    if (!is_engine()) return;
    int kind = thread_class(name);
    if (!kind) return;
    int saved_errno = errno;
    struct sched_param param = {.sched_priority = 0};
    if (kind == 2) pthread_setschedparam(thread, SCHED_IDLE, &param);
    guard_affinity(thread, kind);
    if (pthread_equal(thread, pthread_self())) {
        lower_io_nice((pid_t)syscall(SYS_gettid), kind == 2 ? 19 : 10, kind == 2);
    } else {
        // Naming another pthread must not lower the caller (often Engine's main thread).
        // Equal kernel names have the same policy, so duplicate worker names are safe.
        DIR *tasks = opendir("/proc/self/task");
        struct dirent *entry;
        while (tasks && (entry = readdir(tasks))) {
            char *end;
            long tid = strtol(entry->d_name, &end, 10);
            if (*end || tid <= 0) continue;
            char path[80], comm[32];
            snprintf(path, sizeof path, "/proc/self/task/%ld/comm", tid);
            FILE *f = fopen(path, "r");
            if (!f) continue;
            int match = fgets(comm, sizeof comm, f) &&
                (comm[strcspn(comm, "\n")] = 0, !strncmp(comm, name, 15));
            fclose(f);
            if (match) lower_io_nice((pid_t)tid, kind == 2 ? 19 : 10, kind == 2);
        }
        if (tasks) closedir(tasks);
    }
    errno = saved_errno;
}

int pthread_setname_np(pthread_t thread, const char *name) {
    static void *cache;
    int (*real)(pthread_t, const char *) = next_symbol(&cache, "pthread_setname_np");
    if (!real) return ENOSYS;
    int rc = real(thread, name);
    if (!rc) guard_named_thread(thread, name);
    return rc;
}

static int named_prctl(void **cache, const char *symbol, int option, va_list ap) {
    int (*real)(int, ...) = next_symbol(cache, symbol);
    if (!real) { errno = ENOSYS; return -1; }
    // prctl's libc wrapper forwards four machine-word arguments to the kernel.
    unsigned long a2 = va_arg(ap, unsigned long), a3 = va_arg(ap, unsigned long);
    unsigned long a4 = va_arg(ap, unsigned long), a5 = va_arg(ap, unsigned long);
    int rc = real(option, a2, a3, a4, a5);
    if (!rc && option == PR_SET_NAME) guard_named_thread(pthread_self(), (const char *)a2);
    return rc;
}

int hook_prctl(int, ...) __asm__("prctl");
int hook_prctl_time64(int, ...) __asm__("__prctl_time64");
#define PRCTL_WRAPPER(fn, symbol) \
int fn(int option, ...) { \
    static void *cache; va_list ap; va_start(ap, option); \
    int rc = named_prctl(&cache, symbol, option, ap); va_end(ap); return rc; \
}
PRCTL_WRAPPER(hook_prctl, "prctl")
PRCTL_WRAPPER(hook_prctl_time64, "__prctl_time64")

int pthread_setschedparam(pthread_t thread, int policy, const struct sched_param *param) {
    static void *cache;
    int (*real)(pthread_t, int, const struct sched_param *) = next_symbol(&cache, "pthread_setschedparam");
    if (!real) return ENOSYS;
    char name[16];
    struct sched_param idle = {.sched_priority = 0};
    if (is_engine() && !pthread_getname_np(thread, name, sizeof name) && thread_class(name) == 2) {
        policy = SCHED_IDLE;
        param = &idle;
    }
    return real(thread, policy, param);
}

static int analytics_db(const char *name) {
    return name && (strstr(name, "posthog.db") || strstr(name, "events.db"));
}

typedef int (*sqlite_open_v2_fn)(const char *, void **, int, const char *);
typedef int (*sqlite_open_fn)(const char *, void **);
typedef int (*sqlite_exec_fn)(void *, const char *, int (*)(void *, int, char **, char **), void *, char **);
static sqlite_open_v2_fn sqlite_open_v2_original;
static sqlite_open_fn sqlite_open_original;
static sqlite_exec_fn sqlite_exec_original;

static void tune_database(int rc, const char *filename, void **db) {
    if (rc || !db || !*db || (filename && !strcmp(filename, ":memory:"))) return;
    sqlite_exec_original(*db, "PRAGMA synchronous = NORMAL; PRAGMA temp_store = MEMORY; PRAGMA cache_size = -8000;",
                         NULL, NULL, NULL);
}

static int __attribute__((used)) tuned_sqlite_open_v2(const char *filename, void **db, int flags, const char *vfs) {
    const char *target = analytics_db(filename) ? ":memory:" : filename;
    int rc = sqlite_open_v2_original(target, db, flags, vfs);
    tune_database(rc, target, db);
    return rc;
}

static int __attribute__((used)) tuned_sqlite_open(const char *filename, void **db) {
    const char *target = analytics_db(filename) ? ":memory:" : filename;
    int rc = sqlite_open_original(target, db);
    tune_database(rc, target, db);
    return rc;
}

static void *function_trampoline(uintptr_t entry, size_t page) {
    uint32_t code[4];
    memcpy(code, (const void *)entry, 8);
    code[2] = 0xe51ff004;
    code[3] = (uint32_t)(entry + 8);
    return rx_code(code, sizeof code, page);
}

static void redirect_function(uintptr_t entry, const void *handler, size_t page) {
    uint32_t jump[] = {0xe51ff004, (uint32_t)(uintptr_t)handler};
    uintptr_t first = entry - entry % page;
    size_t span = ((entry + sizeof jump + page - 1) / page * page) - first;
    if (mprotect((void *)first, span, PROT_READ | PROT_WRITE)) _exit(127);
    memcpy((void *)entry, jump, sizeof jump);
    if (mprotect((void *)first, span, PROT_READ | PROT_EXEC)) _exit(127);
    __builtin___clear_cache((char *)entry, (char *)entry + sizeof jump);
}

static void install_sqlite_hooks(uintptr_t base, size_t page) {
    // Identified through sqlite3_api_routines slots 70/135/55 and ARM disassembly.
    if (memcmp((const void *)(base + 0x1cba858), "\x04\xb0\x2d\xe5\x00\x30\xa0\xe3", 8) ||
        memcmp((const void *)(base + 0x1cba874), "\x04\xb0\x2d\xe5\x00\xb0\x8d\xe2", 8) ||
        memcmp((const void *)(base + 0x1c91f74), "\xf4\x42\x6d\xe1\x02\x50\xa0\xe1", 8)) return;
    const uint32_t *api = (const void *)(base + 0x29980a4);
    if (api[70] != base + 0x1cba858 || api[135] != base + 0x1cba874 || api[55] != base + 0x1c91f74) return;
    sqlite_exec_original = (sqlite_exec_fn)(base + 0x1c91f74);
    sqlite_open_original = (sqlite_open_fn)function_trampoline(base + 0x1cba858, page);
    sqlite_open_v2_original = (sqlite_open_v2_fn)function_trampoline(base + 0x1cba874, page);
    redirect_function(base + 0x1cba858, tuned_sqlite_open, page);
    redirect_function(base + 0x1cba874, tuned_sqlite_open_v2, page);
    fprintf(stderr, "engine-ui-hook: SQLite open/open_v2 hooks installed\n");
}

static const char *volatile_path(int dirfd, const char *path, char out[PATH_MAX]) {
    if (!path || !is_engine()) return path;
    char full[PATH_MAX];
    if (*path == '/') {
        if (strlen(path) >= sizeof full) { errno = ENAMETOOLONG; return NULL; }
        strcpy(full, path);
    } else {
        char dir[PATH_MAX];
        if (dirfd == AT_FDCWD) {
            if (!getcwd(dir, sizeof dir)) return path;
        } else {
            char fdpath[64];
            snprintf(fdpath, sizeof fdpath, "/proc/self/fd/%d", dirfd);
            ssize_t n = readlink(fdpath, dir, sizeof dir - 1);
            if (n < 0) return path;
            dir[n] = 0;
        }
        if (snprintf(full, sizeof full, "%s/%s", dir, path) >= (int)sizeof full) {
            errno = ENAMETOOLONG; return NULL;
        }
    }
    if (!strstr(full, "/analytics/posthog.db") && !strstr(full, "/analytics/events.db") &&
        !strstr(full, "/CrashReportDB/")) return path;
    if (strstr(full, "/../") || !strcmp(full + strlen(full) - 3, "/..")) {
        errno = EINVAL; return NULL;
    }
    char root[80];
    snprintf(root, sizeof root, "/tmp/sc5000-engine-volatile-%u", (unsigned)geteuid());
    if (!strncmp(full, root, strlen(root)) && full[strlen(root)] == '/') return path;
    if (mkdir(root, 0700) && errno != EEXIST) return NULL;
    struct stat st;
    if (lstat(root, &st) || !S_ISDIR(st.st_mode) || st.st_uid != geteuid() || (st.st_mode & 077)) {
        errno = EACCES; return NULL;
    }
    if (snprintf(out, PATH_MAX, "%s%s", root, full) >= PATH_MAX) {
        errno = ENAMETOOLONG; return NULL;
    }
    for (char *p = out + strlen(root) + 1; *p; p++) {
        if (*p != '/') continue;
        *p = 0;
        int failed = mkdir(out, 0700) && errno != EEXIST;
        *p = '/';
        if (failed) return NULL;
    }
    return out;
}

static int redirected_open(int dirfd, const char *path, int flags, mode_t mode) {
    static void *cache;
    int (*real)(int, const char *, int, ...) = next_symbol(&cache, "openat");
    char mapped[PATH_MAX];
    const char *target = volatile_path(dirfd, path, mapped);
    if (!target) return -1;
    if (target == mapped) dirfd = AT_FDCWD;
    return real ? real(dirfd, target, flags, mode) : (int)syscall(SYS_openat, dirfd, target, flags, mode);
}

#define OPEN_MODE(flags, ap) (((flags) & O_CREAT) || (((flags) & O_TMPFILE) == O_TMPFILE) ? va_arg(ap, mode_t) : 0)
#define OPEN_WRAPPER(fn, large) \
int fn(const char *path, int flags, ...) { \
    va_list ap; va_start(ap, flags); mode_t mode = OPEN_MODE(flags, ap); va_end(ap); \
    return redirected_open(AT_FDCWD, path, flags | (large), mode); \
}
#define OPENAT_WRAPPER(fn, large) \
int fn(int dirfd, const char *path, int flags, ...) { \
    va_list ap; va_start(ap, flags); mode_t mode = OPEN_MODE(flags, ap); va_end(ap); \
    return redirected_open(dirfd, path, flags | (large), mode); \
}
// Explicit assembler names also export both variants on Debian's default time64 ABI.
int hook_open(const char *, int, ...) __asm__("open");
int hook_open64(const char *, int, ...) __asm__("open64");
int hook_openat(int, const char *, int, ...) __asm__("openat");
int hook_openat64(int, const char *, int, ...) __asm__("openat64");
OPEN_WRAPPER(hook_open, 0)
OPEN_WRAPPER(hook_open64, O_LARGEFILE)
OPENAT_WRAPPER(hook_openat, 0)
OPENAT_WRAPPER(hook_openat64, O_LARGEFILE)

static int crashpad(const char *file) {
    const char *base = file ? strrchr(file, '/') : NULL;
    return file && !strcmp(base ? base + 1 : file, "crashpad_handler");
}

int posix_spawn(pid_t *pid, const char *path, const posix_spawn_file_actions_t *actions,
                const posix_spawnattr_t *attr, char *const argv[], char *const envp[]) {
    static void *cache;
    if (is_engine() && crashpad(path)) return ENOENT; // Never return success with a fabricated PID.
    int (*real)(pid_t *, const char *, const posix_spawn_file_actions_t *,
                const posix_spawnattr_t *, char *const [], char *const []) = next_symbol(&cache, "posix_spawn");
    return real ? real(pid, path, actions, attr, argv, envp) : ENOSYS;
}

int execve(const char *path, char *const argv[], char *const envp[]) {
    static void *cache;
    if (is_engine() && crashpad(path)) { errno = ENOENT; return -1; }
    int (*real)(const char *, char *const [], char *const []) = next_symbol(&cache, "execve");
    if (real) return real(path, argv, envp);
    errno = ENOSYS; return -1;
}

int execv(const char *path, char *const argv[]) {
    extern char **environ;
    return execve(path, argv, environ);
}

// Engine's RenderStemsJob sets thread affinity to CPU cores that exist on 6/8-core models
// (e.g. CPU 4/5), but on the 4-core SC5000 (cores 0..3) pthread_setaffinity_np fails with
// EINVAL, causing Engine to call QMessageLogger::fatal("Failed setting CPU Affinity") and abort.
int pthread_setaffinity_np(pthread_t thread, size_t cpusetsize, const cpu_set_t *cpuset) {
    static void *cache;
    int (*real_fn)(pthread_t, size_t, const cpu_set_t *) = next_symbol(&cache, "pthread_setaffinity_np");
    if (!real_fn) return ENOSYS;
    int res = real_fn(thread, cpusetsize, cpuset);
    if (!is_engine()) return res;
    if (res == EINVAL) {
        cpu_set_t fallback;
        if (engine_cpus_ready) fallback = engine_cpus;
        else if (sched_getaffinity(0, sizeof fallback, &fallback)) return res;
        res = real_fn(thread, sizeof fallback, &fallback);
    }
    char name[16];
    if (!res && !pthread_getname_np(thread, name, sizeof name)) {
        int kind = thread_class(name);
        if (kind) res = guard_affinity(thread, kind);
    }
    return res;
}

int pthread_getaffinity_np(pthread_t thread, size_t cpusetsize, cpu_set_t *cpuset) {
    static void *cache;
    int (*real_fn)(pthread_t, size_t, cpu_set_t *) = next_symbol(&cache, "pthread_getaffinity_np");
    if (!real_fn) return ENOSYS;
    int res = real_fn(thread, cpusetsize, cpuset);
    return res;
}

