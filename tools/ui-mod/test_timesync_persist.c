// Unit tests for Engine timezone & monotonic clock persistence (timesync_persist.h).
//   gcc -O2 -Wall -Wextra -o test_timesync_persist test_timesync_persist.c -ldl -lpthread && ./test_timesync_persist
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>

static void write_file(const char *path, const char *text) {
    assert(ts_mkdirs_parent(path, 0755) == 0);
    FILE *f = fopen(path, "w");
    assert(f && fputs(text, f) >= 0 && fclose(f) == 0);
}

static void read_file(const char *path, char *out, size_t cap) {
    FILE *f = fopen(path, "r");
    assert(f != NULL);
    size_t n = fread(out, 1, cap - 1, f);
    fclose(f);
    out[n] = 0;
}

int main(void) {
    char root[] = "/tmp/enginex-ts-test-XXXXXX";
    assert(mkdtemp(root) != NULL);

    // 1. Seed fake zoneinfo files so test never depends on host /usr/share/zoneinfo layout
    char zdir[256], zsha[PATH_MAX], zlon[PATH_MAX], zny[PATH_MAX], zutc[PATH_MAX];
    snprintf(zdir, sizeof zdir, "%s/zoneinfo", root);
    snprintf(zsha, sizeof zsha, "%s/Asia/Shanghai", zdir);
    snprintf(zlon, sizeof zlon, "%s/Europe/London", zdir);
    snprintf(zny, sizeof zny, "%s/America/New_York", zdir);
    snprintf(zutc, sizeof zutc, "%s/Etc/UTC", zdir);
    write_file(zsha, "TZif2");
    write_file(zlon, "TZif2");
    write_file(zny, "TZif2");
    write_file(zutc, "TZif2");

    char droot[256], conf[PATH_MAX], etc_ts[PATH_MAX], clk[PATH_MAX];
    snprintf(droot, sizeof droot, "%s/data", root);
    snprintf(conf, sizeof conf, "%s/.config/jp07.user.settings/AIR Music Technology/Engine.conf", droot);
    snprintf(etc_ts, sizeof etc_ts, "%s/timestamp", root);
    snprintf(clk, sizeof clk, "%s/varlib/systemd/timesync/clock", root);

    char edir[PATH_MAX];
    snprintf(edir, sizeof edir, "%s/enginex", droot);
    assert(mkdir(droot, 0755) == 0 && mkdir(edir, 0755) == 0);

    hook_test_engine = 0;
    assert(setenv("ENGINEX_TEST_ZONEINFO_DIR", zdir, 1) == 0);
    assert(setenv("ENGINEX_TEST_DATA_ROOT", droot, 1) == 0);
    assert(setenv("ENGINEX_TEST_ETC_TIMESTAMP", etc_ts, 1) == 0);
    assert(setenv("ENGINEX_TEST_CLOCK_FILE", clk, 1) == 0);
    unsetenv("TZ");

    // 2. Validate valid_iana_tz rules (340 Region/City zones ok; UTC/Etc/UTC/GMT/Universal/traversal rejected)
    assert(valid_iana_tz("Asia/Shanghai") == 1);
    assert(valid_iana_tz("Europe/London") == 1);
    assert(valid_iana_tz("UTC") == 0);
    assert(valid_iana_tz("Universal") == 0);
    assert(valid_iana_tz("GMT") == 0);
    assert(valid_iana_tz("Etc/UTC") == 0);
    assert(valid_iana_tz("../Asia/Shanghai") == 0);
    assert(valid_iana_tz("/Asia/Shanghai") == 0);
    assert(valid_iana_tz("") == 0);
    assert(valid_iana_tz("Asia/Nonexistent_City") == 0);

    // 3. Simulate Engine probing QSettings path via access(), with existing 24HourDisplay setting
    write_file(conf, "[General]\nFoo=1\n\n[TimeDateSettings]\n24HourDisplay=true\n\n[Audio]\nMaster=0\n");
    hook_test_engine = 1;
    g_qsettings_conf[0] = 0;
    assert(access(conf, R_OK) == 0);
    assert(!strcmp(g_qsettings_conf, conf));

    // User chooses Asia/Shanghai -> setenv("TZ", "Asia/Shanghai", 1), then hard power-off (no destructor!)
    assert(setenv("TZ", "Asia/Shanghai", 1) == 0);
    assert(!strcmp(getenv("TZ"), "Asia/Shanghai"));

    char buf[4096];
    read_file(conf, buf, sizeof buf);
    assert(strstr(buf, "[General]\nFoo=1\n") != NULL);
    assert(strstr(buf, "24HourDisplay=true\n") != NULL);
    assert(strstr(buf, "TZEnvVar=Asia/Shanghai\n") != NULL);
    assert(strstr(buf, "[Audio]\nMaster=0\n") != NULL);

    char stz_file[PATH_MAX];
    snprintf(stz_file, sizeof stz_file, "%s/enginex/state/timezone", droot);
    read_file(stz_file, buf, sizeof buf);
    assert(!strcmp(buf, "Asia/Shanghai\n"));

    // 4. Failed or fallback auto-detect must NEVER overwrite a valid chosen/saved zone
    assert(setenv("TZ", "UTC", 1) == 0);
    assert(!strcmp(getenv("TZ"), "Asia/Shanghai"));
    assert(setenv("TZ", "Etc/UTC", 1) == 0);
    assert(!strcmp(getenv("TZ"), "Asia/Shanghai"));
    assert(setenv("TZ", "Universal", 1) == 0);
    assert(!strcmp(getenv("TZ"), "Asia/Shanghai"));
    assert(setenv("TZ", "", 1) == 0);
    assert(!strcmp(getenv("TZ"), "Asia/Shanghai"));
    assert(unsetenv("TZ") == 0);
    assert(!strcmp(getenv("TZ"), "Asia/Shanghai"));

    // Switching to another valid Region/City zone updates both Engine.conf and state/timezone
    assert(setenv("TZ", "Europe/London", 1) == 0);
    assert(!strcmp(getenv("TZ"), "Europe/London"));
    read_file(conf, buf, sizeof buf);
    assert(strstr(buf, "TZEnvVar=Europe/London\n") != NULL);
    assert(strstr(buf, "Asia/Shanghai") == NULL);

    // 5. Simulate cold offline boot: clear RAM TZ, call restore_persisted_timezone()
    hook_test_engine = 0;
    unsetenv("TZ");
    g_qsettings_conf[0] = 0;
    hook_test_engine = 1;
    restore_persisted_timezone();
    assert(getenv("TZ") && !strcmp(getenv("TZ"), "Europe/London"));

    // 6. Monotonic clock persistence: creates clock file, respects /etc/timestamp floor, never moves backwards
    write_file(etc_ts, "20261030114129\n");
    persist_monotonic_clock();
    struct stat st1, st2;
    assert(stat(clk, &st1) == 0);
    time_t now_s = time(NULL);
    assert(st1.st_mtim.tv_sec >= 1761824489 && st1.st_mtim.tv_sec >= now_s - 2);

    // Seed future timestamp (2028-01-01) -> persist_monotonic_clock must NOT move mtime backwards to now
    struct timespec future[2] = {{1830297600, 0}, {1830297600, 0}};
    assert(utimensat(AT_FDCWD, clk, future, 0) == 0);
    persist_monotonic_clock();
    assert(stat(clk, &st2) == 0);
    assert(st2.st_mtim.tv_sec == 1830297600);

    // Symlink clock target must be refused
    char evil_target[PATH_MAX], sym_clk[PATH_MAX];
    snprintf(evil_target, sizeof evil_target, "%s/precious", root);
    snprintf(sym_clk, sizeof sym_clk, "%s/sym-clock", root);
    write_file(evil_target, "keep");
    struct timespec old_ts[2] = {{1700000005, 0}, {1700000005, 0}};
    assert(utimensat(AT_FDCWD, evil_target, old_ts, 0) == 0);
    assert(symlink(evil_target, sym_clk) == 0);
    hook_test_engine = 0;
    setenv("ENGINEX_TEST_CLOCK_FILE", sym_clk, 1);
    hook_test_engine = 1;
    persist_monotonic_clock();
    assert(stat(evil_target, &st2) == 0 && st2.st_mtim.tv_sec == 1700000005);

    hook_test_engine = 0;
    char cmd[PATH_MAX + 32];
    snprintf(cmd, sizeof cmd, "rm -rf %s", root);
    assert(system(cmd) == 0);
    puts("timesync_persist: PASS");
    return 0;
}
