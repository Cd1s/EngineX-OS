/* Unit tests for tools/ui-mod/rekordbox_browse.h.
 * Build on Linux (x86_64, arm64, or armhf):
 *   gcc -std=gnu11 -O2 -Wall -Wextra -Werror tools/ui-mod/test_rekordbox_browse.c -lpthread -o test_rekordbox_browse
 */
#define _GNU_SOURCE
#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/stat.h>
#include <unistd.h>

#include "rekordbox_browse.h"

static void put_be32(uint8_t *p, uint32_t v) {
    p[0] = (uint8_t)(v >> 24);
    p[1] = (uint8_t)(v >> 16);
    p[2] = (uint8_t)(v >> 8);
    p[3] = (uint8_t)v;
}

static uint64_t get_be64(const uint8_t *p) {
    return ((uint64_t)sc_rbb_be32(p) << 32) | (uint64_t)sc_rbb_be32(p + 4);
}

static void write_file_all(const char *path, const void *data, size_t len) {
    int fd = open(path, O_WRONLY | O_CREAT | O_TRUNC, 0644);
    assert(fd >= 0);
    const uint8_t *p = (const uint8_t *)data;
    size_t off = 0;
    while (off < len) {
        ssize_t n = write(fd, p + off, len - off);
        assert(n > 0);
        off += (size_t)n;
    }
    assert(close(fd) == 0);
}

static void path_fmt(char *dst, size_t cap, const char *a, const char *b) {
    size_t na = strlen(a), nb = strlen(b);
    assert(na + nb + 1 <= cap);
    memcpy(dst, a, na);
    memcpy(dst + na, b, nb + 1);
}

static void mkdir_p(const char *path) {
    char tmp[PATH_MAX];
    assert(strlen(path) < sizeof tmp);
    strcpy(tmp, path);
    for (char *p = tmp + 1; *p; p++) {
        if (*p == '/') {
            *p = '\0';
            mkdir(tmp, 0755);
            *p = '/';
        }
    }
    mkdir(tmp, 0755);
}

/* Build a minimal PMAI file with a single waveform section (plus optional trailing PWV7 junk). */
static void write_anlz_pwv6(const char *path, uint32_t count, const uint8_t *rgb3, int add_pwv7) {
    uint32_t pmai_h = 28;
    uint32_t s_h = 20;
    uint32_t s_len = s_h + count * 3;
    uint32_t pwv7_len = add_pwv7 ? (24 + 40000) : 0;
    uint32_t total = pmai_h + s_len + pwv7_len;
    uint8_t *buf = (uint8_t *)calloc(1, total);
    assert(buf != NULL);
    memcpy(buf, "PMAI", 4);
    put_be32(buf + 4, pmai_h);
    put_be32(buf + 8, total);
    uint8_t *s = buf + pmai_h;
    memcpy(s, "PWV6", 4);
    put_be32(s + 4, s_h);
    put_be32(s + 8, s_len);
    put_be32(s + 12, 3);
    put_be32(s + 16, count);
    memcpy(s + s_h, rgb3, count * 3);
    if (add_pwv7) {
        uint8_t *s7 = s + s_len;
        memcpy(s7, "PWV7", 4);
        put_be32(s7 + 4, 24);
        put_be32(s7 + 8, pwv7_len);
        put_be32(s7 + 12, 3);
        put_be32(s7 + 16, 13000);
        memset(s7 + 24, 0xEE, pwv7_len - 24);
    }
    write_file_all(path, buf, total);
    free(buf);
}

static void write_anlz_pwv4(const char *path, uint32_t count, const uint8_t *entries6) {
    uint32_t pmai_h = 28;
    uint32_t s_h = 24;
    uint32_t s_len = s_h + count * 6;
    uint32_t total = pmai_h + s_len;
    uint8_t *buf = (uint8_t *)calloc(1, total);
    assert(buf != NULL);
    memcpy(buf, "PMAI", 4);
    put_be32(buf + 4, pmai_h);
    put_be32(buf + 8, total);
    uint8_t *s = buf + pmai_h;
    memcpy(s, "PWV4", 4);
    put_be32(s + 4, s_h);
    put_be32(s + 8, s_len);
    put_be32(s + 12, 6);
    put_be32(s + 16, count);
    memcpy(s + s_h, entries6, count * 6);
    write_file_all(path, buf, total);
    free(buf);
}

