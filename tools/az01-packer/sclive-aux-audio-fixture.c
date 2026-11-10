/* Test-only live Engine/ALSA null instrumentation. Never calls the Mixer. */
#define _GNU_SOURCE
#include <link.h>
#include <dlfcn.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <pthread.h>
#include <sys/mman.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>
static uintptr_t base;
static unsigned writes, reads, write_tid;
static unsigned *trace;
static unsigned paired, pairing_errors, previous_guard;
static unsigned counter(const unsigned *p) { return __atomic_load_n(p, __ATOMIC_RELAXED); }
static unsigned traced(unsigned n) { return trace ? counter(trace + n) : 0; }
// Called by the TRACE build at native construction completion, before its
// production cache read. Restore native enum data and +0x88 consistently,
// without invoking a setter, property callback, or Mixer function.
void sclive_aux_boot_fixture(void *control) {
    void *value = NULL;
    ((void (*)(void **, void *))(base + 0x3ba7d4))(&value, *(void **)((char *)control + 0x78));
    if (!value) _exit(127);
    uintptr_t table = *(uintptr_t *)value;
    void *getter = *(void **)(table + 0xa4);
    if ((uintptr_t)getter != base + 0x1669d58 || *(uint32_t *)(base + 0x1669dac) != 0xe5d34018) _exit(127);
    uintptr_t snapshot[4] = {0};
    ((void (*)(void *, void *))(base + 0x1647dd8))(snapshot, value);
    if (!snapshot[2]) _exit(127);
    *(unsigned char *)(snapshot[2] + 24) = 1;
    *(int *)((char *)control + 0x88) = 1;
    if (((int (*)(void *))getter)(value) != 1) _exit(127);
    if (snapshot[3]) ((void (*)(void *))(base + 0x3895dc))((void *)snapshot[3]);
    void *root = (char *)value + *(int *)(table - 12);
    ((void (*)(void *))(base + 0x1871be8))(root);
    fprintf(stderr, "X171 BOOT_SEED|native enum=1|cache=1|no selector notification|\n");
}
// null PCM has no hardware control card. Advertise one test card to Engine's
// stock enumerator, then let its real ALSADevice open/configure the null PCM.
int snd_card_next(int *card) { *card = *card < 0 ? 0 : -1; return 0; }
int snd_ctl_open(void **ctl, const char *name, int mode) {
    (void)mode; *ctl = (void *)0x171;
    fprintf(stderr, "X171 ALSA_CTL|%s|\n", name); return 0;
}
int snd_ctl_close(void *ctl) { (void)ctl; return 0; }
int snd_ctl_card_info(void *ctl, void *info) { (void)ctl; (void)info; return 0; }
const char *snd_ctl_card_info_get_name(const void *info) { (void)info; return "JP21"; }
const char *snd_ctl_card_info_get_components(const void *info) { (void)info; return "X171 null PCM"; }
int snd_ctl_pcm_next_device(void *ctl, int *device) {
    (void)ctl; *device = *device < 0 ? 0 : -1; return 0;
}
int snd_ctl_pcm_info(void *ctl, void *info) { (void)ctl; (void)info; return 0; }
int snd_ctl_elem_info(void *ctl, void *info) { (void)ctl; (void)info; return -2; }
int snd_ctl_elem_read(void *ctl, void *value) { (void)ctl; (void)value; return -2; }
int snd_ctl_elem_write(void *ctl, void *value) { (void)ctl; (void)value; return -2; }
const char *snd_pcm_info_get_name(const void *info) { (void)info; return "X171 null PCM"; }
typedef struct _snd_pcm snd_pcm_t;
typedef unsigned long snd_pcm_uframes_t;
typedef long snd_pcm_sframes_t;
int snd_pcm_hw_params_any(snd_pcm_t *pcm, void *params) {
    int (*real)(snd_pcm_t *, void *) = dlsym(RTLD_NEXT, "snd_pcm_hw_params_any");
    int (*channels)(snd_pcm_t *, void *, unsigned) = dlsym(RTLD_NEXT, "snd_pcm_hw_params_set_channels");
    int rc = real(pcm, params);
    // The unconstrained null advertises 2^30-1 channels, overflowing Engine's
    // frame width. Give the real null hw_params a finite test-card constraint.
    int (*stream)(snd_pcm_t *) = dlsym(RTLD_NEXT, "snd_pcm_stream");
    unsigned count = stream(pcm) == 1 ? 2 : 8;
    if (rc >= 0) rc = channels(pcm, params, count);
    fprintf(stderr, "X171 ALSA_ANY|channels=%u|rc=%d|\n", count, rc); return rc;
}
int snd_pcm_hw_params_set_access(snd_pcm_t *pcm, void *params, int access) {
    int (*real)(snd_pcm_t *, void *, int) = dlsym(RTLD_NEXT, "snd_pcm_hw_params_set_access");
    int rc = real(pcm, params, access); fprintf(stderr, "X171 ALSA_ACCESS|value=%d|rc=%d|\n", access, rc); return rc;
}
int snd_pcm_hw_params_set_channels(snd_pcm_t *pcm, void *params, unsigned channels) {
    int (*real)(snd_pcm_t *, void *, unsigned) = dlsym(RTLD_NEXT, "snd_pcm_hw_params_set_channels");
    int rc = real(pcm, params, channels); fprintf(stderr, "X171 ALSA_CHANNELS|value=%u|rc=%d|\n", channels, rc); return rc;
}
int snd_pcm_hw_params_test_format(snd_pcm_t *pcm, void *params, int format) {
    int (*real)(snd_pcm_t *, void *, int) = dlsym(RTLD_NEXT, "snd_pcm_hw_params_test_format");
    return format == 10 ? real(pcm, params, format) : -22; // S32_LE
}
int snd_pcm_hw_params_set_format(snd_pcm_t *pcm, void *params, int format) {
    int (*real)(snd_pcm_t *, void *, int) = dlsym(RTLD_NEXT, "snd_pcm_hw_params_set_format");
    int rc = real(pcm, params, format);
    fprintf(stderr, "X171 ALSA_FORMAT|format=%d|rc=%d|\n", format, rc);
    return rc;
}
int snd_pcm_open(snd_pcm_t **pcm, const char *name, int stream, int mode) {
    int (*real)(snd_pcm_t **, const char *, int, int) = dlsym(RTLD_NEXT, "snd_pcm_open");
    const char *enabled = getenv("X171_ALSA_NULL");
    const char *target = enabled && !strcmp(enabled, "1") ? "x171_null" : name;
    int rc = real(pcm, target, stream, mode);
    fprintf(stderr, "X171 ALSA_OPEN|name=%s|target=%s|stream=%d|rc=%d|\n", name, target, stream, rc);
    return rc;
}
int snd_pcm_start(snd_pcm_t *pcm) {
    int (*real)(snd_pcm_t *) = dlsym(RTLD_NEXT, "snd_pcm_start");
    int (*state)(snd_pcm_t *) = dlsym(RTLD_NEXT, "snd_pcm_state");
    int rc = real(pcm), current = state(pcm);
    // null can auto-start during prefill; an already running PCM needs no start.
    fprintf(stderr, "X171 ALSA_START|rc=%d|state=%d|\n", rc, current);
    return rc == -77 && current == 3 ? 0 : rc; // EBADFD / RUNNING
}
snd_pcm_sframes_t snd_pcm_writei(snd_pcm_t *pcm, const void *buf, snd_pcm_uframes_t frames) {
    static snd_pcm_sframes_t (*real)(snd_pcm_t *, const void *, snd_pcm_uframes_t);
    if (!real) real = dlsym(RTLD_NEXT, "snd_pcm_writei");
    unsigned tid = (unsigned)syscall(SYS_gettid), guard = traced(0);
    __atomic_fetch_add(&writes, 1, __ATOMIC_RELAXED);
    __atomic_store_n(&write_tid, tid, __ATOMIC_RELAXED);
    if (guard && previous_guard) {
        if (guard != previous_guard + 1 || guard != traced(5) ||
            tid != traced(1) || tid == (unsigned)getpid())
            __atomic_fetch_add(&pairing_errors, 1, __ATOMIC_RELAXED);
        __atomic_fetch_add(&paired, 1, __ATOMIC_RELAXED);
    }
    previous_guard = guard;
    uint64_t ns = (uint64_t)frames * 1000000000u / 48000;
    struct timespec delay = {.tv_sec=(time_t)(ns / 1000000000u), .tv_nsec=(long)(ns % 1000000000u)};
    nanosleep(&delay, NULL); // pace the real null PCM; fixture only
    return real(pcm, buf, frames);
}
snd_pcm_sframes_t snd_pcm_readi(snd_pcm_t *pcm, void *buf, snd_pcm_uframes_t frames) {
    static snd_pcm_sframes_t (*real)(snd_pcm_t *, void *, snd_pcm_uframes_t);
    if (!real) real = dlsym(RTLD_NEXT, "snd_pcm_readi");
    snd_pcm_sframes_t n = real(pcm, buf, frames);
    __atomic_fetch_add(&reads, 1, __ATOMIC_RELAXED);
    // Synthetic packed ADC samples enter at the ALSA boundary, then pass
    // through Engine's real deinterleaver, callback and complete Mixer DSP.
    int32_t *samples = buf;
    static unsigned phase;
    for (snd_pcm_sframes_t i = 0; i < n; i++, phase = (phase + 1) % 48) {
        // Quiet 1 kHz triangle waves at 48 kHz; both packed 16-bit lanes carry
        // a signal, with quadrature stereo rather than an inaudible DC test.
        for (unsigned side = 0; side < 2; side++) {
            unsigned at = (phase + side * 12) % 48;
            int triangle = (int)(at < 24 ? at : 48 - at) - 12;
            uint16_t aux = (uint16_t)(int16_t)(triangle * 128);
            uint16_t mic = (uint16_t)(int16_t)(triangle * 64);
            samples[i * 2 + side] = (int32_t)(((uint32_t)aux << 16) | mic);
        }
    }
    return n;
}
static void *report_audio(void *unused) {
    (void)unused;
    for (unsigned i = 0; i < 4; i++) {
        sleep(5);
        unsigned n = traced(0);
        fprintf(stderr, "X171 AUDIO|pid=%u|guard=%u|tid=%u|main=%u|caller=0x%x|frames=%u|writes=%u|reads=%u|write_tid=%u|\n",
                (unsigned)getpid(), n, traced(1), traced(2),
                n ? traced(3) - (unsigned)base : 0, traced(4), counter(&writes), counter(&reads), counter(&write_tid));
    }
    unsigned errors = counter(&pairing_errors), blocks = counter(&paired);
    fprintf(stderr, "X171 AUDIO_PAIR|blocks=%u|errors=%u|native-returns=%u|\n", blocks, errors, traced(5));
    fprintf(stderr, "X171 BOOT_AUX|initial=%u|constructors=%u|before-transition=%u|capture-nonzero=%u|render-nonzero=%u|capture-alias=%u|\n",
            traced(6), traced(7), traced(12), traced(13), traced(14), traced(15));
    int pass = blocks >= 100 && !errors && !traced(2) && traced(6) == 1 && traced(7) == 1 &&
        traced(8) && traced(9) && traced(12) && traced(13) && traced(14) && !traced(15);
    fprintf(stderr, "X171 AUDIO_DONE|%s|Engine ALSA thread; one guard/native return per block; full DSP; boot AUX without touching selector|\n", pass ? "PASS" : "FAIL");
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
    trace = dlsym(RTLD_DEFAULT, "sclive_aux_trace");
    if (!trace) _exit(127);
    pthread_t reporter;
    if (pthread_create(&reporter, NULL, report_audio, NULL)) _exit(127);
    pthread_detach(reporter);
    fprintf(stderr, "X171 LIVE|SoC/card fixture; real ALSA null; initial AUX; no direct Mixer invocation or DSP shortcut|\n");
}
