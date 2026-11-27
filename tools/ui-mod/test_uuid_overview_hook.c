// S19: Database::resetUuid() OverviewData directory rename/merge and in-memory m_overviewDataDir sync.
// Build & run natively on an ARM Linux host:
//   aarch64: gcc -O2 -Wall -Wextra -Werror -funwind-tables -o /tmp/test_uuid_overview_hook64 tools/ui-mod/test_uuid_overview_hook.c -ldl -lpthread
//   ASan:    gcc -O1 -g -Wall -Wextra -Werror -funwind-tables -fsanitize=address,undefined -fno-sanitize-recover=undefined -o /tmp/test_uuid_overview_hook_asan tools/ui-mod/test_uuid_overview_hook.c -ldl -lpthread
//   armhf:   arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror -funwind-tables -o /tmp/test_uuid_overview_hook32 tools/ui-mod/test_uuid_overview_hook.c -ldl -lpthread
#define ENGINE_UI_HOOK_TEST
int hook_test_engine = 1;
#include "engine_ui_hook.c"
#include <assert.h>
#include <stddef.h>

struct fake_cxx_string {
    char *ptr;
    size_t len;
    union {
        size_t cap;
        char sso[16];
    } u;
};
_Static_assert(sizeof(struct fake_cxx_string) == (sizeof(void *) == 8 ? 32 : 24), "CXX11 std::string size");

struct fake_database {
#if defined(__aarch64__)
    unsigned char pad0[0xa8];
    struct fake_cxx_string overview_dir;
    struct fake_cxx_string uuid;
#else
    unsigned char pad0[0x6c];
    struct fake_cxx_string overview_dir;
    struct fake_cxx_string uuid;
#endif
    unsigned char tail[64];
};
_Static_assert(offsetof(struct fake_database, overview_dir) == S19_DB_OVERVIEW_DIR_OFF, "m_overviewDataDir offset");
_Static_assert(offsetof(struct fake_database, uuid) == S19_DB_UUID_OFF, "m_uuid offset");

static void set_heap_string(struct fake_cxx_string *s, const char *val) {
    size_t len = strlen(val);
    char *buf = malloc(len + 64);
    assert(buf != NULL);
    memcpy(buf, val, len + 1);
    s->ptr = buf;
    s->len = len;
    s->u.cap = len + 63;
}

static void set_sso_string(struct fake_cxx_string *s, const char *val) {
    size_t len = strlen(val);
    assert(len < sizeof s->u.sso);
    memcpy(s->u.sso, val, len + 1);
    s->ptr = s->u.sso;
    s->len = len;
}

static void free_cxx_string(struct fake_cxx_string *s) {
    if (s->ptr && s->ptr != s->u.sso) free(s->ptr);
    s->ptr = NULL;
    s->len = 0;
}

static const char *fake_next_uuid;
static int fake_orig_calls;

static void fake_reset_uuid(void *ptr) {
    struct fake_database *db = ptr;
    fake_orig_calls++;
    if (!fake_next_uuid) return;
    size_t len = strlen(fake_next_uuid);
    if (db->uuid.ptr == db->uuid.u.sso && len >= sizeof db->uuid.u.sso) {
        char *buf = malloc(len + 16);
        assert(buf != NULL);
        memcpy(buf, fake_next_uuid, len + 1);
        db->uuid.ptr = buf;
        db->uuid.len = len;
        db->uuid.u.cap = len + 15;
        return;
    }
    if (db->uuid.ptr == db->uuid.u.sso) {
        memcpy(db->uuid.u.sso, fake_next_uuid, len + 1);
        db->uuid.len = len;
        return;
    }
    if (len <= db->uuid.u.cap) {
        memcpy(db->uuid.ptr, fake_next_uuid, len + 1);
        db->uuid.len = len;
    } else {
        char *buf = malloc(len + 16);
        assert(buf != NULL);
        memcpy(buf, fake_next_uuid, len + 1);
        free(db->uuid.ptr);
        db->uuid.ptr = buf;
        db->uuid.len = len;
        db->uuid.u.cap = len + 15;
    }
}