static void write_anlz_pwav(const char *path, uint32_t count, const uint8_t *entries1) {
    uint32_t pmai_h = 28;
    uint32_t s_h = 20;
    uint32_t s_len = s_h + count;
    uint32_t total = pmai_h + s_len;
    uint8_t *buf = (uint8_t *)calloc(1, total);
    assert(buf != NULL);
    memcpy(buf, "PMAI", 4);
    put_be32(buf + 4, pmai_h);
    put_be32(buf + 8, total);
    uint8_t *s = buf + pmai_h;
    memcpy(s, "PWAV", 4);
    put_be32(s + 4, s_h);
    put_be32(s + 8, s_len);
    put_be32(s + 12, count);
    memcpy(s + s_h, entries1, count);
    write_file_all(path, buf, total);
    free(buf);
}

/* Verify a qCompress'd SC_RBB_BLOB_LEN RFC-1950 stored-block blob (and Adler-32). */
static void verify_qcompress_blob(const uint8_t *blob, size_t len, uint8_t raw_out[SC_RBB_RAW_LEN]) {
    assert(len == SC_RBB_BLOB_LEN);
    uint32_t uncomp_len = sc_rbb_be32(blob);
    assert(uncomp_len == SC_RBB_RAW_LEN);
    /* RFC 1950 zlib header: CMF=0x78, FLG=0x01 -> ((0x78<<8)|0x01) % 31 == 0 */
    assert(blob[4] == 0x78 && blob[5] == 0x01);
    assert((((uint32_t)blob[4] << 8) | blob[5]) % 31u == 0);
    /* RFC 1951 stored block: BFINAL=1, BTYPE=00, LE16 len, LE16 ~len */
    assert(blob[6] == 0x01);
    uint16_t blen = (uint16_t)blob[7] | ((uint16_t)blob[8] << 8);
    uint16_t nlen = (uint16_t)blob[9] | ((uint16_t)blob[10] << 8);
    assert(blen == SC_RBB_RAW_LEN);
    assert((uint16_t)(blen ^ nlen) == 0xffffu);
    memcpy(raw_out, blob + 11, SC_RBB_RAW_LEN);
    /* Verify Adler-32 trailer independently */
    uint32_t s1 = 1, s2 = 0;
    for (size_t i = 0; i < SC_RBB_RAW_LEN; i++) {
        s1 = (s1 + raw_out[i]) % 65521u;
        s2 = (s2 + s1) % 65521u;
    }
    assert(sc_rbb_be32(blob + 11 + SC_RBB_RAW_LEN) == ((s2 << 16) | s1));
    assert(get_be64(raw_out + 0) == 1024);
    assert(get_be64(raw_out + 8) == 1024);
}

/* Fake SQLite statement for testing SQLite browse synthesis & thread isolation. */
struct fake_stmt {
    const char *sql;
    const char *db_file;
    int ncols;
    const char *names[4];
    int64_t tid;
    int analyzed;
    const uint8_t *overview;
    int overview_len;
};

static const void *fake_col_blob(void *s, int col) {
    struct fake_stmt *st = (struct fake_stmt *)s;
    return (col == 2) ? st->overview : NULL;
}
static int fake_col_bytes(void *s, int col) {
    struct fake_stmt *st = (struct fake_stmt *)s;
    return (col == 2) ? st->overview_len : 0;
}
static int fake_col_int(void *s, int col) {
    struct fake_stmt *st = (struct fake_stmt *)s;
    if (col == 0) return (int)st->tid;
    if (col == 1) return st->analyzed;
    return 0;
}
static long long fake_col_int64(void *s, int col) {
    struct fake_stmt *st = (struct fake_stmt *)s;
    if (col == 0) return (long long)st->tid;
    if (col == 1) return (long long)st->analyzed;
    return 0;
}
static int fake_col_type(void *s, int col) {
    struct fake_stmt *st = (struct fake_stmt *)s;
    if (col == 0) return 1; /* SQLITE_INTEGER */
    if (col == 1) return 1; /* SQLITE_INTEGER */
    if (col == 2) return st->overview ? 4 : 5; /* SQLITE_BLOB : SQLITE_NULL */
    return 5;
}
static int fake_col_count(void *s) { return ((struct fake_stmt *)s)->ncols; }
static const char *fake_col_name(void *s, int col) { return ((struct fake_stmt *)s)->names[col]; }
static void *fake_db_handle(void *s) { return s; }
static const char *fake_sql(void *s) { return ((struct fake_stmt *)s)->sql; }
static const char *fake_db_filename(void *db, const char *name) {
    (void)name;
    return ((struct fake_stmt *)db)->db_file;
}

