// Checks for the second-generation *-Update.img hooks and the Factory Reset keep-EngineX erase (#119).
// Native on the build host (needs sha256sum):
//   gcc -O2 -Wall -o test_gen2_img test_gen2_img.c -ldl -lpthread && ./test_gen2_img
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>
#include <sys/mount.h>

static void put(const char *dir, const char *rel, const char *text) {
    char path[PATH_MAX];
    snprintf(path, sizeof path, "%s/%s", dir, rel);
    for (char *p = strchr(path + strlen(dir) + 1, '/'); p; p = strchr(p + 1, '/')) {
        *p = 0; mkdir(path, 0755); *p = '/';
    }
    FILE *f = fopen(path, "w"); assert(f && fputs(text, f) >= 0 && !fclose(f));
}

static int exists(const char *dir, const char *rel) {
    char path[PATH_MAX];
    snprintf(path, sizeof path, "%s/%s", dir, rel);
    return access(path, F_OK) == 0;
}

// header + payload, the way build-data-bundle.py writes it; hex_override replaces the real digest
static void make_img(const char *dir, const char *name, const char *payload, const char *hex_override, size_t cut) {
    char pay[PATH_MAX], cmd[PATH_MAX + 32], hex[65] = {0}, img[PATH_MAX];
    snprintf(pay, sizeof pay, "%s/payload", dir);
    FILE *f = fopen(pay, "w"); assert(f && fputs(payload, f) >= 0 && !fclose(f));
    snprintf(cmd, sizeof cmd, "sha256sum < %s", pay);
    FILE *p = popen(cmd, "r"); assert(p && fread(hex, 1, 64, p) == 64); pclose(p);
    uint8_t head[ENGINEX_IMG_HEADER] = {0};
    snprintf((char *)head, sizeof head, ENGINEX_IMG_MAGIC "%s\nSCLIVE4\n5.3.0.3\n", hex_override ? hex_override : hex);
    snprintf(img, sizeof img, "%s/%s", dir, name);
    f = fopen(img, "w"); assert(f);
    fwrite(head, 1, sizeof head, f);
    size_t n = strlen(payload) - cut;
    fwrite(payload, 1, n, f);
    fclose(f);
}