static void write_file_bytes(const char *path, const char *content) {
    FILE *f = fopen(path, "wb");
    assert(f != NULL);
    assert(fwrite(content, 1, strlen(content), f) == strlen(content));
    assert(fclose(f) == 0);
}

static void read_file_bytes(const char *path, char *buf, size_t cap) {
    FILE *f = fopen(path, "rb");
    assert(f != NULL);
    size_t n = fread(buf, 1, cap - 1, f);
    buf[n] = 0;
    assert(fclose(f) == 0);
}

static void rm_rf_tmp(const char *path) {
    struct stat st;
    if (lstat(path, &st) != 0) return;
    if (S_ISDIR(st.st_mode)) {
        DIR *d = opendir(path);
        if (d) {
            struct dirent *e;
            while ((e = readdir(d)) != NULL) {
                if (!strcmp(e->d_name, ".") || !strcmp(e->d_name, "..")) continue;
                char sub[PATH_MAX];
                assert(s19_join_path(sub, sizeof sub, path, e->d_name));
                rm_rf_tmp(sub);
            }
            closedir(d);
        }
        rmdir(path);
    } else {
        unlink(path);
    }
}

static void make_db(struct fake_database *db, const char *root, const char *uuid, int sso_uuid) {
    memset(db, 0, sizeof *db);
    char dir[PATH_MAX];
    snprintf(dir, sizeof dir, "%s/OverviewData/%s", root, uuid);
    set_heap_string(&db->overview_dir, dir);
    if (sso_uuid && strlen(uuid) < 16) set_sso_string(&db->uuid, uuid);
    else set_heap_string(&db->uuid, uuid);
}