struct thread_check_arg {
    const char *name;
    struct fake_stmt *st;
    int expect_synth;
};

static void *run_on_named_thread(void *arg) {
    struct thread_check_arg *a = (struct thread_check_arg *)arg;
    assert(prctl(PR_SET_NAME, a->name, 0, 0, 0) == 0);
    int ana = sc_rbb_column_int_hook(a->st, 1);
    long long ana64 = sc_rbb_column_int64_hook(a->st, 1);
    int ty = sc_rbb_column_type_hook(a->st, 2);
    int nbytes = sc_rbb_column_bytes_hook(a->st, 2);
    const void *blob = sc_rbb_column_blob_hook(a->st, 2);
    if (a->expect_synth) {
        assert(ana == 1);
        assert(ana64 == 1);
        assert(ty == 4);
        assert(nbytes == SC_RBB_BLOB_LEN);
        assert(blob != NULL);
        uint8_t raw[SC_RBB_RAW_LEN];
        verify_qcompress_blob((const uint8_t *)blob, (size_t)nbytes, raw);
        /* Track 10 PWV6 had {10, 120, 240} */
        assert(raw[24] == 10 && raw[25] == 120 && raw[26] == 240);
        assert(raw[SC_RBB_RAW_LEN - 3] == 10 && raw[SC_RBB_RAW_LEN - 2] == 120 && raw[SC_RBB_RAW_LEN - 1] == 240);
    } else {
        assert(ana == 0);
        assert(ana64 == 0);
        assert(ty == 5);
        assert(nbytes == 0);
        assert(blob == NULL);
    }
    return NULL;
}

