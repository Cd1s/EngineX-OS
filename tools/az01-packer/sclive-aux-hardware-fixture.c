/* Sandbox-only JP21 hardware-identity fixture. Never ship this library.
 * The gen-2 Engine exits with "TestApp" when its SoC identity check cannot
 * inspect the real eFuse sysfs device. Stub that ONE predicate in this private
 * process so the actual Engine/Planck/QML can run without a deck. Both BuildID
 * and the function prefix are mandatory. Production hooks retain this check.
 */
#define _GNU_SOURCE
#include <link.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <pthread.h>
#include <signal.h>
#include <ucontext.h>
#include <sys/mman.h>
#include <unistd.h>
static uintptr_t base;
static void (*route)(void *, void *);
static float input_aux[2][8192], input_mic[2][8192];
static unsigned observed_off, observed_aux3, observed_aux4, observed_mic, observed_both, observed_on, bad;
static void *control_seen;
static void (*changed)(void *, void *, uintptr_t, uintptr_t);
static void *(*constructed)(void *, void *, void *, void *, uintptr_t);
static void *allocations[8192];
static unsigned allocation_count;
static pthread_mutex_t driver_lock = PTHREAD_MUTEX_INITIALIZER;
// Capture the real Mixer allocated by Engine, as the existing offline FX
// fixture captures real native handlers. No heap scan runs in production.
void *_Znwj(size_t n) {
    void *p = malloc(n);
    if (n == 0x150) {
        unsigned at = __atomic_fetch_add(&allocation_count, 1, __ATOMIC_RELAXED);
        if (at < 8192) __atomic_store_n(&allocations[at], p, __ATOMIC_RELEASE);
    }
    return p;
}

