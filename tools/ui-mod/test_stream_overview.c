// Unit tests for OfflineAnalyzer stream overview peak swap and Engine next-load fallback:
//   cc -O2 -Wall -Wextra -Itools/ui-mod tools/ui-mod/test_stream_overview.c -lsqlite3 -ldl -lpthread -lm -o out/test_stream_overview
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>
#include <sqlite3.h>

static int ov_exec(void *db, const char *sql, int (*row)(void *, int, char **, char **), void *arg, char **err) {
    return sqlite3_exec(db, sql, row, arg, err);
}
static const char *ov_main(void *db, const char *name) { return sqlite3_db_filename(db, name); }

static void put_be64(uchar *p, uint64_t v) {
    for (int i = 0; i < 8; i++) p[i] = (uchar)(v >> (56 - 8 * i));
}

static void put_le32(uchar *p, uint32_t v) {
    p[0] = (uchar)v;
    p[1] = (uchar)(v >> 8);
    p[2] = (uchar)(v >> 16);
    p[3] = (uchar)(v >> 24);
}

static void build_raw_overview(uchar raw[SC_STREAM_OV_RAW_LEN], uchar low, uchar mid, uchar high) {
    memset(raw, 0, SC_STREAM_OV_RAW_LEN);
    put_be64(raw, SC_STREAM_OV_BINS);
    put_be64(raw + 8, SC_STREAM_OV_BINS);
    double step = 4410.0;
    uint64_t step_bits = 0;
    memcpy(&step_bits, &step, 8);
    put_be64(raw + 16, step_bits);
    for (size_t i = 0; i < SC_STREAM_OV_BINS; i++) {
        raw[24 + i * 3 + 0] = low;
        raw[24 + i * 3 + 1] = mid;
        raw[24 + i * 3 + 2] = high;
    }
    raw[SC_STREAM_OV_RAW_LEN - 3] = low;
    raw[SC_STREAM_OV_RAW_LEN - 2] = mid;
    raw[SC_STREAM_OV_RAW_LEN - 1] = high;
}

static size_t build_sidecar(uchar *out, size_t cap, const uchar raw[SC_STREAM_OV_RAW_LEN], const uchar *z, size_t z_len) {
    size_t total = SC_STREAM_OV_HDR_LEN + SC_STREAM_OV_RAW_LEN + z_len;
    assert(total <= cap);
    memcpy(out, SC_STREAM_OV_MAGIC, 5);
    put_le32(out + 5, SC_STREAM_OV_RAW_LEN);
    put_le32(out + 9, (uint32_t)z_len);
    memcpy(out + SC_STREAM_OV_HDR_LEN, raw, SC_STREAM_OV_RAW_LEN);
    memcpy(out + SC_STREAM_OV_HDR_LEN + SC_STREAM_OV_RAW_LEN, z, z_len);
    uint32_t crc = sc_ov_crc32(out + SC_STREAM_OV_HDR_LEN, SC_STREAM_OV_RAW_LEN + z_len);
    put_le32(out + 13, crc);
    return total;
}

static int emit_calls;
static int emit_sig;
static size_t emit_len;
static uchar emit_buf[SC_STREAM_OV_RAW_LEN + 64];

static void fake_activate(void *sender, const void *meta, int sig, void **args) {
    (void)sender;
    (void)meta;
    emit_calls++;
    emit_sig = sig;
    if (args && args[1] && (sig == OA_OVERVIEW || sig == OA_OVERVIEW_RGB)) {
        const struct oa_bytes *b = args[1];
        emit_len = (size_t)b->size;
        if (emit_len <= sizeof emit_buf) memcpy(emit_buf, b->ptr, emit_len);
    }
}