int main(void) {
    char root_tpl[] = "/tmp/test_rbb_XXXXXX";
    char *root = mkdtemp(root_tpl);
    assert(root != NULL);

    char drives_prefix[PATH_MAX];
    path_fmt(drives_prefix, sizeof drives_prefix, root, "/");
    sc_rbb_drives = drives_prefix;

    char stick[PATH_MAX], dbdir[PATH_MAX], anlzdir[PATH_MAX], artdir[PATH_MAX];
    path_fmt(stick, sizeof stick, root, "/STICK");
    path_fmt(dbdir, sizeof dbdir, stick, "/Engine Library/Database2");
    path_fmt(anlzdir, sizeof anlzdir, stick, "/PIONEER/USBANLZ/P001/00000001");
    path_fmt(artdir, sizeof artdir, stick, "/PIONEER/Artwork/00001");
    mkdir_p(dbdir);
    mkdir_p(anlzdir);
    mkdir_p(artdir);

    /* Track 10: .2EX with PWV6 + huge trailing PWV7 */
    uint8_t pwv6[1200 * 3];
    for (int i = 0; i < 1200; i++) {
        pwv6[i * 3 + 0] = 10;
        pwv6[i * 3 + 1] = 120;
        pwv6[i * 3 + 2] = 240;
    }
    char p2ex[PATH_MAX];
    path_fmt(p2ex, sizeof p2ex, anlzdir, "/ANLZ0000.2EX");
    write_anlz_pwv6(p2ex, 1200, pwv6, 1);

    /* Track 20: .EXT only (PWV4 fallback) */
    uint8_t pwv4[1200 * 6];
    for (int i = 0; i < 1200; i++) {
        pwv4[i * 6 + 0] = 1; pwv4[i * 6 + 1] = 2; pwv4[i * 6 + 2] = 3;
        pwv4[i * 6 + 3] = 55; pwv4[i * 6 + 4] = 88; pwv4[i * 6 + 5] = 200;
    }
    char pext[PATH_MAX];
    path_fmt(pext, sizeof pext, anlzdir, "/ANLZ0001.EXT");
    write_anlz_pwv4(pext, 1200, pwv4);

    /* Track 30: .DAT only (PWAV monochrome fallback, tinted blue) */
    uint8_t pwav[400];
    memset(pwav, 31, sizeof pwav);
    char pdat[PATH_MAX];
    path_fmt(pdat, sizeof pdat, anlzdir, "/ANLZ0002.DAT");
    write_anlz_pwav(pdat, 400, pwav);

    /* Cover file */
    char art_file[PATH_MAX];
    path_fmt(art_file, sizeof art_file, artdir, "/a1.jpg");
    write_file_all(art_file, "\xff\xd8\xff\xe0TESTJPG", 11);

    /* Write .rb-bridge-browse (including traversal attempt that must be rejected!) */
    const char *uuid = "e165a04a-ac0a-4912-bd11-d42e6acbbcf6";
    char idx_path[PATH_MAX];
    path_fmt(idx_path, sizeof idx_path, dbdir, "/.rb-bridge-browse");
    char idx_body[2048];
    snprintf(idx_body, sizeof idx_body,
             "RBB1 %s\n"
             "T\t10\t240.000\t44100\t/PIONEER/USBANLZ/P001/00000001/ANLZ0000.DAT\n"
             "T\t20\t180.500\t48000\t/PIONEER/USBANLZ/P001/00000001/ANLZ0001.DAT\n"
             "T\t30\t120.000\t44100\t/PIONEER/USBANLZ/P001/00000001/ANLZ0002.DAT\n"
             "T\t40\t100.000\t44100\t/../../etc/passwd\n"
             "A\tJcGzLMZmZphFwIWYUrbI7LyECvA\t/PIONEER/Artwork/00001/a1.jpg\n"
             "A\tBADTRAV\t/../../etc/shadow\n",
             uuid);
    write_file_all(idx_path, idx_body, strlen(idx_body));

    /* 1. Note m.db open -> activates sc_rbb_active */
    char mdb_path[PATH_MAX];
    path_fmt(mdb_path, sizeof mdb_path, dbdir, "/m.db");
    assert(__atomic_load_n(&sc_rbb_active, __ATOMIC_RELAXED) == 0);
    sc_rbb_note_path(mdb_path);
    assert(__atomic_load_n(&sc_rbb_active, __ATOMIC_RELAXED) == 1);

    /* 2. Artwork redirect & traversal rejection */
    char art_req[PATH_MAX], art_mapped[PATH_MAX];
    path_fmt(art_req, sizeof art_req, stick, "/Engine Library/Artwork/JcGzLMZmZphFwIWYUrbI7LyECvA.jpg");
    assert(sc_rbb_map_artwork(art_req, art_mapped) == 1);
    assert(strcmp(art_mapped, art_file) == 0);

    path_fmt(art_req, sizeof art_req, stick, "/Engine Library/Artwork/BADTRAV.jpg");
    assert(sc_rbb_map_artwork(art_req, art_mapped) == 0);

    /* 3. RGB memfd_create + stat/statx/access + RFC-1950 verify for PWV6, PWV4, PWAV */
    char ovdir[PATH_MAX], rgb_req[PATH_MAX];
    path_fmt(ovdir, sizeof ovdir, dbdir, "/OverviewData/e165a04a-ac0a-4912-bd11-d42e6acbbcf6");
    path_fmt(rgb_req, sizeof rgb_req, ovdir, "/10.rgb");
    assert(sc_rbb_access_rgb(rgb_req, R_OK) == 1);
    assert(sc_rbb_access_rgb(rgb_req, W_OK) == 0);
    struct stat st;
    assert(sc_rbb_stat_rgb(rgb_req, &st) == 1);
    assert(S_ISREG(st.st_mode) && st.st_size == SC_RBB_BLOB_LEN);
#ifdef STATX_BASIC_STATS
    struct statx stx;
    assert(sc_rbb_statx_rgb(rgb_req, &stx) == 1);
    assert(S_ISREG(stx.stx_mode) && stx.stx_size == SC_RBB_BLOB_LEN);
#endif

    int fd = sc_rbb_open_rgb(rgb_req, O_RDONLY | O_CLOEXEC);
    assert(fd >= 0);
    uint8_t blob[SC_RBB_BLOB_LEN];
    assert(read(fd, blob, sizeof blob) == SC_RBB_BLOB_LEN);
    close(fd);
    uint8_t raw[SC_RBB_RAW_LEN];
    verify_qcompress_blob(blob, sizeof blob, raw);
    assert(raw[24] == 10 && raw[25] == 120 && raw[26] == 240);
    assert(raw[SC_RBB_RAW_LEN - 3] == 10 && raw[SC_RBB_RAW_LEN - 2] == 120 && raw[SC_RBB_RAW_LEN - 1] == 240);

    /* Track 20 (PWV4) */
    path_fmt(rgb_req, sizeof rgb_req, ovdir, "/20.rgb");
    fd = sc_rbb_open_rgb(rgb_req, O_RDONLY);
    assert(fd >= 0);
    assert(read(fd, blob, sizeof blob) == SC_RBB_BLOB_LEN);
    close(fd);
    verify_qcompress_blob(blob, sizeof blob, raw);
    assert(raw[24] == 55 && raw[25] == 88 && raw[26] == 200);

    /* Track 30 (PWAV monochrome tinted blue: 255/4=63, 255/2=127, 255) */
    path_fmt(rgb_req, sizeof rgb_req, ovdir, "/30.rgb");
    fd = sc_rbb_open_rgb(rgb_req, O_RDONLY);
    assert(fd >= 0);
    assert(read(fd, blob, sizeof blob) == SC_RBB_BLOB_LEN);
    close(fd);
    verify_qcompress_blob(blob, sizeof blob, raw);
    assert(raw[24] == 63 && raw[25] == 127 && raw[26] == 255);

    /* Track 40 (rejected traversal) must not open */
    path_fmt(rgb_req, sizeof rgb_req, ovdir, "/40.rgb");
    assert(sc_rbb_open_rgb(rgb_req, O_RDONLY) < 0);

    /* Write-open marks track on_disk so future reads bypass memfd */
    path_fmt(rgb_req, sizeof rgb_req, ovdir, "/30.rgb");
    assert(sc_rbb_open_rgb(rgb_req, O_WRONLY | O_CREAT) < 0);
    assert(sc_rbb_access_rgb(rgb_req, R_OK) == 0);
    assert(sc_rbb_open_rgb(rgb_req, O_RDONLY) < 0);

    /* 4. SQLite browse column synthesis & strict thread isolation */
    sc_rbb_sq.col_blob = fake_col_blob;
    sc_rbb_sq.col_bytes = fake_col_bytes;
    sc_rbb_sq.col_int = fake_col_int;
    sc_rbb_sq.col_int64 = fake_col_int64;
    sc_rbb_sq.col_type = fake_col_type;
    sc_rbb_sq.col_count = fake_col_count;
    sc_rbb_sq.col_name = fake_col_name;
    sc_rbb_sq.db_handle = fake_db_handle;
    sc_rbb_sq.sql = fake_sql;
    sc_rbb_sq.db_filename = fake_db_filename;

    struct fake_stmt browse_stmt = {
        .sql = "SELECT id, isAnalyzed, overviewWaveFormData FROM Track WHERE id = ?",
        .db_file = mdb_path,
        .ncols = 3,
        .names = {"id", "isAnalyzed", "overviewWaveFormData"},
        .tid = 10,
        .analyzed = 0,
        .overview = NULL,
        .overview_len = 0,
    };
    struct fake_stmt path_stmt = browse_stmt;
    path_stmt.sql = "SELECT id, isAnalyzed, overviewWaveFormData FROM Track t WHERE t.path = ?";

    const struct { const char *thr; struct fake_stmt *st; int expect; } cases[] = {
        {"Thread (pooled)", &browse_stmt, 1},
        {"Thread (pooled)", &path_stmt, 0},
        {"TrackLoad1",      &browse_stmt, 0},
        {"J:Searchingtrac", &browse_stmt, 0},
        {"J:Fetchingperfo", &browse_stmt, 0},
        {"J:Importingtrac", &browse_stmt, 0},
        {"SceneGraph",      &browse_stmt, 0},
        {"EMain",           &browse_stmt, 0},
    };
    for (size_t i = 0; i < sizeof cases / sizeof *cases; i++) {
        pthread_t th;
        struct thread_check_arg arg = {cases[i].thr, cases[i].st, cases[i].expect};
        assert(pthread_create(&th, NULL, run_on_named_thread, &arg) == 0);
        assert(pthread_join(th, NULL) == 0);
    }

    /* Clean up temp dir */
    char cmd[PATH_MAX + 32];
    snprintf(cmd, sizeof cmd, "rm -rf '%s'", root);
    assert(system(cmd) == 0);
    printf("test_rekordbox_browse: PASS\n");
    return 0;
}
