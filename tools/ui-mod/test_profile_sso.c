// Native armhf / AArch64 regression against the stock Engine ELF, never qemu.
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>

static void test_profile_sso(const char *filename, uintptr_t slot_rva, uintptr_t getter_rva,
                             uintptr_t other_slot_rva) {
    FILE *f = fopen(filename, "rb"); assert(f);
    assert(!fseek(f, 0, SEEK_END)); long size = ftell(f); assert(size > 0);
    rewind(f); char *file = malloc(size); assert(file);
    assert(fread(file, 1, size, f) == (size_t)size); fclose(f);
    ElfW(Ehdr) *eh = (void *)file;
    assert(eh->e_ident[EI_CLASS] == (sizeof(void *) == 8 ? ELFCLASS64 : ELFCLASS32));
    ElfW(Phdr) *ph = (void *)(file + eh->e_phoff);
    size_t len = 0, page = sysconf(_SC_PAGESIZE);
    for (unsigned i = 0; i < eh->e_phnum; i++)
        if (ph[i].p_type == PT_LOAD && ph[i].p_vaddr + ph[i].p_memsz > len)
            len = ph[i].p_vaddr + ph[i].p_memsz;
    len = (len + page - 1) & ~(page - 1);
    char *image = mmap(NULL, len, PROT_READ | PROT_WRITE | PROT_EXEC,
                       MAP_PRIVATE | MAP_ANONYMOUS, -1, 0); assert(image != MAP_FAILED);
    for (unsigned i = 0; i < eh->e_phnum; i++)
        if (ph[i].p_type == PT_LOAD)
            memcpy(image + ph[i].p_vaddr, file + ph[i].p_offset, ph[i].p_filesz);
    __builtin___clear_cache(image, image + len);
    uintptr_t base = (uintptr_t)image, found = 0;
    struct dl_phdr_info info = {.dlpi_addr = base, .dlpi_phdr = ph, .dlpi_phnum = eh->e_phnum};
    engine_base(&info, sizeof info, &found); assert(found == base);
    // Apply just these relative relocations: the pure stock getters call no Qt.
    uintptr_t *slots[] = {(void *)(base + slot_rva), (void *)(base + other_slot_rva)};
#if defined(__aarch64__)
    ElfW(Shdr) *sh = (void *)(file + eh->e_shoff);
    for (unsigned i = 0; i < eh->e_shnum; i++) {
        if (sh[i].sh_type != SHT_RELA) continue;
        ElfW(Rela) *r = (void *)(file + sh[i].sh_offset);
        for (size_t j = 0; j < sh[i].sh_size / sizeof *r; j++)
            for (unsigned k = 0; k < 2; k++)
                if (r[j].r_offset == (uintptr_t)slots[k] - base)
                    *slots[k] = r[j].r_addend;
    }
#endif
    assert(*slots[0] == getter_rva);
    for (unsigned k = 0; k < 2; k++) *slots[k] += base;
    typedef int (*capability)(void *);
    assert(((capability)*slots[0])(NULL) == 1);
    assert(((capability)*slots[1])(NULL) == 1);
    uintptr_t other = *slots[1];
    // Reject a changed vtable slot, then install through the production path.
    (*slots[0])++;
    apply_engine_edits(base, page);
    assert(*slots[0] == base + getter_rva + 1);
    *slots[0] = base + getter_rva;
    apply_engine_edits(base, page);
    apply_engine_edits(base, page);
    char settings[] = "/tmp/profile-sso-XXXXXX";
    int fd = mkstemp(settings); assert(fd >= 0); close(fd);
    four_deck_settings = settings;
    const char *scopes[] = {"", "scope=netease\n", "scope=spotify\n", "scope=deezer\n",
        "scope=beatport\n", "scope=soundcloud\n", "scope=applemusic\n", "scope=tidal\n",
        "scope=deezer\n", "scope=tidal"};
    for (unsigned i = 0; i < sizeof scopes / sizeof *scopes; i++) {
        f = fopen(settings, "w"); assert(f);
        assert(fputs(scopes[i], f) >= 0 && !fclose(f));
        int expected = !strncmp(scopes[i], "scope=tidal", 11);
        assert(((capability)*slots[0])(NULL) == expected);
        assert(*slots[1] == other && ((capability)*slots[1])(NULL) == 1);
    }
    unlink(settings);
    assert(((capability)*slots[0])(NULL) == 0);
    printf("#304 %s: carrier ignores profile credentials; real TIDAL and other providers retain stock SSO: PASS\n", build->name);
    munmap(image, len); free(file);
}

int main(int argc, char **argv) {
#if defined(__aarch64__)
    assert(argc == 2);
    test_profile_sso(argv[1], 0x2d52870, 0x713b80, 0x2d52d20);
#else
    assert(argc == 3);
    test_profile_sso(argv[1], 0x2b68184, 0x573624, 0x2b683dc);
    test_profile_sso(argv[2], 0x2be7944, 0x585a1c, 0x2be7b9c);
#endif
    return 0;
}