static void feed_chunked_overview(void *runner, const uchar stock_raw[SC_STREAM_OV_RAW_LEN], size_t chunks) {
    assert(SC_STREAM_OV_BINS % chunks == 0);
    size_t step_bins = SC_STREAM_OV_BINS / chunks;
    uchar chunk[SC_STREAM_OV_RAW_LEN];
    for (size_t c = 0; c < chunks; c++) {
        size_t len = 24u + step_bins * 3u + 3u;
        put_be64(chunk, step_bins);
        put_be64(chunk + 8, SC_STREAM_OV_BINS);
        memcpy(chunk + 16, stock_raw + 16, 8);
        memcpy(chunk + 24, stock_raw + 24 + c * step_bins * 3u, step_bins * 3u);
        memcpy(chunk + 24 + step_bins * 3u, stock_raw + SC_STREAM_OV_RAW_LEN - 3u, 3);
        struct oa_bytes qb = {NULL, (char *)chunk, (ssize_t)len};
        void *cargs[2] = {NULL, &qb};
        oa_activate_stream_ov(runner, oa_meta, OA_OVERVIEW, cargs);
    }
    double bpm = 155.0;
    void *dargs[2] = {NULL, &bpm};
    oa_activate_stream_ov(runner, oa_meta, OA_DONE, dargs);
}

static int fake_refs;
static uchar fake_alloc[SC_STREAM_OV_RAW_LEN + 64];
static void fake_make_bytes(struct oa_bytes *out, const char *data, ssize_t len) {
    assert(len >= 0 && (size_t)len <= sizeof fake_alloc);
    fake_refs = 2; // prevent free(&fake_refs) inside inline ~QByteArray
    memcpy(fake_alloc, data, (size_t)len);
    out->d = &fake_refs;
    out->ptr = (char *)fake_alloc;
    out->size = len;
}

static void touch_file(const char *path, const void *data, size_t len) {
    FILE *f = fopen(path, "wb");
    assert(f && fwrite(data, 1, len, f) == len && !fclose(f));
}