static void observe_control(void *c, void *p, uintptr_t a, uintptr_t b) {
    changed(c, p, a, b);
    __atomic_store_n(&control_seen, c, __ATOMIC_RELEASE);
}
static void *observe_initial(void *c, void *d, void *mic2, void *aux, uintptr_t flag) {
    void *result = constructed(c, d, mic2, aux, flag);
    __atomic_store_n(&control_seen, c, __ATOMIC_RELEASE);
    return result;
}
static void inject_capture(void *m, void *buffers) {
    pthread_mutex_lock(&driver_lock); // fixture only: serialize the offline driver
    float **io = buffers;
    unsigned frames = *(unsigned *)((char *)buffers + 0x140);
    if (frames > 8192) { __atomic_fetch_add(&bad, 1, __ATOMIC_RELAXED); route(m, buffers); pthread_mutex_unlock(&driver_lock); return; }
    float *saved[4];
    memcpy(saved, io + 0x2e, sizeof saved);
    io[0x2e] = input_mic[0]; io[0x2f] = input_mic[1];
    io[0x30] = input_aux[0]; io[0x31] = input_aux[1];
    route(m, buffers); // the shipping guard, then the real Mixer entry probe
    if (io[0x30] != input_aux[0] || io[0x31] != input_aux[1]) __atomic_fetch_add(&bad, 1, __ATOMIC_RELAXED);
    memcpy(io + 0x2e, saved, sizeof saved);
    pthread_mutex_unlock(&driver_lock);
}
static int silent(float *p, unsigned n) {
    if (!p) return 0;
    for (unsigned i = 0; i < n; i++) if (p[i] != 0) return 0;
    return 1;
}
// Observe immediately after the shipping guard changed the native source
// pointers. Return before the DSP body, whose device setup is unavailable here.
// This signal instrumentation and synthetic capture exist ONLY in this fixture.
static void source_trap(int signal, siginfo_t *info, void *context) {
    (void)signal; (void)info;
    ucontext_t *uc = context;
    if (uc->uc_mcontext.arm_pc != base + 0xcbd974) _exit(126);
    char *m = (void *)(uintptr_t)uc->uc_mcontext.arm_r0;
    float **io = (void *)(uintptr_t)uc->uc_mcontext.arm_r1;
    void **channels = *(void ***)(m + 0x40);
    unsigned frames = *(unsigned *)((char *)io + 0x140);
    int three = channels && *(unsigned char *)((char *)channels[2] + 0x7a);
    int four = channels && *(unsigned char *)((char *)channels[3] + 0x7a);
    if (three || four) {
        if (io[0x2e] != input_mic[0] || io[0x2f] != input_mic[1])
            __atomic_fetch_add(&bad, 1, __ATOMIC_RELAXED);
        void *c = __atomic_load_n(&control_seen, __ATOMIC_ACQUIRE);
        int select = c ? *(int *)((char *)c + 0x88) : -1;
        for (unsigned i = 0x30; i < 0x32; i++)
            if (select == 1 ? !silent(io[i], frames) : io[i] != input_aux[i - 0x30])
                __atomic_fetch_add(&bad, 1, __ATOMIC_RELAXED);
        if (io[0x32] == input_aux[0] && io[0x33] == input_aux[1] &&
            io[0x34] == input_aux[0] && io[0x35] == input_aux[1]) {
            if (select == 1) {
                if (three) __atomic_fetch_add(&observed_aux3, 1, __ATOMIC_RELAXED);
                if (four) __atomic_fetch_add(&observed_aux4, 1, __ATOMIC_RELAXED);
                if (three && four) __atomic_fetch_add(&observed_both, 1, __ATOMIC_RELAXED);
                if (*(unsigned char *)((char *)c + 0x84)) __atomic_fetch_add(&observed_on, 1, __ATOMIC_RELAXED);
            }
        } else {
            for (unsigned i = 0x32; i < 0x36; i++)
                if (!silent(io[i], frames)) __atomic_fetch_add(&bad, 1, __ATOMIC_RELAXED);
            if (select == 0) __atomic_fetch_add(&observed_mic, 1, __ATOMIC_RELAXED);
        }
    } else if (io[0x30] == input_aux[0] && io[0x2e] == input_mic[0]) {
        __atomic_fetch_add(&observed_off, 1, __ATOMIC_RELAXED);
    } else __atomic_fetch_add(&bad, 1, __ATOMIC_RELAXED);
    uint32_t *sp = (void *)(uintptr_t)uc->uc_mcontext.arm_sp;
    // There is no real ALSA device clock in this sandbox. Return after observing
    // the real Mixer entry; its DSP body requires the hardware driver's setup.
    // Restore exactly the two displaced STRD saves and the original SP/ISA.
    uc->uc_mcontext.arm_r4 = sp[0]; uc->uc_mcontext.arm_r5 = sp[1];
    uc->uc_mcontext.arm_r6 = sp[2]; uc->uc_mcontext.arm_r7 = sp[3];
    uc->uc_mcontext.arm_sp += 36;
    uc->uc_mcontext.arm_pc = uc->uc_mcontext.arm_lr & ~1u;
    uc->uc_mcontext.arm_cpsr = (uc->uc_mcontext.arm_cpsr & ~0x20u) | ((uc->uc_mcontext.arm_lr & 1u) ? 0x20u : 0);
}
static void *report_sources(void *unused) {
    (void)unused;
    static float samples[0x50][512];
    static float *io[0x52];
    void *mixer = NULL;
    for (unsigned i = 0; i < 0x50; i++) io[i] = samples[i];
    *(unsigned *)((char *)io + 0x140) = 256;
    for (unsigned i = 0; i < 100; i++) {
        usleep(200000);
        if (!mixer) {
            unsigned n = __atomic_load_n(&allocation_count, __ATOMIC_ACQUIRE);
            if (n > 8192) n = 8192;
            for (unsigned j = 0; j < n; j++) {
                void *p = __atomic_load_n(&allocations[j], __ATOMIC_ACQUIRE);
                if (p && *(uintptr_t *)p == base + 0x2b966ec && *(unsigned *)((char *)p + 0x18) == 4 &&
                    *(void **)((char *)p + 0xa4) && *(void **)((char *)p + 0xa8)) { mixer = p; break; }
            }
            if (mixer) fprintf(stderr, "X171 DRIVER|real Mixer captured; isolated synthetic capture clock|\n");
        }
        if (mixer) ((void (*)(void *, void *))(base + 0xcbd96c))(mixer, io);
        unsigned off = __atomic_load_n(&observed_off, __ATOMIC_RELAXED);
        unsigned three = __atomic_load_n(&observed_aux3, __ATOMIC_RELAXED);
        unsigned four = __atomic_load_n(&observed_aux4, __ATOMIC_RELAXED);
        unsigned mic = __atomic_load_n(&observed_mic, __ATOMIC_RELAXED);
        unsigned errors = __atomic_load_n(&bad, __ATOMIC_RELAXED);
        unsigned both = __atomic_load_n(&observed_both, __ATOMIC_RELAXED);
        unsigned on = __atomic_load_n(&observed_on, __ATOMIC_RELAXED);
        if (errors || (off && three && four && mic && both && on) || i == 99) {
            fprintf(stderr, "X171 BUFFER|off=%u|aux3=%u|aux4=%u|mic-denied=%u|both=%u|on=%u|errors=%u|\n", off, three, four, mic, both, on, errors);
            fprintf(stderr, "X171 ALLOCATIONS|%u|\n", __atomic_load_n(&allocation_count, __ATOMIC_RELAXED));
            fprintf(stderr, "X171 BUFFER_DONE|%s|real Mixer entry, injected capture, source pointers and dry silence|\n",
                    !errors && off && three && four && mic && both && on ? "PASS" : "FAIL");
            return NULL;
        }
    }
    return NULL;
}
static int identify(struct dl_phdr_info *i, size_t size, void *unused) {
    (void)size; (void)unused;
    const unsigned char id[20] = {0xb9,0x46,0xc3,0xb8,0xd9,0x9d,0x32,0x99,0x8c,0x81,0x97,0xd5,0xdc,0x90,0x5a,0x6b,0xdd,0xaa,0x5d,0x2f};
    for (unsigned p = 0; p < i->dlpi_phnum; p++) {
        const ElfW(Phdr) *h = i->dlpi_phdr + p;
        if (h->p_type != PT_NOTE) continue;
        const unsigned char *n = (void *)(i->dlpi_addr + h->p_vaddr), *end = n + h->p_memsz;
        while (n + sizeof(ElfW(Nhdr)) <= end) {
            const ElfW(Nhdr) *note = (void *)n;
            size_t names = (note->n_namesz + 3u) & ~3u, desc = (note->n_descsz + 3u) & ~3u;
            n += sizeof *note;
            if (names > (size_t)(end-n) || desc > (size_t)(end-n)-names) break;
            if (note->n_type == NT_GNU_BUILD_ID && note->n_namesz == 4 && note->n_descsz == 20 &&
                !memcmp(n, "GNU", 4) && !memcmp(n + names, id, 20)) base = i->dlpi_addr;
            n += names + desc;
        }
    }
    return 1;
}
__attribute__((constructor)) static void mock_hardware(void) {
    char exe[64];
    ssize_t n = readlink("/proc/self/exe", exe, sizeof exe - 1);
    if (n <= 0) return;
    exe[n] = 0;
    if (strcmp(exe, "/usr/Engine/Engine")) return;
    dl_iterate_phdr(identify, NULL);
    const uint32_t expected[] = {0xe16d42f4, 0xe3031e54};
    const uint32_t replacement[] = {0xe3a00001, 0xe12fff1e};
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    void *at = (void *)(base + 0x3a6794);
    if (!base || memcmp(at, expected, sizeof expected)) _exit(127);
    void *start = (void *)((uintptr_t)at & ~(page - 1));
    if (mprotect(start, page, PROT_READ | PROT_WRITE)) _exit(127);
    memcpy(at, replacement, sizeof replacement);
    if (mprotect(start, page, PROT_READ | PROT_EXEC)) _exit(127);
    __builtin___clear_cache(at, (char *)at + sizeof replacement);
    uint32_t *process = (void *)(base + 0xcbd96c), *control = (void *)(base + 0xdba454);
    uint32_t *init = (void *)(base + 0x1ba13c0);
    uint32_t *observe = (void *)(base + 0xcbd974);
    if (process[0] != 0xe51ff004 || control[0] != 0xe51ff004 || init[0] != 0xe51ff004 || *observe != 0xe1cda1f8) _exit(127);
    route = (void *)(uintptr_t)process[1]; changed = (void *)(uintptr_t)control[1];
    constructed = (void *)(uintptr_t)init[1];
    for (unsigned i = 0; i < 8192; i++) {
        input_aux[0][i] = 0.125f; input_aux[1][i] = 0.25f;
        input_mic[0][i] = input_mic[1][i] = -0.75f;
    }
    struct sigaction trap = {.sa_sigaction=source_trap, .sa_flags=SA_SIGINFO};
    sigemptyset(&trap.sa_mask);
    if (sigaction(SIGILL, &trap, NULL)) _exit(127);
    void *process_page = (void *)((uintptr_t)process & ~(page - 1));
    void *control_page = (void *)((uintptr_t)control & ~(page - 1));
    void *init_page = (void *)((uintptr_t)init & ~(page - 1));
    if (mprotect(process_page, page, PROT_READ | PROT_WRITE) || mprotect(control_page, page, PROT_READ | PROT_WRITE) ||
        mprotect(init_page, page, PROT_READ | PROT_WRITE)) _exit(127);
    process[1] = (uintptr_t)inject_capture; control[1] = (uintptr_t)observe_control;
    init[1] = (uintptr_t)observe_initial;
    *observe = 0xe7f000f0; // UDF #0
    if (mprotect(process_page, page, PROT_READ | PROT_EXEC) || mprotect(control_page, page, PROT_READ | PROT_EXEC) ||
        mprotect(init_page, page, PROT_READ | PROT_EXEC)) _exit(127);
    __builtin___clear_cache(process, (char *)observe + 4);
    __builtin___clear_cache(control, (char *)control + 8);
    __builtin___clear_cache(init, (char *)init + 8);
    pthread_t reporter;
    if (pthread_create(&reporter, NULL, report_sources, NULL)) _exit(127);
    pthread_detach(reporter);
    fprintf(stderr, "X171 HARNESS|mock SoC identity predicate 0x3a6794; real hardware untested|\n");
}
