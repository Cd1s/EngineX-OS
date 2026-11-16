// The overview band maxima engine_ui_hook.c fills where Engine decompresses overviewWaveFormData.
// Native (AArch64 = AZ04's table) and armhf (5.3.0 / 5.0.4 tables), from the repository root:
//   cc -O2 -Wall -Wextra -Itools/ui-mod tools/ui-mod/test_overview_maxima.c -ldl -lpthread -lm -o out/test_overview_maxima
//   arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Itools/ui-mod tools/ui-mod/test_overview_maxima.c -ldl -lpthread -lm -o out/test_overview_maxima32
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>
#include <math.h>

static uchar result[256];
static int refs; // the reference count of the result's QArrayData

// Stands in for Qt's qUncompress: hands back the bytes it is given as a fresh QByteArray.
static struct oa_bytes fake_uncompress(const uchar *data, ssize_t size) {
    assert(size >= 0 && (size_t)size <= sizeof result);
    memcpy(result, data, (size_t)size);
    struct oa_bytes out = {&refs, (char *)result, size};
    return out;
}

// >Q length, >Q bins, >d samples per bin, bins x (low, mid, high), the 3 maxima.
static size_t overview(uchar *out, uint64_t length, uint64_t bins, double step, const uchar *data, size_t n,
                       const uchar trailer[3]) {
    uint64_t step_bits;
    memcpy(&step_bits, &step, 8);
    for (int i = 0; i < 8; i++) {
        out[i] = (uchar)(length >> (56 - 8 * i));
        out[8 + i] = (uchar)(bins >> (56 - 8 * i));
        out[16 + i] = (uchar)(step_bits >> (56 - 8 * i));
    }
    memcpy(out + 24, data, n);
    memcpy(out + 24 + n, trailer, 3);
    return 27 + n;
}

static const uchar zero[3], bands[15] = {0x10, 0x05, 0x01, 0x71, 0x20, 0x08, 0x33, 0x42, 0x02,
                                         0x00, 0x00, 0x1f, 0x44, 0x41, 0x1e};

// Decompresses blob through the hook; returns whether the result differs from blob.
static int changed(const uchar *blob, size_t len) {
    refs = 1;
    struct oa_bytes out = q_uncompress(blob, (ssize_t)len);
    assert(out.ptr == (char *)result && out.size == (ssize_t)len && out.d == &refs);
    return memcmp(result, blob, len) != 0;
}

static const struct engine_build *find_build(int old) {
    for (size_t i = 0; i < sizeof engine_builds / sizeof *engine_builds; i++)
        if ((strcmp(engine_builds[i].name, "5.0.4") == 0) == old) return &engine_builds[i];
    return NULL;
}

int main(void) {
    q_uncompress_cache = (void *)fake_uncompress;
    uchar blob[128];
    size_t len = overview(blob, 5, 5, 512.0, bands, sizeof bands, zero);

    build = NULL; // not Engine, or a build the hook does not know
    assert(!changed(blob, len));
    build = find_build(0);
    assert(build);
    // A zero trailer becomes each band's maximum; nothing else changes.
    assert(changed(blob, len));
    assert(result[len - 3] == 0x71 && result[len - 2] == 0x42 && result[len - 1] == 0x1f);
    assert(!memcmp(result, blob, len - 3));

    uchar want[128];
    size_t n;
    // A trailer Engine wrote is kept, even one that is not the maximum.
    n = overview(want, 5, 5, 512.0, bands, sizeof bands, (const uchar[3]){1, 2, 3});
    assert(!changed(want, n));
    // A silent track keeps its zeros.
    n = overview(want, 5, 5, 512.0, (const uchar[15]){0}, 15, zero);
    assert(!changed(want, n));
    // Anything that is not an overview: bins that do not match the length, a length short of
    // the bins, a step that is not a positive number, a blob too short or not of whole bins.
    n = overview(want, 6, 6, 512.0, bands, sizeof bands, zero);
    assert(!changed(want, n));
    n = overview(want, 5, 4, 512.0, bands, sizeof bands, zero);
    assert(!changed(want, n));
    n = overview(want, 4, 5, 512.0, bands, sizeof bands, zero);
    assert(!changed(want, n));
    n = overview(want, 5, 5, 0.0, bands, sizeof bands, zero);
    assert(!changed(want, n));
    n = overview(want, 5, 5, INFINITY, bands, sizeof bands, zero);
    assert(!changed(want, n));
    n = overview(want, 5, 5, NAN, bands, sizeof bands, zero);
    assert(!changed(want, n));
    n = overview(want, 5, 5, 512.0, bands, sizeof bands, zero);
    assert(!changed(want, n - 1) && !changed(want, 26) && !changed(want, 0));
    n = overview(want, 0, 0, 512.0, bands, 0, zero);
    assert(!changed(want, n));
    // A result shared with another QByteArray is never written.
    refs = 2;
    struct oa_bytes shared = q_uncompress(blob, (ssize_t)len);
    assert(shared.size == (ssize_t)len && !memcmp(result, blob, len));
    // Engine 5.0.4 is left as it is (armhf table only).
    if ((build = find_build(1))) assert(!changed(blob, len));
    // Without Qt's qUncompress it returns an empty QByteArray.
    build = find_build(0);
    q_uncompress_cache = NULL;
    struct oa_bytes none = q_uncompress(blob, (ssize_t)len);
    assert(!none.d && !none.ptr && !none.size);
    puts("test_overview_maxima PASS");
    return 0;
}