int main(void) {
    char tmp[] = "/tmp/stream-ov-test-XXXXXX";
    assert(mkdtemp(tmp));
    char ovdir[128], dldir[128], reldir[32];
    assert(snprintf(ovdir, sizeof ovdir, "%s/ov", tmp) < (int)sizeof ovdir);
    assert(snprintf(dldir, sizeof dldir, "%s/dl/", tmp) < (int)sizeof dldir);
    snprintf(reldir, sizeof reldir, "rel/");
    assert(!mkdir(ovdir, 0755));
    assert(!mkdir(dldir, 0755));

    sc_stream_ov_dir = ovdir;
    sc_stream_ov_downloads = dldir;
    sc_stream_ov_downloads_rel = reldir;
    sc_stream_ov_drives = "/media/";

    // Download path matching 40-hex key
    const char *key = "0123456789abcdef0123456789abcdef01234567";
    char audio[256];
    snprintf(audio, sizeof audio, "%s%s.flac", dldir, key);
    uchar fake_audio[128] = "fLaC-test-payload";
    touch_file(audio, fake_audio, sizeof fake_audio);

    assert(sc_stream_ov_download_key(audio, dldir) != NULL);
    char am_mp4[256];
    snprintf(am_mp4, sizeof am_mp4, "%s%s.mp4", dldir, key);
    assert(sc_stream_ov_download_key(am_mp4, dldir) == NULL);
    assert(sc_stream_ov_download_key("/media/usb/song.flac", dldir) == NULL);

    char fp[65];
    assert(sc_stream_ov_fingerprint(audio, fp));

    uchar stock_raw[SC_STREAM_OV_RAW_LEN], peak_raw[SC_STREAM_OV_RAW_LEN];
    build_raw_overview(stock_raw, 10, 12, 8);
    build_raw_overview(peak_raw, 90, 110, 45);
    static const uchar z_payload[] = {0x00, 0x00, 0x0c, 0x1b, 0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01};
    uchar sc[8192];
    size_t sc_len = build_sidecar(sc, sizeof sc, peak_raw, z_payload, sizeof z_payload);

    // Sidecar validation cases
    const unsigned char *p_raw = NULL, *p_z = NULL;
    size_t p_zlen = 0;
    assert(sc_stream_ov_parse(sc, sc_len, &p_raw, &p_z, &p_zlen));
    sc[25] ^= 1;
    assert(!sc_stream_ov_parse(sc, sc_len, &p_raw, &p_z, &p_zlen));
    sc[25] ^= 1;
    assert(!sc_stream_ov_parse(sc, sc_len - 1, &p_raw, &p_z, &p_zlen));

    // Setup OfflineAnalyzer hook simulation
    int dummy_meta = 0;
    uchar fake_runner[64] = {0};
    oa_meta = &dummy_meta;
    oa_activate = fake_activate;
    oa_make_bytes = fake_make_bytes;
    struct oa_bytes stock_qb = {NULL, (char *)stock_raw, SC_STREAM_OV_RAW_LEN};
    void *args[2] = {NULL, &stock_qb};

    // 1. Missing sidecar -> chunked stock assembled and emitted untouched at OA_DONE
    oa_stream_ov_ctx.active = 1;
    oa_stream_ov_ctx.emitted = 0;
    sc_stream_ov_accum_reset(&oa_stream_ov_ctx.accum);
    oa_stream_ov_ctx.obj = fake_runner;
    snprintf(oa_stream_ov_ctx.path, sizeof oa_stream_ov_ctx.path, "%s", audio);
    emit_calls = 0;
    feed_chunked_overview(fake_runner, stock_raw, 4);
    assert(emit_calls == 2 && emit_sig == OA_DONE && emit_len == SC_STREAM_OV_RAW_LEN &&
           !memcmp(emit_buf, stock_raw, SC_STREAM_OV_RAW_LEN));

    // 2. Broken sidecar -> fail open with assembled stock
    char sc_path[512], ready_path[256];
    snprintf(sc_path, sizeof sc_path, "%s/%s.ov", ovdir, fp);
    snprintf(ready_path, sizeof ready_path, "%s/.ready", ovdir);
    sc[30] ^= 0xff;
    touch_file(sc_path, sc, sc_len);
    sc[30] ^= 0xff;
    oa_stream_ov_ctx.emitted = 0;
    sc_stream_ov_accum_reset(&oa_stream_ov_ctx.accum);
    emit_calls = 0;
    feed_chunked_overview(fake_runner, stock_raw, 4);
    assert(emit_calls == 2 && emit_sig == OA_DONE && emit_len == SC_STREAM_OV_RAW_LEN &&
           !memcmp(emit_buf, stock_raw, SC_STREAM_OV_RAW_LEN));

    // 3. Valid sidecar -> swaps OA_OVERVIEW at OA_DONE, keeps OA_OVERVIEW_RGB and unexpected formats untouched
    touch_file(sc_path, sc, sc_len);
    touch_file(ready_path, "", 0);
    oa_stream_ov_ctx.emitted = 0;
    sc_stream_ov_accum_reset(&oa_stream_ov_ctx.accum);
    emit_calls = 0;
    feed_chunked_overview(fake_runner, stock_raw, 4);
    assert(emit_calls == 2 && emit_sig == OA_DONE && emit_len == SC_STREAM_OV_RAW_LEN &&
           !memcmp(emit_buf, peak_raw, SC_STREAM_OV_RAW_LEN));

    emit_calls = 0;
    oa_activate_stream_ov(fake_runner, oa_meta, OA_OVERVIEW_RGB, args);
    assert(emit_calls == 1 && !memcmp(emit_buf, stock_raw, SC_STREAM_OV_RAW_LEN));

    uchar odd_raw[SC_STREAM_OV_RAW_LEN];
    memcpy(odd_raw, stock_raw, sizeof odd_raw);
    put_be64(odd_raw + 8, 512);
    struct oa_bytes odd_qb = {NULL, (char *)odd_raw, SC_STREAM_OV_RAW_LEN};
    void *odd_args[2] = {NULL, &odd_qb};
    oa_stream_ov_ctx.emitted = 0;
    sc_stream_ov_accum_reset(&oa_stream_ov_ctx.accum);
    emit_calls = 0;
    oa_activate_stream_ov(fake_runner, oa_meta, OA_OVERVIEW, odd_args);
    assert(emit_calls == 1 && !memcmp(emit_buf, odd_raw, SC_STREAM_OV_RAW_LEN));
    oa_stream_ov_ctx.active = 0;

    // 4. Engine next-load SQLite fallback: updates ONLY overviewWaveFormData on analyzed streaming row
    char db_path[128];
    snprintf(db_path, sizeof db_path, "%s/stm.db", tmp);
    sqlite3 *db = NULL;
    assert(sqlite3_open(db_path, &db) == SQLITE_OK);
    char rel_dl[128];
    assert(snprintf(rel_dl, sizeof rel_dl, "%s%s.flac", reldir, key) < (int)sizeof rel_dl);
    assert(sqlite3_exec(db,
                        "CREATE TABLE Track (id INTEGER PRIMARY KEY, uri TEXT, label TEXT, path TEXT, isAnalyzed INTEGER);"
                        "CREATE TABLE PerformanceData (trackId INTEGER PRIMARY KEY, trackData BLOB, overviewWaveFormData BLOB, beatData BLOB, quickCues BLOB, loops BLOB);"
                        "INSERT INTO Track VALUES (1, 'streaming://TIDAL/Track/99', '', '', 1);"
                        "INSERT INTO Track VALUES (2, 'streaming://TIDAL/Track/100', 'rekordbox [128 BPM]', '', 1);"
                        "INSERT INTO Track VALUES (3, 'streaming://TIDAL/Track/101', '', '', 0);",
                        NULL, NULL, NULL) == SQLITE_OK);
    char upd_paths[512];
    snprintf(upd_paths, sizeof upd_paths,
             "UPDATE Track SET path = '%s' WHERE id IN (1,2,3);"
             "INSERT INTO PerformanceData VALUES (1, X'deadbeef', X'0102', X'cafe', X'babe', X'f00d');"
             "INSERT INTO PerformanceData VALUES (2, X'deadbeef', X'0102', X'cafe', X'babe', X'f00d');"
             "INSERT INTO PerformanceData VALUES (3, NULL, NULL, NULL, NULL, NULL);",
             rel_dl);
    assert(sqlite3_exec(db, upd_paths, NULL, NULL, NULL) == SQLITE_OK);

    struct sc_stream_ov_sqlite q = {ov_exec, ov_main};
    const char *r1 = sc_stream_ov_fill_track(&q, db, 1);
    assert(r1 && !strcmp(r1, "updated"));
    assert(!sc_stream_ov_fill_track(&q, db, 1)); // idempotent second call returns NULL
    assert(!sc_stream_ov_fill_track(&q, db, 2)); // rekordbox label ignored
    assert(!sc_stream_ov_fill_track(&q, db, 3)); // unanalyzed trackData NULL ignored

    sqlite3_stmt *st = NULL;
    assert(sqlite3_prepare_v2(db, "SELECT hex(trackData)||'/'||hex(overviewWaveFormData)||'/'||hex(beatData)||'/'||hex(quickCues)||'/'||hex(loops) FROM PerformanceData WHERE trackId=1", -1, &st, NULL) == SQLITE_OK);
    assert(sqlite3_step(st) == SQLITE_ROW);
    const char *got = (const char *)sqlite3_column_text(st, 0);
    assert(got && !strcmp(got, "DEADBEEF/00000C1B789C030000000001/CAFE/BABE/F00D"));
    sqlite3_finalize(st);
    sqlite3_close(db);

    puts("test_stream_overview PASS");
    return 0;
}
