// Checks for the second-generation *-Update.img hooks and the Factory Reset keep-EngineX erase (#119).
// Native on the build host (needs sha256sum):
//   gcc -O2 -Wall -o test_gen2_img test_gen2_img.c -ldl -lpthread && ./test_gen2_img
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>

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
    puts("sha256: PASS");
}

static void test_open_verify(void) {
    char dir[] = "/tmp/gen2-img-XXXXXX", img[PATH_MAX];
    assert(mkdtemp(dir));
    snprintf(img, sizeof img, "%s/ok.img", dir);
    make_img(dir, "ok.img", "payload-bytes-0123456789", NULL, 0);
    void *h = NULL;
    assert(az01_open_image(img, &h) == 0 && h == enginex_dummy_img);
    assert(az01_image_set_onboard_key_lookup(h) == 0);
    assert(az01_verify_image(h, 0) == 0);
    assert(az01_close_image(h) == 0);
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
    char cmd[PATH_MAX + 16];
    snprintf(cmd, sizeof cmd, "rm -rf %s", dir);
    assert(system(cmd) == 0);
    puts("open/verify/close/apply checks: PASS");
}

static void test_erase(void) {
    char root[] = "/tmp/gen2-erase-XXXXXX", cmd[PATH_MAX + 16];
    assert(mkdtemp(root));
    static const char *const wiped[] = {
        "AppDataUser/x.db", "Engine Library/m.db", "StreamingAccounts.json", "Wi-Fi Drop/a.wav", "az01-update.img",
        "enginex/state/tokens.json", "enginex/mods/m1/mod.json", "enginex/ui/settings.conf", "enginex/ui/views.json",
        "enginex/ui/mods-overlay/a.qml", "enginex/ui/mods-quarantined/b", "enginex/ui/safe-mode", "enginex/ui/boot-count",
        "enginex/ui/boot-token", "enginex/ui/last-crash"};
    static const char *const kept[] = {
        "enginex/bundle/install.sh", "enginex/bundle/bin/sc5000-suite", "enginex/ui/packs/a/Main.qml", "enginex/ui/icons/x.svg",
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
    setenv("SC5000_TEST_PASSTHROUGH_ERASE", "1", 1);  // passthrough: no stock function here -> -1, nothing wiped
    assert(az01_erase_data_partition() == -1 && exists(root, "AppDataUser/x.db"));
    unsetenv("SC5000_TEST_PASSTHROUGH_ERASE");
    if (access("/usr/share/sc5000-custom/sc5000-suite.xz", F_OK) == 0) { puts("erase: SKIP (1st-gen rootfs host)"); return; }
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

int main(void) {
    test_sha256();
    test_open_verify();
    test_erase();
    return 0;
}
