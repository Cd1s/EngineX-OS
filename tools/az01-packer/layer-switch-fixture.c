/* Sandbox only (layer-switch-sandbox.sh): logs each eglfs buffer swap with its wall-clock
 * time, and on the SC LIVE 4 Engine stubs the SoC identity check (else it quits to TestApp).
 * LSW_PROFILE=1: samples where the GUI thread (EMain) runs, every 0.5 ms of its CPU time,
 * as "<wall ms> 0x<ip>" lines for tools/ui-mod/summarize_perf.py.
 * arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror -shared -fPIC -o layer-switch-fixture.so layer-switch-fixture.c -ldl -lpthread */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <elf.h>
#include <fcntl.h>
#include <link.h>
#include <linux/perf_event.h>
#include <pthread.h>
#include <stdlib.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/syscall.h>
#include <sys/time.h>
#include <time.h>
#include <unistd.h>

static int swaps = -1;
void _ZN19QEGLPlatformContext11swapBuffersEP16QPlatformSurface(void *ctx, void *surface) {
    static void (*next)(void *, void *);
    if (!next) next = dlsym(RTLD_NEXT, "_ZN19QEGLPlatformContext11swapBuffersEP16QPlatformSurface");
    next(ctx, surface);
    if (swaps < 0) swaps = open("/tmp/swaps.log", O_WRONLY | O_CREAT | O_APPEND | O_CLOEXEC, 0644);
    struct timeval tv;
    gettimeofday(&tv, NULL);
    char line[32];
    int n = snprintf(line, sizeof line, "%lld\n", (long long)tv.tv_sec * 1000000 + tv.tv_usec);
    if (swaps >= 0 && n > 0) (void)!write(swaps, line, (size_t)n);
}

static uintptr_t base;
static int identify(struct dl_phdr_info *i, size_t size, void *unused) {
    (void)size; (void)unused;
    static const unsigned char id[20] = {0xb9,0x46,0xc3,0xb8,0xd9,0x9d,0x32,0x99,0x8c,0x81,0x97,0xd5,0xdc,0x90,0x5a,0x6b,0xdd,0xaa,0x5d,0x2f};
    for (unsigned p = 0; p < i->dlpi_phnum; p++) {
        const ElfW(Phdr) *h = i->dlpi_phdr + p;
        if (h->p_type != PT_NOTE) continue;
        const unsigned char *n = (void *)(i->dlpi_addr + h->p_vaddr), *end = n + h->p_memsz;
        while (n + sizeof(ElfW(Nhdr)) <= end) {
            const ElfW(Nhdr) *note = (void *)n;
            size_t names = (note->n_namesz + 3u) & ~3u, desc = (note->n_descsz + 3u) & ~3u;
            n += sizeof *note;
            if (names > (size_t)(end - n) || desc > (size_t)(end - n) - names) break;
            if (note->n_type == NT_GNU_BUILD_ID && note->n_descsz == 20 && !memcmp(n + names, id, 20)) base = i->dlpi_addr;
            n += names + desc;
        }
    }
    return 1; /* the main program only */
}
static void ring(char *dst, const char *data, size_t size, uint64_t at, size_t len) {
    for (size_t i = 0; i < len; i++) dst[i] = data[(at + i) % size];
}
static void *profile(void *arg) {
    struct perf_event_attr a;
    memset(&a, 0, sizeof a);
    a.type = PERF_TYPE_SOFTWARE; a.size = sizeof a; a.config = PERF_COUNT_SW_TASK_CLOCK;
    a.sample_period = 500000; a.sample_type = PERF_SAMPLE_IP | PERF_SAMPLE_TIME;
    a.use_clockid = 1; a.clockid = CLOCK_REALTIME; a.exclude_kernel = 1; a.exclude_hv = 1;
    int fd = (int)syscall(SYS_perf_event_open, &a, (pid_t)(intptr_t)arg, -1, -1, 0);
    size_t page = (size_t)sysconf(_SC_PAGESIZE), size = 64 * page;
    struct perf_event_mmap_page *meta = fd < 0 ? MAP_FAILED : mmap(NULL, size + page, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    FILE *out = fopen("/tmp/samples.txt", "w");
    if (meta == MAP_FAILED || !out) { fprintf(stderr, "LSW PROFILE|unavailable|\n"); return NULL; }
    const char *data = (const char *)meta + page;
    for (unsigned tick = 0;; tick++) {
        uint64_t head = __atomic_load_n(&meta->data_head, __ATOMIC_ACQUIRE), tail = meta->data_tail;
        while (tail < head) {
            struct perf_event_header h;
            uint64_t v[2]; /* ip, time */
            ring((char *)&h, data, size, tail, sizeof h);
            if (h.type == PERF_RECORD_SAMPLE) {
                ring((char *)v, data, size, tail + sizeof h, sizeof v);
                fprintf(out, "%llu 0x%llx\n", (unsigned long long)(v[1] / 1000000), (unsigned long long)v[0]);
            }
            tail += h.size;
        }
        __atomic_store_n(&meta->data_tail, tail, __ATOMIC_RELEASE);
        fflush(out);
        if (tick % 300 == 10) { /* the maps settle once QML has loaded */
            FILE *in = fopen("/proc/self/maps", "r"), *maps = fopen("/tmp/maps.txt", "w");
            char line[512];
            while (in && maps && fgets(line, sizeof line, in)) fputs(line, maps);
            if (in) fclose(in);
            if (maps) fclose(maps);
        }
        usleep(10000);
    }
}
__attribute__((constructor)) static void sclive4_soc(void) {
    char exe[64];
    ssize_t n = readlink("/proc/self/exe", exe, sizeof exe - 1);
    if (n <= 0) return;
    exe[n] = 0;
    if (strcmp(exe, "/usr/Engine/Engine")) return;
    const char *p = getenv("LSW_PROFILE");
    pthread_t sampler;
    if (p && !strcmp(p, "1") && !pthread_create(&sampler, NULL, profile, (void *)(intptr_t)getpid())) pthread_detach(sampler);
    dl_iterate_phdr(identify, NULL);
    if (!base) return; /* not the 5.3.0-sclive4 Engine */
    static const uint32_t expected[] = {0xe16d42f4, 0xe3031e54}, replacement[] = {0xe3a00001, 0xe12fff1e}; /* mov r0,#1; bx lr */
    void *at = (void *)(base + 0x3a6794);
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    void *start = (void *)((uintptr_t)at & ~(page - 1));
    if (memcmp(at, expected, sizeof expected) || mprotect(start, page, PROT_READ | PROT_WRITE)) _exit(127);
    memcpy(at, replacement, sizeof replacement);
    if (mprotect(start, page, PROT_READ | PROT_EXEC)) _exit(127);
    __builtin___clear_cache(at, (char *)at + sizeof replacement);
}
