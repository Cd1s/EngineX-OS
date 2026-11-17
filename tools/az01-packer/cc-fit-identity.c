/* Sandbox-only. Never ship this library.
 * The gen-2 armhf Engine (5.3.0-sclive4, every SC LIVE / PRIME 4+ / PRIME GO+ / Mixstream Pro+ / Pro Go board) exits with
 * "TestApp" when its SoC identity check cannot read a deck's eFuse. Stub that one predicate, as sclive-aux-hardware-fixture.c
 * does, without its AUX routing capture (which only the SC LIVE boards' hook edits satisfy). The build id and the original
 * instructions are both required; production hooks keep the check.
 */
#define _GNU_SOURCE
#include <link.h>
#include <stdint.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>
static uintptr_t base;
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
__attribute__((constructor)) static void mock_identity(void) {
    char exe[64];
    ssize_t n = readlink("/proc/self/exe", exe, sizeof exe - 1);
    if (n <= 0) return;
    exe[n] = 0;
    if (strcmp(exe, "/usr/Engine/Engine")) return;
    dl_iterate_phdr(identify, NULL);
    const uint32_t expected[] = {0xe16d42f4, 0xe3031e54}, replacement[] = {0xe3a00001, 0xe12fff1e}; // mov r0, #1; bx lr
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    void *at = (void *)(base + 0x3a6794);
    if (!base || memcmp(at, expected, sizeof expected)) _exit(127);
    void *start = (void *)((uintptr_t)at & ~(page - 1));
    if (mprotect(start, page, PROT_READ | PROT_WRITE)) _exit(127);
    memcpy(at, replacement, sizeof replacement);
    if (mprotect(start, page, PROT_READ | PROT_EXEC)) _exit(127);
    __builtin___clear_cache(at, (char *)at + sizeof replacement);
}