static void test_sha256(void) {
    sha256_ctx c;
    char hex[65];
    sha256_init(&c);
    sha256_final(&c, hex);
    assert(!strcmp(hex, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"));
    sha256_init(&c);
    for (int i = 0; i < 1000; i++) sha256_update(&c, (const uint8_t *)"abcdefghij", 10);  // crosses block edges
    sha256_final(&c, hex);
    char want[65] = {0};
    FILE *p = popen("i=0; while [ $i -lt 1000 ]; do printf abcdefghij; i=$((i+1)); done | sha256sum", "r");
    assert(p && fread(want, 1, 64, p) == 64); pclose(p);
    assert(!strcmp(hex, want));
    // 56-byte boundary: forces 0x80 at index 56 and wraps length into a second block
    static const char msg56[] = "0123456789abcdef0123456789abcdef0123456789abcdef01234567";
    sha256_init(&c);
    sha256_update(&c, (const uint8_t *)msg56, 56);
    sha256_final(&c, hex);
    p = popen("printf '%s' '0123456789abcdef0123456789abcdef0123456789abcdef01234567' | sha256sum", "r");
    assert(p && fread(want, 1, 64, p) == 64); pclose(p);
    assert(!strcmp(hex, want));
    puts("sha256: PASS");
}

// The device tree of the deck under test (NUL-separated compatible strings).
static char compat_file[PATH_MAX];
static void board(const char *dir, const char *compatible) {
    snprintf(compat_file, sizeof compat_file, "%s/compatible", dir);
    FILE *f = fopen(compat_file, "wb");
    assert(f && fwrite(compatible, 1, strlen(compatible) + 1, f) == strlen(compatible) + 1);
    assert(fwrite("rockchip,rk3288", 1, 16, f) == 16 && !fclose(f));
    enginex_compatible_path = compat_file;
}

static void test_open_verify(void) {
    char dir[] = "/tmp/gen2-img-XXXXXX", img[PATH_MAX], cmd[PATH_MAX * 2 + 64];
    assert(mkdtemp(dir));
    board(dir, "inmusic,jp21"); // an SC LIVE 4: the images below are SCLIVE4 images
    snprintf(img, sizeof img, "%s/ok.img", dir);
    make_img(dir, "ok.img", "payload-bytes-0123456789", NULL, 0);
    void *h = NULL;
    assert(az01_open_image(img, NULL) == ENGINEX_ERR_BAD_IMAGE);
    assert(az01_open_image(img, &h) == 0 && h == enginex_dummy_img);
    assert(az01_image_set_onboard_key_lookup(h) == 0);
    assert(az01_verify_image(h, 0) == 0);
    assert(az01_close_image(h) == 0);
    assert(az01_verify_image(h, 0) == ENGINEX_ERR_BAD_IMAGE); // closed handle cannot verify again

    // gen2-locked must reject both verify and apply even on a valid image
    setenv("ENGINEX_TEST_DATA_ROOT", dir, 1);
    put(dir, "enginex/gen2-locked", "1\n");
    assert(az01_open_image(img, &h) == 0 && az01_verify_image(h, 0) == ENGINEX_ERR_BAD_IMAGE);
    assert(az01_apply_image(img, 0) == ENGINEX_ERR_BAD_IMAGE);
    snprintf(cmd, sizeof cmd, "%s/enginex/gen2-locked", dir);
    assert(unlink(cmd) == 0);

    // Valid bundle .tar.gz inside ENGINEX_DATA_BUNDLE_IMG_V1: az01_apply_image unpacks, runs install.sh, and unlinks when flags=1
    put(dir, "pkg/top/install.sh", "#!/bin/sh\nprintf '5.3.0.3\\n' > \"$ENGINEX_CUSTOM/applied-version.txt\"\n");
    snprintf(cmd, sizeof cmd, "chmod +x %s/pkg/top/install.sh && tar -C %s/pkg -czf %s/bundle.tar.gz top", dir, dir, dir);
    assert(system(cmd) == 0);
    char hex[65] = {0};
    snprintf(cmd, sizeof cmd, "sha256sum < %s/bundle.tar.gz", dir);
    FILE *p = popen(cmd, "r"); assert(p && fread(hex, 1, 64, p) == 64); pclose(p);
    uint8_t head[ENGINEX_IMG_HEADER] = {0};
    snprintf((char *)head, sizeof head, ENGINEX_IMG_MAGIC "%s\nSCLIVE4\n5.3.0.3\n", hex);
    snprintf(img, sizeof img, "%s/apply.img", dir);
    FILE *f = fopen(img, "wb"); assert(f && fwrite(head, 1, sizeof head, f) == sizeof head && !fclose(f));
    snprintf(cmd, sizeof cmd, "cat %s/bundle.tar.gz >> %s/apply.img", dir, dir);
    assert(system(cmd) == 0);
    assert(az01_apply_image(img, 1) == 0);
    assert(!exists(dir, "apply.img") && exists(dir, "enginex/applied-version.txt"));
    unsetenv("ENGINEX_TEST_DATA_ROOT");

    make_img(dir, "flip.img", "payload-bytes-0123456789", "0000000000000000000000000000000000000000000000000000000000000000", 0);
    snprintf(img, sizeof img, "%s/flip.img", dir);
    assert(az01_open_image(img, &h) == 0 && h == enginex_dummy_img && az01_verify_image(h, 0) == ENGINEX_ERR_BAD_IMAGE);
    make_img(dir, "cut.img", "payload-bytes-0123456789", NULL, 1);  // truncated after the hash was taken
    snprintf(img, sizeof img, "%s/cut.img", dir);
    assert(az01_open_image(img, &h) == 0 && az01_verify_image(h, 0) == ENGINEX_ERR_BAD_IMAGE);
    assert(az01_apply_image(img, 0) == ENGINEX_ERR_BAD_IMAGE);  // apply re-checks, never trusts verify
    put(dir, "legacy.img", "ENGINEX_DATA_BUNDLE without a V1 header");  // no checksum: refused
    snprintf(img, sizeof img, "%s/legacy.img", dir);
    assert(enginex_img_check(img) == IMG_BAD);
    put(dir, "other.img", "AZ01 not ours");
    snprintf(img, sizeof img, "%s/other.img", dir);
    assert(enginex_img_check(img) == IMG_NONE && enginex_img_check("/nonexistent") == IMG_NONE);
    snprintf(cmd, sizeof cmd, "rm -rf %s", dir);
    assert(system(cmd) == 0);
    puts("open/verify/close/apply checks: PASS");
}

// BH-G2-1: the native check compared only the payload digest, so an intact image for
// another model and CPU (SYSTEM ONE, arm64) verified and installed on an SC LIVE 4 (armhf).
static void with_prefix(const char *dir, const char *name, const char *prefix_line) {
    char pay[PATH_MAX], cmd[PATH_MAX + 32], hex[65] = {0}, img[PATH_MAX];
    snprintf(pay, sizeof pay, "%s/payload", dir);
    FILE *f = fopen(pay, "w"); assert(f && fputs("payload-bytes-0123456789", f) >= 0 && !fclose(f));
    snprintf(cmd, sizeof cmd, "sha256sum < %s", pay);
    FILE *p = popen(cmd, "r"); assert(p && fread(hex, 1, 64, p) == 64); pclose(p);
    uint8_t head[ENGINEX_IMG_HEADER];
    memset(head, 0, sizeof head);
    int n = snprintf((char *)head, sizeof head, ENGINEX_IMG_MAGIC "%s\n%s", hex, prefix_line);
    if (n < (int)sizeof head && !strchr(prefix_line, '\n')) memset(head + n, 'X', sizeof head - (size_t)n); // no newline, no NUL
    snprintf(img, sizeof img, "%s/%s", dir, name);
    f = fopen(img, "w"); assert(f);
    fwrite(head, 1, sizeof head, f);
    fputs("payload-bytes-0123456789", f);
    fclose(f);
}

static void test_other_model_refused(void) {
    char dir[] = "/tmp/gen2-model-XXXXXX", img[PATH_MAX], cmd[PATH_MAX + 16];
    assert(mkdtemp(dir));
    setenv("ENGINEX_TEST_DATA_ROOT", dir, 1);
    void *h = NULL;
    with_prefix(dir, "systemone.img", "SYSTEMONE\n5.3.0.5\n");
    snprintf(img, sizeof img, "%s/systemone.img", dir);
    board(dir, "inmusic,jp21"); // SC LIVE 4 (armhf) and a SYSTEM ONE (arm64) image
    assert(enginex_img_check(img) == IMG_BAD);
    assert(az01_open_image(img, &h) == 0 && az01_verify_image(h, 0) == ENGINEX_ERR_BAD_IMAGE && az01_close_image(h) == 0);
    assert(az01_apply_image(img, 0) == ENGINEX_ERR_BAD_IMAGE && !exists(dir, "enginex/bundle"));
    board(dir, "inmusic,rmz2"); // the same image on a SYSTEM ONE
    assert(enginex_img_check(img) == IMG_OK);
    with_prefix(dir, "sclive4.img", "SCLIVE4\n5.3.0.5\n");
    snprintf(img, sizeof img, "%s/sclive4.img", dir);
    board(dir, "inmusic,jp07"); // a first-generation SC5000 never takes a Gen-2 image
    assert(enginex_img_check(img) == IMG_BAD);
    board(dir, "vendor,unknown"); // not a deck we know: refused, never guessed
    assert(enginex_img_check(img) == IMG_BAD);
    enginex_compatible_path = "/nonexistent/compatible";
    assert(enginex_img_check(img) == IMG_BAD);
    board(dir, "inmusic,jp21");
    assert(enginex_img_check(img) == IMG_OK);
    with_prefix(dir, "noline.img", "SCLIVE4"); // prefix line without its newline, header without a NUL
    snprintf(img, sizeof img, "%s/noline.img", dir);
    assert(enginex_img_check(img) == IMG_BAD);
    unsetenv("ENGINEX_TEST_DATA_ROOT");
    snprintf(cmd, sizeof cmd, "rm -rf %s", dir);
    assert(system(cmd) == 0);
    puts("BH-G2-1: an image for another model or CPU, or on an unknown board, is refused: PASS");
}

static void test_erase(void) {
    char root[] = "/tmp/gen2-erase-XXXXXX", cmd[PATH_MAX + 16];
    assert(mkdtemp(root));
    static const char *const wiped[] = {
        "AppDataUser/x.db", "Engine Library/m.db", "StreamingAccounts.json", "Wi-Fi Drop/a.wav", "az01-update.img",
        "enginex/state/tokens.json", "enginex/mods/m1/mod.json", "enginex/Stream/track.flac", "enginex/shadow-v3/a",
        "enginex/ui-scale.env", "enginex/ui/settings.conf", "enginex/ui/views.json",
        "enginex/ui/mods-overlay/a.qml", "enginex/ui/mods-quarantined/b", "enginex/ui/safe-mode", "enginex/ui/boot-count",
        "enginex/ui/boot-token", "enginex/ui/last-crash", "enginex/ui/running-sentinel"};
    static const char *const kept[] = {
        "enginex/bundle/boot-activate.sh", "enginex/bundle/bin/enginex-suite", "enginex/ui/packs/a/Main.qml", "enginex/ui/icons/x.svg",
        "enginex/version.txt", "enginex/gen2-unlocked", "enginex/authorized_keys", "enginex/ssh_host_ed25519_key",
        "system/var-lib/alsa/conf.d/99-sc5000-custom.conf"};
    for (size_t i = 0; i < sizeof wiped / sizeof *wiped; i++) put(root, wiped[i], "x");
    for (size_t i = 0; i < sizeof kept / sizeof *kept; i++) put(root, kept[i], "x");
    char link[PATH_MAX];
    snprintf(link, sizeof link, "%s/sc5000-custom", root);
    assert(symlink("enginex", link) == 0);
    snprintf(link, sizeof link, "%s/AppDataUser/out", root);  // a symlink out of /data is removed, never followed
    char outside[] = "/tmp/gen2-outside-XXXXXX";
    assert(mkdtemp(outside));
    put(outside, "precious", "x");
    assert(symlink(outside, link) == 0);

    setenv("ENGINEX_TEST_DATA_ROOT", root, 1);
    setenv("ENGINEX_TEST_PASSTHROUGH_ERASE", "1", 1);  // passthrough: no stock function here -> -1, nothing wiped
    assert(az01_erase_data_partition() == -1 && exists(root, "AppDataUser/x.db"));
    unsetenv("ENGINEX_TEST_PASSTHROUGH_ERASE");
    if (access("/usr/share/enginex/enginex-suite.xz", F_OK) == 0) { puts("erase: SKIP (1st-gen rootfs host)"); return; }
    assert(az01_erase_data_partition() == 0);
    for (size_t i = 0; i < sizeof wiped / sizeof *wiped; i++) assert(!exists(root, wiped[i]));
    for (size_t i = 0; i < sizeof kept / sizeof *kept; i++) assert(exists(root, kept[i]));
    assert(exists(root, "sc5000-custom") && exists(root, "enginex/state") && exists(root, "enginex/mods"));
    assert(exists(outside, "precious"));

    // a 1st-gen layout (no bundle/install.sh) is handed to the stock function
    snprintf(cmd, sizeof cmd, "rm -rf %s/enginex/bundle", root);
    assert(system(cmd) == 0);
    put(root, "AppDataUser/y.db", "x");
    assert(az01_erase_data_partition() == -1 && exists(root, "AppDataUser/y.db"));
    unsetenv("ENGINEX_TEST_DATA_ROOT");
    snprintf(cmd, sizeof cmd, "rm -rf %s %s", root, outside);
    assert(system(cmd) == 0);
    puts("Factory Reset keeps EngineX: PASS");
}

// BH-G2-6: a /data that cannot be written (read-only or error-remounted) left every user file in place
// and the reset still returned 0 and rebooted. It must report the failure and skip the reboot.
static void test_erase_reports_failure(void) {
    char root[] = "/tmp/gen2-erase-ro-XXXXXX", cmd[PATH_MAX + 16];
    assert(mkdtemp(root));
    put(root, "enginex/bundle/install.sh", "x");
    put(root, "StreamingAccounts.json", "x");
    put(root, "AppDataUser/x.db", "x");
    if (access("/usr/share/enginex/enginex-suite.xz", F_OK) == 0) { puts("erase failure: SKIP (1st-gen rootfs host)"); return; }
    fflush(NULL);
    pid_t child = fork();
    assert(child >= 0);
    if (!child) {
        // Own mount namespace: the read-only bind exists only in this child.
        if (unshare(CLONE_NEWNS) != 0 || mount(NULL, "/", NULL, MS_REC | MS_PRIVATE, NULL) != 0) _exit(77);
        if (mount(root, root, NULL, MS_BIND, NULL) != 0 || mount(NULL, root, NULL, MS_BIND | MS_REMOUNT | MS_RDONLY, NULL) != 0) _exit(77);
        setenv("ENGINEX_TEST_DATA_ROOT", root, 1);
        int rc = az01_erase_data_partition();
        _exit(rc == -1 && exists(root, "StreamingAccounts.json") && exists(root, "AppDataUser/x.db") ? 0 : 1);
    }
    int status;
    assert(waitpid(child, &status, 0) == child && WIFEXITED(status));
    snprintf(cmd, sizeof cmd, "rm -rf %s", root);
    assert(system(cmd) == 0);
    if (WEXITSTATUS(status) == 77) { puts("erase failure: SKIP (needs root for a private mount namespace)"); return; }
    assert(WEXITSTATUS(status) == 0);
    puts("Factory Reset on a /data it cannot write reports failure, keeps the files, does not reboot (BH-G2-6): PASS");
}

// #234: after a good install from a USB image, this release's leave step (/run/enginex/leave.sh,
// written by its boot-activate.sh) runs before the reboot, with --no-reboot: the hook reboots.
// A failed install leaves the running release alone: no leave step.
static void apply_with(const char *dir, const char *install, int want) {
    char cmd[PATH_MAX * 3], img[PATH_MAX], hex[65] = {0};
    snprintf(cmd, sizeof cmd, "rm -rf %s/pkg %s/bundle.tar.gz", dir, dir);
    assert(system(cmd) == 0);
    put(dir, "pkg/top/install.sh", install);
    snprintf(cmd, sizeof cmd, "chmod +x %s/pkg/top/install.sh && tar -C %s/pkg -czf %s/bundle.tar.gz top", dir, dir, dir);
    assert(system(cmd) == 0);
    snprintf(cmd, sizeof cmd, "sha256sum < %s/bundle.tar.gz", dir);
    FILE *p = popen(cmd, "r"); assert(p && fread(hex, 1, 64, p) == 64); pclose(p);
    uint8_t head[ENGINEX_IMG_HEADER] = {0};
    snprintf((char *)head, sizeof head, ENGINEX_IMG_MAGIC "%s\nSCLIVE4\n5.3.0.4\n", hex);
    snprintf(img, sizeof img, "%s/apply.img", dir);
    FILE *f = fopen(img, "wb"); assert(f && fwrite(head, 1, sizeof head, f) == sizeof head && !fclose(f));
    snprintf(cmd, sizeof cmd, "cat %s/bundle.tar.gz >> %s", dir, img);
    assert(system(cmd) == 0);
    assert(az01_apply_image(img, 0) == want);
}
static void test_apply_runs_the_leave_step(void) {
    char dir[] = "/tmp/gen2-leave-XXXXXX", cmd[PATH_MAX];
    const char *inst = "#!/bin/sh\nprintf '5.3.0.4\\n' > \"$ENGINEX_CUSTOM/applied-version.txt\"\n";
    assert(mkdtemp(dir));
    board(dir, "inmusic,jp21");
    setenv("ENGINEX_TEST_DATA_ROOT", dir, 1);
    // A release before 5.3.0.5 has no leave step: the bundle's own install.sh, in place.
    apply_with(dir, inst, 0);
    assert(exists(dir, "enginex/applied-version.txt"));
    snprintf(cmd, sizeof cmd, "rm -f %s/enginex/applied-version.txt", dir);
    assert(system(cmd) == 0);
    // The step gets the unpacked bundle and puts it in (an older one through its stage, #234
    // review); the hook does not run install.sh itself, and the caller reboots.
    put(dir, "run/enginex/leave.sh",
        "[ \"$1\" = --install ] && [ -f \"$2/install.sh\" ] && printf '%s\\n' \"$3\" > \"$(dirname \"$0\")/left\"\n");
    apply_with(dir, inst, 0);
    assert(!exists(dir, "enginex/applied-version.txt") && exists(dir, "run/enginex/left"));
    FILE *f = fopen(strcat(strcpy(cmd, dir), "/run/enginex/left"), "r");
    char args[64] = {0};
    assert(f && fgets(args, sizeof args, f) && !fclose(f) && !strcmp(args, "--no-reboot\n"));
    // A step that fails (a bad bundle, a deck that cannot swap atomically) fails the apply.
    put(dir, "run/enginex/leave.sh", "exit 1\n");
    apply_with(dir, inst, -1);
    unsetenv("ENGINEX_TEST_DATA_ROOT");
    snprintf(cmd, sizeof cmd, "rm -rf %s", dir);
    assert(system(cmd) == 0);
    puts("#234: a USB image install goes through this release's leave step (--install, --no-reboot), in place without one, and fails with it: PASS");
}

int main(void) {
    test_sha256();
    test_open_verify();
    test_apply_runs_the_leave_step();
    test_other_model_refused();
    test_erase();
    test_erase_reports_failure();
    return 0;
}
