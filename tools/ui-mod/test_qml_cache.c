// Build natively and for armhf with -Itools/ui-mod -ldl -lpthread -lm.
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#ifndef ENGINE_UI_HOOK_SOURCE
#define ENGINE_UI_HOOK_SOURCE "engine_ui_hook.c"
#endif
#include ENGINE_UI_HOOK_SOURCE
#include <assert.h>

static int stock_fallback;
static int cache_access(const char *path, int mode) {
    assert(!strcmp(path, UI_ROOT "/safe-mode") && mode == F_OK);
    return stock_fallback ? 0 : -1;
}

int main(void) {
    const char *key = "QML_DISK_CACHE_PATH";
    const char *stock = "/data/AppDataUser/AIR Music Technology/Engine/.cache/qml";
    void *saved_access = access_cache;
    access_cache = (void *)cache_access;
    hook_test_engine = 0;
    assert(!setenv(key, stock, 1) && !strcmp(getenv(key), stock));
    errno = 0;
    assert(setenv(NULL, stock, 1) == -1 && errno == EINVAL);
    errno = 0;
    assert(setenv("invalid=name", stock, 1) == -1 && errno == EINVAL);
    hook_test_engine = 1;
    assert(!setenv(key, stock, 1) && !strcmp(getenv(key), UI_ROOT "/qml-cache"));
    assert(!setenv(key, "another path", 0) && !strcmp(getenv(key), UI_ROOT "/qml-cache"));
    assert(!setenv("QML_FORCE_DISK_CACHE", "1", 1) && !strcmp(getenv("QML_FORCE_DISK_CACHE"), "1"));
    stock_fallback = 1;
    assert(!setenv(key, stock, 1) && !strcmp(getenv(key), stock));
    stock_fallback = 0;
    hook_test_engine = 0;
    assert(!setenv(key, stock, 1) && !strcmp(getenv(key), stock));
    access_cache = saved_access;
    puts("QML cache: EngineX isolated; stock fallback, other processes and overwrite contract preserved: PASS");
    return 0;
}