static void test_hook_behaviors(void) {
    char root_tmpl[] = "/var/tmp/f336-s19-unit-XXXXXX";
    char *root = mkdtemp(root_tmpl);
    assert(root != NULL);

    char ov_root[PATH_MAX], conf[PATH_MAX];
    assert(s19_join_path(ov_root, sizeof ov_root, root, "OverviewData"));
    assert(mkdir(ov_root, 0755) == 0);
    assert(s19_join_path(conf, sizeof conf, root, "hook-disable.conf"));
    hf_path = conf;
    hf_reload();

    const char *old_u = "11111111-1111-4111-8111-111111111111";
    const char *new_u = "22222222-2222-4222-8222-222222222222";
    char old_dir[PATH_MAX], new_dir[PATH_MAX], f1[PATH_MAX], f2[PATH_MAX], want_dir[PATH_MAX], buf[128];
    struct stat st;

    // 1. Happy path whole-dir rename + in-memory m_overviewDataDir update.
    assert(s19_join_path(old_dir, sizeof old_dir, ov_root, old_u));
    assert(s19_join_path(new_dir, sizeof new_dir, ov_root, new_u));
    assert(mkdir(old_dir, 0755) == 0);
    assert(s19_join_path(f1, sizeof f1, old_dir, "1.rgb"));
    assert(s19_join_path(f2, sizeof f2, old_dir, "2.rgb"));
    write_file_bytes(f1, "rgb-one");
    write_file_bytes(f2, "rgb-two");

    struct fake_database db;
    make_db(&db, root, old_u, 0);
    reset_uuid_orig = fake_reset_uuid;
    fake_next_uuid = new_u;
    fake_orig_calls = 0;
    hook_database_reset_uuid(&db);
    assert(fake_orig_calls == 1);
    assert(lstat(old_dir, &st) != 0 && errno == ENOENT);
    assert(s19_join_path(f1, sizeof f1, new_dir, "1.rgb"));
    read_file_bytes(f1, buf, sizeof buf);
    assert(!strcmp(buf, "rgb-one"));
    snprintf(want_dir, sizeof want_dir, "%s/OverviewData/%s", root, new_u);
    assert(!strcmp(db.overview_dir.ptr, want_dir));
    assert(db.overview_dir.len == strlen(want_dir));
    free_cxx_string(&db.overview_dir);
    free_cxx_string(&db.uuid);
    rm_rf_tmp(new_dir);

    // 2. Empty existing dest dir is replaced atomically.
    assert(mkdir(old_dir, 0755) == 0);
    assert(mkdir(new_dir, 0755) == 0);
    assert(s19_join_path(f1, sizeof f1, old_dir, "1.rgb"));
    write_file_bytes(f1, "from-old");
    make_db(&db, root, old_u, 0);
    fake_next_uuid = new_u;
    hook_database_reset_uuid(&db);
    assert(lstat(old_dir, &st) != 0 && errno == ENOENT);
    assert(s19_join_path(f1, sizeof f1, new_dir, "1.rgb"));
    read_file_bytes(f1, buf, sizeof buf);
    assert(!strcmp(buf, "from-old"));
    assert(!strcmp(db.overview_dir.ptr, want_dir));
    free_cxx_string(&db.overview_dir);
    free_cxx_string(&db.uuid);
    rm_rf_tmp(new_dir);

    // 3. Non-empty dest dir merges non-overwriting and skips symlinks.
    char outside[PATH_MAX];
    assert(s19_join_path(outside, sizeof outside, root, "outside.rgb"));
    write_file_bytes(outside, "outside-secret");
    assert(mkdir(old_dir, 0755) == 0);
    assert(mkdir(new_dir, 0755) == 0);
    assert(s19_join_path(f1, sizeof f1, new_dir, "1.rgb"));
    write_file_bytes(f1, "newer-in-dest");
    assert(s19_join_path(f1, sizeof f1, old_dir, "1.rgb"));
    write_file_bytes(f1, "older-in-src");
    assert(s19_join_path(f2, sizeof f2, old_dir, "2.rgb"));
    write_file_bytes(f2, "missing-in-dest");
    char sym[PATH_MAX];
    assert(s19_join_path(sym, sizeof sym, old_dir, "3.rgb"));
    assert(symlink(outside, sym) == 0);

    make_db(&db, root, old_u, 0);
    fake_next_uuid = new_u;
    hook_database_reset_uuid(&db);
    assert(s19_join_path(f1, sizeof f1, new_dir, "1.rgb"));
    read_file_bytes(f1, buf, sizeof buf);
    assert(!strcmp(buf, "newer-in-dest"));
    assert(s19_join_path(f2, sizeof f2, new_dir, "2.rgb"));
    read_file_bytes(f2, buf, sizeof buf);
    assert(!strcmp(buf, "missing-in-dest"));
    assert(s19_join_path(sym, sizeof sym, new_dir, "3.rgb"));
    assert(lstat(sym, &st) != 0 && errno == ENOENT);
    assert(!strcmp(db.overview_dir.ptr, want_dir));
    free_cxx_string(&db.overview_dir);
    free_cxx_string(&db.uuid);
    rm_rf_tmp(old_dir);
    rm_rf_tmp(new_dir);
    unlink(outside);

    // 4. Symlink at old_dir or new_dir refused without updating m_overviewDataDir.
    char sym_target[PATH_MAX];
    assert(s19_join_path(sym_target, sizeof sym_target, root, "sym_target"));
    assert(mkdir(sym_target, 0755) == 0);
    assert(symlink(sym_target, old_dir) == 0);
    make_db(&db, root, old_u, 0);
    fake_next_uuid = new_u;
    hook_database_reset_uuid(&db);
    snprintf(want_dir, sizeof want_dir, "%s/OverviewData/%s", root, old_u);
    assert(!strcmp(db.overview_dir.ptr, want_dir));
    assert(lstat(new_dir, &st) != 0 && errno == ENOENT);
    free_cxx_string(&db.overview_dir);
    free_cxx_string(&db.uuid);
    unlink(old_dir);

    assert(mkdir(old_dir, 0755) == 0);
    assert(symlink(sym_target, new_dir) == 0);
    make_db(&db, root, old_u, 0);
    hook_database_reset_uuid(&db);
    assert(!strcmp(db.overview_dir.ptr, want_dir));
    free_cxx_string(&db.overview_dir);
    free_cxx_string(&db.uuid);
    unlink(new_dir);
    rm_rf_tmp(old_dir);
    rm_rf_tmp(sym_target);

    // 5. Missing old_dir (ENOENT) still updates m_overviewDataDir in memory.
    make_db(&db, root, old_u, 0);
    fake_next_uuid = new_u;
    hook_database_reset_uuid(&db);
    snprintf(want_dir, sizeof want_dir, "%s/OverviewData/%s", root, new_u);
    assert(!strcmp(db.overview_dir.ptr, want_dir));
    free_cxx_string(&db.overview_dir);
    free_cxx_string(&db.uuid);

    // 6. Unchanged UUID is a clean no-op.
    assert(mkdir(old_dir, 0755) == 0);
    make_db(&db, root, old_u, 0);
    fake_next_uuid = old_u;
    hook_database_reset_uuid(&db);
    assert(lstat(old_dir, &st) == 0 && S_ISDIR(st.st_mode));
    free_cxx_string(&db.overview_dir);
    free_cxx_string(&db.uuid);
    rm_rf_tmp(old_dir);

    // 7. Invalid old_dir (does not end with /OverviewData/<old_uuid>) leaves dir untouched.
    assert(mkdir(old_dir, 0755) == 0);
    make_db(&db, root, old_u, 0);
    free_cxx_string(&db.overview_dir);
    set_heap_string(&db.overview_dir, "/tmp/wrong-dir");
    fake_next_uuid = new_u;
    hook_database_reset_uuid(&db);
    assert(!strcmp(db.overview_dir.ptr, "/tmp/wrong-dir"));
    assert(lstat(old_dir, &st) == 0 && S_ISDIR(st.st_mode));
    free_cxx_string(&db.overview_dir);
    free_cxx_string(&db.uuid);
    rm_rf_tmp(old_dir);

    // 8. Different UUID length (plus SSO UUID) still migrates disk dir without overwriting m_overviewDataDir.
    const char *short_old = "sso-old-1", *long_new = "longer-new-uuid-12345";
    char s_old[PATH_MAX], s_new[PATH_MAX];
    assert(s19_join_path(s_old, sizeof s_old, ov_root, short_old));
    assert(s19_join_path(s_new, sizeof s_new, ov_root, long_new));
    assert(mkdir(s_old, 0755) == 0);
    assert(s19_join_path(f1, sizeof f1, s_old, "9.rgb"));
    write_file_bytes(f1, "sso-rgb");
    make_db(&db, root, short_old, 1);
    fake_next_uuid = long_new;
    hook_database_reset_uuid(&db);
    assert(lstat(s_old, &st) != 0 && errno == ENOENT);
    assert(s19_join_path(f1, sizeof f1, s_new, "9.rgb"));
    read_file_bytes(f1, buf, sizeof buf);
    assert(!strcmp(buf, "sso-rgb"));
    snprintf(want_dir, sizeof want_dir, "%s/OverviewData/%s", root, short_old);
    assert(!strcmp(db.overview_dir.ptr, want_dir));
    free_cxx_string(&db.overview_dir);
    free_cxx_string(&db.uuid);
    rm_rf_tmp(s_new);

    // 9. sqlite-tuning disabled disables hook execution cleanly.
    write_file_bytes(conf, "sqlite-tuning\n");
    hf_reload();
    assert(!hf_on(HF_SQLITE_TUNING));
    assert(mkdir(old_dir, 0755) == 0);
    make_db(&db, root, old_u, 0);
    fake_next_uuid = new_u;
    fake_orig_calls = 0;
    hook_database_reset_uuid(&db);
    assert(fake_orig_calls == 1);
    assert(lstat(old_dir, &st) == 0 && S_ISDIR(st.st_mode));
    assert(lstat(new_dir, &st) != 0 && errno == ENOENT);
    snprintf(want_dir, sizeof want_dir, "%s/OverviewData/%s", root, old_u);
    assert(!strcmp(db.overview_dir.ptr, want_dir));
    free_cxx_string(&db.overview_dir);
    free_cxx_string(&db.uuid);
    rm_rf_tmp(old_dir);
    unlink(conf);
    hf_reload();

    // 10. Synthetic prefix mismatch refuses to patch.
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    uchar *synth = mmap(NULL, page, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(synth != MAP_FAILED);
    memset(synth, 0xaa, page);
    struct engine_build bad_build = engine_builds[0];
    bad_build.reset_uuid_fn = 0x100;
    select_build(&bad_build);
    reset_uuid_orig = NULL;
    install_uuid_overview_hook((uintptr_t)synth, page);
    assert(reset_uuid_orig == NULL);
    for (size_t i = 0; i < 32; i++) assert(synth[0x100 + i] == 0xaa);
    munmap(synth, page);

    rm_rf_tmp(root);
    puts("S19 hook edge cases (rename, empty dest, merge, symlinks, ENOENT, same UUID, bad dir, SSO/diff len, sqlite-tuning off, prefix mismatch): PASS");
}

static void test_real_elf(const char *filename) {
    FILE *f = fopen(filename, "rb");
    assert(f != NULL);
    assert(fseek(f, 0, SEEK_END) == 0);
    long sz = ftell(f);
    assert(sz > 0);
    rewind(f);
    char *file = malloc((size_t)sz);
    assert(file != NULL && fread(file, 1, (size_t)sz, f) == (size_t)sz);
    fclose(f);

    ElfW(Ehdr) *eh = (void *)file;
    assert(eh->e_ident[EI_CLASS] == (sizeof(void *) == 8 ? ELFCLASS64 : ELFCLASS32));
    ElfW(Phdr) *ph = (void *)(file + eh->e_phoff);
    size_t len = 0, page = (size_t)sysconf(_SC_PAGESIZE);
    for (unsigned i = 0; i < eh->e_phnum; i++)
        if (ph[i].p_type == PT_LOAD && ph[i].p_vaddr + ph[i].p_memsz > len)
            len = ph[i].p_vaddr + ph[i].p_memsz;
    len = (len + page - 1) & ~(page - 1);
    uchar *image = mmap(NULL, len, PROT_READ | PROT_WRITE | PROT_EXEC, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(image != MAP_FAILED);
    for (unsigned i = 0; i < eh->e_phnum; i++)
        if (ph[i].p_type == PT_LOAD)
            memcpy(image + ph[i].p_vaddr, file + ph[i].p_offset, ph[i].p_filesz);
    __builtin___clear_cache(image, image + len);

    uintptr_t base = (uintptr_t)image, found = 0;
    struct dl_phdr_info info = {.dlpi_addr = base, .dlpi_phdr = ph, .dlpi_phnum = eh->e_phnum};
    build = NULL;
    engine_base(&info, sizeof info, &found);
    assert(found == base && build != NULL && build->reset_uuid_fn != 0);

    uintptr_t entry = base + build->reset_uuid_fn;
    uchar saved_prefix[16];
    memcpy(saved_prefix, (const void *)entry, sizeof saved_prefix);

    // Disabled sqlite-tuning must leave stock ELF untouched.
    char conf[] = "/var/tmp/f336-s19-elf-conf-XXXXXX";
    int cfd = mkstemp(conf);
    assert(cfd >= 0);
    assert(write(cfd, "sqlite-tuning\n", 14) == 14);
    assert(close(cfd) == 0);
    hf_path = conf;
    hf_reload();
    reset_uuid_orig = NULL;
    install_uuid_overview_hook(base, page);
    assert(reset_uuid_orig == NULL && !memcmp((const void *)entry, saved_prefix, sizeof saved_prefix));
    unlink(conf);
    hf_reload();

    // Mutated prefix must fail closed.
    ((uchar *)entry)[0] ^= 1;
    install_uuid_overview_hook(base, page);
    assert(reset_uuid_orig == NULL);
    memcpy((void *)entry, saved_prefix, sizeof saved_prefix);

    // Install on real stock bytes.
#if !defined(__aarch64__)
    uint32_t expected_lit = *(const uint32_t *)(entry + 8 + 0x4a4);
#endif
    install_uuid_overview_hook(base, page);
    assert(reset_uuid_orig != NULL);

#if defined(__aarch64__)
    const uint32_t *at = (const uint32_t *)entry, *tramp = (const uint32_t *)reset_uuid_orig;
    uint64_t to = 0, back = 0;
    memcpy(&to, &at[2], 8);
    memcpy(&back, &tramp[6], 8);
    assert(at[0] == 0x58000050 && at[1] == 0xd61f0200 && to == (uintptr_t)hook_database_reset_uuid);
    assert(!memcmp(tramp, saved_prefix, 16) && tramp[4] == 0x58000050 && tramp[5] == 0xd61f0200 && back == entry + 16);
    // Execute displaced prologue + jump to entry+16 with a return gadget at entry+16.
    assert(mprotect((void *)((entry + 16) & ~(page - 1)), page * 2, PROT_READ | PROT_WRITE) == 0);
    uint32_t *ret_at = (uint32_t *)(entry + 16);
    ret_at[0] = 0xa8c67bfd; // ldp x29, x30, [sp], #96
    ret_at[1] = 0xd50323bf; // autiasp
    ret_at[2] = 0xd65f03c0; // ret
    assert(mprotect((void *)((entry + 16) & ~(page - 1)), page * 2, PROT_READ | PROT_EXEC) == 0);
    __builtin___clear_cache(ret_at, ret_at + 3);
    reset_uuid_orig(NULL);
#else
    const uint32_t *at = (const uint32_t *)entry, *tramp = (const uint32_t *)reset_uuid_orig;
    assert(at[0] == 0xe51ff004u && at[1] == (uint32_t)(uintptr_t)hook_database_reset_uuid);
    assert(tramp[0] == 0xe59fc008u && tramp[1] == 0xe16d42f4u && tramp[2] == 0xe51ff004u &&
           tramp[3] == (uint32_t)(entry + 8) && tramp[4] == expected_lit);
    // Execute the 5-word stub and verify it loads expected_lit into ip and lands at entry+8.
    assert(mprotect((void *)((entry + 8) & ~(page - 1)), page * 2, PROT_READ | PROT_WRITE) == 0);
    uint32_t *ret_at = (uint32_t *)(entry + 8);
    ret_at[0] = 0xe28dd024u; // add sp, sp, #36 (undo strd r4, [sp, #-36]!)
    ret_at[1] = 0xe1a0000cu; // mov r0, ip
    ret_at[2] = 0xe12fff1eu; // bx lr
    assert(mprotect((void *)((entry + 8) & ~(page - 1)), page * 2, PROT_READ | PROT_EXEC) == 0);
    __builtin___clear_cache(ret_at, ret_at + 3);
    uint32_t got_lit = ((uint32_t (*)(void))(uintptr_t)reset_uuid_orig)();
    assert(got_lit == expected_lit);
#endif

    printf("S19 real ELF %s (%s): BuildID, resetUuid site 0x%x, stub exec & fail-closed PASS\n",
           build->name, filename, build->reset_uuid_fn);
    munmap(image, len);
    free(file);
}

int main(int argc, char **argv) {
    test_hook_behaviors();
    for (int i = 1; i < argc; i++) {
        test_real_elf(argv[i]);
    }
    return 0;
}
