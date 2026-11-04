// Compile and run with ARMv7 gcc / qemu-arm on IN.
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>
#include <sqlite3.h>
#include <sys/wait.h>

__attribute__((naked)) static void invoke_stub(void *feature __attribute__((unused)),
    void *frame __attribute__((unused)), void *stub __attribute__((unused)),
    uint32_t *out __attribute__((unused))) {
    __asm__("push {r4, r5, r7, r8, fp, lr}\n"
            "mov r7, r0\nmov fp, r1\nmov r8, r2\nmov r5, r3\n"
            "mov r0, #17\nmov r1, #23\nmov r2, #29\nmov r4, #0\nblx r8\nstm r5, {r0, r1, r2, r3}\n"
            "pop {r4, r5, r7, r8, fp, pc}");
}

static void test_stems(void) {
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    char *image = mmap(NULL, 0x2a00000, PROT_READ | PROT_WRITE,
                       MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(image != MAP_FAILED);
    uint32_t enabled[] = {0xe51b2328, 0xe51b3324, 0xe12fff1e};
    uint32_t ordinary[] = {0x05873004, 0xe12fff1e}; // streq r3,[r7,#4]; bx lr
    memcpy(image + 0x8429b0, enabled, sizeof enabled);
    memcpy(image + 0x84264c, ordinary, sizeof ordinary);
    char *code_page = image + 0x84264c / page * page;
    assert(mprotect(code_page, page, PROT_READ | PROT_EXEC) == 0);
    __builtin___clear_cache(code_page, code_page + page);
    image[0x258e185] = 1;
    prepare_stems_feature((uintptr_t)image, page);
    uint32_t *feature = (void *)(image + 0x29bc83c);
    uint32_t *other = feature + 8;
    uint32_t false_ptr = (uint32_t)(uintptr_t)(image + 0x258e186);
    feature[1] = other[1] = false_ptr;
    uint32_t frame[0x100] = {0}, out[4];
    char *fp = (char *)frame + 0x350;
    *(uint32_t *)(fp - 808) = 1234;
    *(uint32_t *)(fp - 804) = 5678;
    *(uint32_t *)(fp - 832) = false_ptr;
    void *stub = (void *)(uintptr_t)stems_feature_jump[1];
    invoke_stub(feature, fp, stub, out);
    assert(feature[1] == (uint32_t)(uintptr_t)(image + 0x258e185));
    assert(*(char *)(uintptr_t)feature[1] == 1);
    assert(out[0] == 17 && out[1] == 23 && out[2] == 1234 && out[3] == 5678);
    invoke_stub(other, fp, stub, out);
    assert(other[1] == false_ptr);
    assert(out[0] == 17 && out[1] == 23 && out[2] == 29 && out[3] == false_ptr);
    puts("ARM Stems binding, other-feature isolation and register replay: PASS");
    munmap(image, 0x2a00000);
}

__attribute__((naked)) static void invoke_config(void *config __attribute__((unused)),
    void *stub __attribute__((unused)), uint32_t *out __attribute__((unused))) {
    __asm__("push {r4, lr}\nmov r4, r2\nblx r1\nstm r4, {r0, r2, r3}\npop {r4, pc}");
}

static void test_config(void) {
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    char *image = mmap(NULL, 0x1000000, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(image != MAP_FAILED);
    uint32_t resume = 0xe12fff1e;
    memcpy(image + 0xf63044, &resume, 4);
    *(uint32_t *)(image + 0xf63124) = 123;
    *(uint32_t *)(image + 0xf63120) = 456;
    assert(!mprotect(image + 0xf63000, page, PROT_READ | PROT_EXEC));
    __builtin___clear_cache(image + 0xf63000, image + 0xf64000);
    prepare_config((uintptr_t)image, page);
    const unsigned fields[] = {0xdb, 0x24c, 0x24e, 0x276, 0x289};
    for (int model = 0; model < 6; model++) {
        uchar config[0x500], expected[0x500];
        memset(config, 0xa0 + model, sizeof config);
        memcpy(expected, config, sizeof expected);
        for (size_t i = 0; i < sizeof fields / sizeof *fields; i++) expected[fields[i]] = i == 4 ? 0 : 1;
        uint32_t out[3];
        invoke_config(config, (void *)(uintptr_t)config_jump[1], out);
        assert(!memcmp(config, expected, sizeof config));
        assert(out[0] == (uintptr_t)config && out[1] == 123 && out[2] == 456);
    }
    munmap(image, 0x1000000);
    puts("Common config hook, six input layouts, literal/register replay: PASS");
}

static int exec_count;
static int real_sqlite_exec(void *db, const char *sql, int (*cb)(void *, int, char **, char **), void *arg, char **err) {
    exec_count++;
    return sqlite3_exec(db, sql, cb, arg, err);
}

static void emit_tail(char *at, const uint32_t *prefix, size_t count, const void *fn) {
    memcpy(at, prefix, count * 4);
    uint32_t jump[] = {0xe51ff004, (uint32_t)(uintptr_t)fn};
    memcpy(at + count * 4, jump, sizeof jump);
}

static int pragma(sqlite3 *db, const char *sql) {
    sqlite3_stmt *stmt = NULL;
    assert(sqlite3_prepare_v2(db, sql, -1, &stmt, NULL) == SQLITE_OK);
    assert(sqlite3_step(stmt) == SQLITE_ROW);
    int value = sqlite3_column_int(stmt, 0);
    assert(sqlite3_finalize(stmt) == SQLITE_OK);
    return value;
}

static void test_sqlite(void) {
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    char *image = mmap(NULL, 0x2a00000, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(image != MAP_FAILED);
    // Vendor entry prologues, followed by ABI-correct tails into real SQLite 3.45.3.
    uint32_t open_code[] = {0xe52db004, 0xe3a03000, 0xe28db000, 0xe28bd000, 0xe49db004};
    uint32_t v2_code[] = {0xe52db004, 0xe28db000, 0xe28bd000, 0xe49db004};
    uint32_t exec_code[] = {0xe16d42f4, 0xe1a05002, 0xe1cd40d0, 0xe28dd024};
    emit_tail(image + 0x1cba858, open_code, 5, sqlite3_open);
    emit_tail(image + 0x1cba874, v2_code, 4, sqlite3_open_v2);
    emit_tail(image + 0x1c91f74, exec_code, 4, real_sqlite_exec);
    uint32_t *api = (void *)(image + 0x29980a4);
    api[70] = (uintptr_t)image + 0x1cba858;
    api[135] = (uintptr_t)image + 0x1cba874;
    api[55] = (uintptr_t)image + 0x1c91f74;
    image[0x1c91f74] ^= 1;
    install_sqlite_hooks((uintptr_t)image, page);
    assert(*(uint32_t *)(image + 0x1cba858) == open_code[0]);
    image[0x1c91f74] ^= 1;
    assert(!mprotect(image + 0x1cba000, page, PROT_READ | PROT_EXEC));
    assert(!mprotect(image + 0x1c91000, page, PROT_READ | PROT_EXEC));
    __builtin___clear_cache(image, image + 0x2a00000);
    api[55] = 0;
    install_sqlite_hooks((uintptr_t)image, page);
    assert(*(uint32_t *)(image + 0x1cba858) == open_code[0]);
    api[55] = (uintptr_t)image + 0x1c91f74;
    install_sqlite_hooks((uintptr_t)image, page);
    sqlite_open_fn open_fn = (void *)(image + 0x1cba858);
    sqlite_open_v2_fn v2_fn = (void *)(image + 0x1cba874);
    void *db = NULL;
    char path[128];
    snprintf(path, sizeof path, "/tmp/sc5000-hook-test-%ld.db", (long)getpid());
    unlink(path);
    assert(v2_fn(path, &db, SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE, "unix") == SQLITE_OK);
    assert(exec_count == 1);
    assert(pragma(db, "PRAGMA synchronous") == 1);
    assert(pragma(db, "PRAGMA temp_store") == 2);
    assert(pragma(db, "PRAGMA cache_size") == -8000);
    assert(sqlite3_exec(db, "CREATE TABLE t(v); INSERT INTO t VALUES(42);", NULL, NULL, NULL) == SQLITE_OK);
    assert(sqlite3_close(db) == SQLITE_OK);
    assert(open_fn(path, &db) == SQLITE_OK && exec_count == 2);
    assert(pragma(db, "SELECT v FROM t") == 42);
    assert(sqlite3_close(db) == SQLITE_OK);
    assert(v2_fn(path, &db, SQLITE_OPEN_READONLY, NULL) == SQLITE_OK && exec_count == 3);
    assert(pragma(db, "PRAGMA cache_size") == -8000);
    assert(sqlite3_close(db) == SQLITE_OK);
    unlink(path);
    const char *memory[] = {"/nonexistent/analytics/posthog.db", "/nonexistent/analytics/events.db", ":memory:"};
    for (size_t i = 0; i < sizeof memory / sizeof *memory; i++) {
        assert(v2_fn(memory[i], &db, SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE, NULL) == SQLITE_OK);
        assert(!strcmp(sqlite3_db_filename(db, "main"), ""));
        assert(sqlite3_close(db) == SQLITE_OK);
    }
    assert(exec_count == 3);
    assert(open_fn(memory[0], &db) == SQLITE_OK && exec_count == 3);
    assert(sqlite3_close(db) == SQLITE_OK);
    assert(v2_fn("/nonexistent/hook-test/m.db", &db, SQLITE_OPEN_READONLY, NULL) != SQLITE_OK);
    assert(exec_count == 3);
    if (db) sqlite3_close(db);
    db = NULL;
    assert(v2_fn(path, &db, SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE, "nonexistent-vfs") != SQLITE_OK);
    if (db) sqlite3_close(db);
    tune_database(SQLITE_OK, path, NULL);
    assert(exec_count == 3);
    munmap(image, 0x2a00000);
    puts("Installed ARM SQLite hooks, real PRAGMA readback, memory telemetry, errors/readonly: PASS");
}

static void test_files(void) {
    char path[PATH_MAX], target[PATH_MAX], mapped[PATH_MAX];
    snprintf(path, sizeof path, "/tmp/sc5000-hook-%ld/CrashReportDB/report.lock", (long)getpid());
    hook_test_engine = 1;
    const char *p = volatile_path(AT_FDCWD, path, mapped);
    assert(p == mapped && strstr(p, "/tmp/sc5000-engine-volatile-") == p);
    strcpy(target, mapped);
    assert(volatile_path(AT_FDCWD, target, mapped) == target);
    int (*openers[])(const char *, int, ...) = {hook_open, hook_open64};
    int fd;
    for (size_t i = 0; i < sizeof openers / sizeof *openers; i++) {
        fd = openers[i](path, O_CREAT | O_RDWR | O_TRUNC, 0600);
        assert(fd >= 0 && write(fd, "ok", 2) == 2);
        close(fd);
    }
    struct stat st;
    assert(stat(path, &st) == -1 && stat(target, &st) == 0 && st.st_size == 2);
    char *slash = strrchr(path, '/'); *slash = 0;
    // Resolve relative openat through an existing directory without bypassing redirection.
    char *dest_slash = strrchr(target, '/'); *dest_slash = 0;
    int dirfd = open(target, O_RDONLY | O_DIRECTORY);
    assert(dirfd >= 0);
    // An analytics directory used as dirfd must retain filename/flags/mode.
    char analytics[PATH_MAX];
    snprintf(analytics, sizeof analytics, "/tmp/sc5000-hook-%ld/analytics", (long)getpid());
    mkdir(path, 0700); // Missing original parents are intentional; do not create them.
    char root[128]; snprintf(root, sizeof root, "/tmp/sc5000-hook-%ld", (long)getpid());
    assert(!mkdir(root, 0700) || errno == EEXIST);
    assert(!mkdir(analytics, 0700));
    int ad = open(analytics, O_RDONLY | O_DIRECTORY);
    assert(ad >= 0);
    int (*at_openers[])(int, const char *, int, ...) = {hook_openat, hook_openat64};
    for (size_t i = 0; i < sizeof at_openers / sizeof *at_openers; i++) {
        fd = at_openers[i](ad, "events.db-journal", O_CREAT | O_WRONLY, 0640);
        assert(fd >= 0 && !fstat(fd, &st) && (st.st_mode & 0777) == 0640);
        close(fd);
    }
    assert(faccessat(ad, "events.db-journal", F_OK, 0) == -1);
    close(ad); close(dirfd);
    assert(volatile_path(AT_FDCWD, "/tmp/ordinary.db", mapped) != mapped);
    pid_t pid = 12345;
    char *argv[] = {"crashpad_handler", NULL};
    assert(posix_spawn(&pid, "/usr/bin/crashpad_handler", NULL, NULL, argv, environ) == ENOENT && pid == 12345);
    assert(execv("/usr/bin/crashpad_handler", argv) == -1 && errno == ENOENT);
    argv[0] = "true";
    assert(posix_spawn(&pid, "/bin/true", NULL, NULL, argv, environ) == 0);
    int status; assert(waitpid(pid, &status, 0) == pid && WIFEXITED(status) && !WEXITSTATUS(status));
    hook_test_engine = 0;
    assert(volatile_path(AT_FDCWD, "/data/analytics/posthog.db", mapped) != mapped);
    assert(hook_openat(-99, "events.db", O_RDONLY) == -1 && errno == EBADF);
    puts("File/openat64 isolation, mode/flags, Crashpad rejection and ordinary spawn: PASS");
}

static void *named_worker(void *unused) {
    (void)unused;
    assert(pthread_setname_np(pthread_self(), "Analysis Queue") == 0);
    cpu_set_t cpus; assert(pthread_getaffinity_np(pthread_self(), sizeof cpus, &cpus) == 0);
    assert(!CPU_ISSET(0, &cpus) && CPU_COUNT(&cpus) == 1);
    int policy; struct sched_param p;
    assert(!pthread_getschedparam(pthread_self(), &policy, &p) && policy == SCHED_IDLE);
    assert(getpriority(PRIO_PROCESS, 0) == 19);
    assert((syscall(SYS_ioprio_get, 1, 0) >> 13) == 3);
    return NULL;
}

static void *foreign_named_worker(void *fd) {
    char c;
    assert(read(*(int *)fd, &c, 1) == 1);
    cpu_set_t cpus; assert(!pthread_getaffinity_np(pthread_self(), sizeof cpus, &cpus));
    assert(!CPU_ISSET(0, &cpus));
    assert(getpriority(PRIO_PROCESS, 0) == 10);
    return NULL;
}

struct qt_thread_case { const char *name; int time64, kind; };

static void *qt_named_worker(void *arg) {
    const struct qt_thread_case *test = arg;
    int (*name_fn)(int, ...) = test->time64 ? hook_prctl_time64 : hook_prctl;
    assert(!name_fn(PR_SET_NAME, (unsigned long)test->name));
    char name[16];
    assert(!name_fn(PR_GET_NAME, (unsigned long)name));
    assert(!strcmp(name, test->name));
    struct sched_param normal = {.sched_priority = 0}, actual;
    // Qt applies its requested scheduling policy after setting the name.
    assert(!pthread_setschedparam(pthread_self(), SCHED_OTHER, &normal));
    int policy;
    assert(!pthread_getschedparam(pthread_self(), &policy, &actual));
    assert(policy == (test->kind == 2 ? SCHED_IDLE : SCHED_OTHER));
    assert(actual.sched_priority == 0);
    assert(getpriority(PRIO_PROCESS, 0) == (test->kind == 2 ? 19 : 10));
    cpu_set_t wanted = engine_cpus, got, cpu1;
    CPU_CLR(0, &wanted);
    if (test->kind == 2) {
        int last = CPU_SETSIZE - 1;
        while (!CPU_ISSET(last, &wanted)) last--;
        CPU_ZERO(&wanted); CPU_SET(last, &wanted);
        assert((syscall(SYS_ioprio_get, 1, 0) >> 13) == 3);
    }
    assert(!pthread_getaffinity_np(pthread_self(), sizeof got, &got));
    assert(CPU_EQUAL(&got, &wanted));
    CPU_ZERO(&cpu1); CPU_SET(1, &cpu1);
    assert(!pthread_setaffinity_np(pthread_self(), sizeof cpu1, &cpu1));
    assert(!pthread_getaffinity_np(pthread_self(), sizeof got, &got));
    assert(CPU_EQUAL(&got, &wanted));
    assert(name_fn(PR_SET_NAME, 0UL, 0UL, 0UL, 0UL) == -1 && errno == EFAULT);
    return NULL;
}

static void test_threads(void) {
    hook_test_engine = 1;
    cpu_set_t saved, cpu1;
    assert(!sched_getaffinity(0, sizeof saved, &saved) && CPU_ISSET(1, &saved));
    CPU_ZERO(&cpu1); CPU_SET(1, &cpu1);
    assert(!sched_setaffinity(0, sizeof cpu1, &cpu1));
    initialize_engine_cpus();
    long online = sysconf(_SC_NPROCESSORS_ONLN);
    assert(online > 1 && online <= CPU_SETSIZE);
    assert(CPU_COUNT(&engine_cpus) == online && CPU_ISSET((int)online - 1, &engine_cpus));
    int before = getpriority(PRIO_PROCESS, 0);
    pthread_t thread;
    assert(!pthread_create(&thread, NULL, named_worker, NULL));
    assert(!pthread_join(thread, NULL));
    assert(getpriority(PRIO_PROCESS, 0) == before);
    int fds[2]; assert(!pipe(fds));
    assert(!pthread_create(&thread, NULL, foreign_named_worker, &fds[0]));
    assert(!pthread_setname_np(thread, "QQuickPixmapRea"));
    assert(getpriority(PRIO_PROCESS, 0) == before);
    assert(write(fds[1], "x", 1) == 1);
    assert(!pthread_join(thread, NULL));
    close(fds[0]); close(fds[1]);
    const struct qt_thread_case cases[] = {
        {"Analysis Queue ", 0, 2}, {"Freeze Checker ", 1, 2},
        {"QQuickPixmapRea", 0, 1}, {"FileInfoThread", 1, 1},
    };
    for (size_t i = 0; i < sizeof cases / sizeof *cases; i++) {
        assert(!pthread_create(&thread, NULL, qt_named_worker, (void *)&cases[i]));
        assert(!pthread_join(thread, NULL));
    }
    cpu_set_t main_cpus;
    assert(!sched_getaffinity(0, sizeof main_cpus, &main_cpus) && CPU_EQUAL(&main_cpus, &cpu1));
    assert(getpriority(PRIO_PROCESS, 0) == before);
    assert(hook_prctl_time64(PR_GET_DUMPABLE, 0UL, 0UL, 0UL, 0UL) >= 0);
    assert(thread_class("AudioOut") == 0 && thread_class("AudioRenderer") == 0);
    assert(thread_class("QQuickPixmapRea") == 1 && thread_class("Freeze Checker") == 2);
    hook_test_engine = 0;
    struct sched_param normal = {.sched_priority = 0};
    assert(!pthread_setschedparam(pthread_self(), SCHED_OTHER, &normal));
    assert(!sched_setaffinity(0, sizeof saved, &saved));
    engine_cpus_ready = 0;
    puts("pthread/prctl/time64 naming, Qt policy override, online CPUs and caller isolation: PASS");
}

struct mock_sw_params { int type; unsigned long start, stop, silence_threshold, silence_size; };
static int mock_get_rc, mock_set_rc, mock_apply_rc, mock_seen_type;
static char alsa_order[16];
static unsigned alsa_count;
static void *mock_pcm;

static int mock_tstamp_get(const void *params, int *type) {
    alsa_order[alsa_count++] = 'g';
    if (!mock_get_rc) *type = ((const struct mock_sw_params *)params)->type;
    return mock_get_rc;
}

static int mock_tstamp_set(void *pcm, void *params, int type) {
    assert(pcm == mock_pcm);
    alsa_order[alsa_count++] = 's';
    mock_seen_type = type;
    if (!mock_set_rc) ((struct mock_sw_params *)params)->type = type;
    return mock_set_rc;
}

static int mock_sw_apply(void *pcm, void *params) {
    assert(pcm == mock_pcm && params);
    alsa_order[alsa_count++] = 'a';
    return mock_apply_rc;
}

static void test_alsa(void) {
    void *old_sw = alsa_sw_cache, *old_set = alsa_tstamp_set_cache, *old_get = alsa_tstamp_get_cache;
    alsa_sw_cache = mock_sw_apply;
    alsa_tstamp_set_cache = mock_tstamp_set;
    alsa_tstamp_get_cache = mock_tstamp_get;
    char pcm; mock_pcm = &pcm;
    hook_test_engine = 1;
    for (int type = 0; type <= 2; type++) {
        struct mock_sw_params params, wanted;
        memset(&params, 0x5a, sizeof params);
        params.type = type; params.silence_threshold = 0;
        memcpy(&wanted, &params, sizeof wanted);
        wanted.type = type == 0 ? 2 : type;
        alsa_count = 0; memset(alsa_order, 0, sizeof alsa_order);
        assert(snd_pcm_sw_params(&pcm, &params) == 0);
        assert(!strcmp(alsa_order, type == 0 ? "gsa" : "ga"));
        assert(!memcmp(&params, &wanted, sizeof params));
    }
    struct mock_sw_params params = {.type = 0, .silence_threshold = 0};
    alsa_count = 0; mock_set_rc = -EINVAL;
    assert(snd_pcm_sw_params(&pcm, &params) == -EINVAL && alsa_count == 2 && params.type == 0);
    mock_set_rc = 0; mock_get_rc = -EINVAL; mock_apply_rc = -EBUSY; alsa_count = 0;
    assert(snd_pcm_sw_params(&pcm, &params) == -EBUSY && alsa_count == 2 && params.type == 0);
    mock_get_rc = 0; mock_apply_rc = 0;
    for (int type = -1; type <= 3; type++) {
        alsa_count = 0;
        assert(!snd_pcm_sw_params_set_tstamp_type(&pcm, &params, type));
        assert(mock_seen_type == (type == 0 ? 2 : type));
    }
    hook_test_engine = 0; params.type = 0; alsa_count = 0;
    assert(!snd_pcm_sw_params(&pcm, &params) && alsa_count == 1 && params.type == 0);
    alsa_count = 0;
    assert(!snd_pcm_sw_params_set_tstamp_type(&pcm, &params, 0) && mock_seen_type == 0);
    alsa_sw_cache = old_sw; alsa_tstamp_set_cache = old_set; alsa_tstamp_get_cache = old_get;
    puts("ALSA setup: realtime -> RAW, preserve explicit clocks/silence, call order and errors: PASS");
}

static int dns_calls;
static const char *dns_node, *dns_service;
static const struct addrinfo *dns_hints;

static int mock_dns(const char *node, const char *service, const struct addrinfo *hints, struct addrinfo **result) {
    assert(node == dns_node && service == dns_service && hints == dns_hints);
    dns_calls++;
    *result = NULL;
    return EAI_AGAIN;
}

static void test_update_dns(void) {
    void *old = getaddrinfo_cache; getaddrinfo_cache = mock_dns;
    hook_test_engine = 1;
    const char *blocked[] = {"autoupdate.airmusictech.com", "AUTOUPDATE.AIRMUSICTECH.COM", "autoupdate.airmusictech.com."};
    struct addrinfo hints = {.ai_family = AF_UNSPEC}, *result;
    for (size_t i = 0; i < sizeof blocked / sizeof *blocked; i++) {
        result = &hints;
        assert(getaddrinfo(blocked[i], "443", &hints, &result) == EAI_NONAME && !result);
    }
    assert(!dns_calls);
    const char *allowed[] = {"device.inmusicbrands.com", "auth.profile.inmusicbrands.com",
        "ipv4.connman.inmusicbrands.com", "api.github.com", "notautoupdate.airmusictech.com",
        "autoupdate.airmusictech.com.example", NULL};
    dns_service = "443"; dns_hints = &hints;
    for (size_t i = 0; i < sizeof allowed / sizeof *allowed; i++) {
        dns_node = allowed[i];
        assert(getaddrinfo(dns_node, dns_service, dns_hints, &result) == EAI_AGAIN);
    }
    hook_test_engine = 0; dns_node = blocked[0];
    assert(getaddrinfo(dns_node, dns_service, dns_hints, &result) == EAI_AGAIN);
    assert(dns_calls == (int)(sizeof allowed / sizeof *allowed) + 1);
    getaddrinfo_cache = old;
    puts("Update DNS: exact official updater host blocked; auth/GitHub/connman/other processes preserved: PASS");
}

static void test_patch_files(const char *filename) {
    FILE *f = fopen(filename, "rb"); assert(f);
    assert(!fseek(f, 0, SEEK_END)); long size = ftell(f); assert(size > 0);
    rewind(f); char *image = malloc((size_t)size); assert(image);
    assert(fread(image, 1, (size_t)size, f) == (size_t)size); fclose(f);
    ElfW(Ehdr) *header = (void *)image;
    struct dl_phdr_info info = {.dlpi_addr = (uintptr_t)image,
        .dlpi_phdr = (void *)(image + header->e_phoff), .dlpi_phnum = header->e_phnum};
    uintptr_t found = 0;
    engine_base(&info, sizeof info, &found);
    assert(found == (uintptr_t)image);
    char id[] = "\xaf\x07\x38\x65\xa3\x31\x8d\x80\x7a\x83\x03\xf7\x16\x15\x2e\xac\x92\x3a\x4c\x6f";
    char *at = memmem(image, (size_t)size, id, 20); assert(at);
    *at ^= 1; found = 0;
    engine_base(&info, sizeof info, &found);
    assert(found == 0);
    *at ^= 1;
    for (size_t i = 0; i < sizeof engine_edits / sizeof *engine_edits; i++) {
        const struct engine_edit *e = &engine_edits[i];
        assert(e->offset + e->size <= (size_t)size);
        int state = edit_state((uintptr_t)image, e);
        assert(state == 1 || state == 2);
        memcpy(image + e->offset, e->replacement, e->size);
        assert(edit_state((uintptr_t)image, e) == 2);
        image[e->offset] ^= 0xff;
        assert(edit_state((uintptr_t)image, e) == 0);
    }
    free(image);
    printf("Stock/patched/mismatched edit classification: %s PASS\n", filename);
}

int main(int argc, char **argv) {
    test_stems();
    test_config();
    test_sqlite();
    test_files();
    test_threads();
    test_alsa();
    test_update_dns();
    for (int i = 1; i < argc; i++) test_patch_files(argv[i]);
    return 0;
}
