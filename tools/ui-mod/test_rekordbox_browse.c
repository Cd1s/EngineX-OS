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

struct tls_alias_arg {
    const char *db_file;
    char *root;
};

static void *run_tls_alias_and_gen_test(void *arg) {
    struct tls_alias_arg *a = (struct tls_alias_arg *)arg;
    assert(prctl(PR_SET_NAME, "Thread (pooled)", 0, 0, 0) == 0);

    char sql_buf[128];
    strcpy(sql_buf, "SELECT id, isAnalyzed, overviewWaveFormData FROM Track WHERE id = ?");
    struct fake_stmt st = {
        .sql = sql_buf,
        .db_file = a->db_file,
        .ncols = 3,
        .names = {"id", "isAnalyzed", "overviewWaveFormData"},
        .tid = 10,
        .analyzed = 0,
        .overview = NULL,
        .overview_len = 0,
    };
    assert(sc_rbb_column_int_hook(&st, 1) == 1);

    /* Bumping drive gen invalidates cached stmt drive_gen on the same thread & st handle */
    __atomic_add_fetch(&sc_rbb_tbl[0].gen, 1, __ATOMIC_RELEASE);
    assert(sc_rbb_column_int_hook(&st, 1) == 1);

    /* Reuse exact same stmt & sql_buf pointer with rewritten SQL text (3 columns) -> must NOT alias */
    strcpy(sql_buf, "SELECT id, isAnalyzed, overviewWaveFormData FROM Track WHERE path = ?");
    assert(sc_rbb_column_int_hook(&st, 1) == 0);

    /* Evicting the active indexed drive across SC_RBB_MAX_DRIVES unindexed paths recomputes sc_rbb_active=0 */
    assert(__atomic_load_n(&sc_rbb_active, __ATOMIC_RELAXED) == 1);
    for (int i = 0; i < SC_RBB_MAX_DRIVES; i++) {
        char m[PATH_MAX];
        snprintf(m, sizeof m, "%s/UNINDEXED_%d", a->root, i);
        mkdir_p(m);
        (void)sc_rbb_ensure_drive(m);
    }
    assert(__atomic_load_n(&sc_rbb_active, __ATOMIC_RELAXED) == 0);
    return NULL;
}

struct gui_hook_arg {
    const char *thr_name;
    const char *rgb_path;
    const char *art_path;
    const char *note_mdb;
    int failures;
};

static uint64_t g_dup_race_gen;
static void inject_concurrent_dup_slot(void) {
    pthread_mutex_lock(&sc_rbb_mu);
    sc_rbb_cache[0].state = 0;
    sc_rbb_cache[7].drive_gen = g_dup_race_gen;
    sc_rbb_cache[7].track_id = 29;
    sc_rbb_cache[7].lru_tick = ++sc_rbb_lru_seq;
    sc_rbb_cache[7].state = 1;
    sc_rbb_cache[7].neg_until = 0;
    pthread_mutex_unlock(&sc_rbb_mu);
}

static void *run_gui_browse_guard(void *arg) {
    struct gui_hook_arg *a = (struct gui_hook_arg *)arg;
#ifdef __linux__
    assert(prctl(PR_SET_NAME, a->thr_name, 0, 0, 0) == 0);
#endif
    char mapped[PATH_MAX];
    struct stat st;
    if (a->note_mdb) sc_rbb_note_path(a->note_mdb);
    int fd = sc_rbb_open_rgb(a->rgb_path, O_RDONLY | O_CLOEXEC);
    if (fd >= 0) {
        close(fd);
        a->failures++;
    }
    if (sc_rbb_access_rgb(a->rgb_path, R_OK) != 0) a->failures++;
    if (sc_rbb_stat_rgb(a->rgb_path, &st) != 0) a->failures++;
#ifdef STATX_BASIC_STATS
    struct statx stx;
    if (sc_rbb_statx_rgb(a->rgb_path, &stx) != 0) a->failures++;
#endif
    if (sc_rbb_map_artwork(a->art_path, mapped) != 0) a->failures++;
    int afd = sc_rbb_open_artwork(a->art_path, O_RDONLY | O_CLOEXEC);
    if (afd >= 0) {
        close(afd);
        a->failures++;
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

    /* Write-open or O_APPEND/O_DIRECTORY must never bypass to memfd */
    path_fmt(rgb_req, sizeof rgb_req, ovdir, "/30.rgb");
    assert(sc_rbb_open_rgb(rgb_req, O_RDONLY | O_DIRECTORY) < 0);
    assert(sc_rbb_open_rgb(rgb_req, O_RDONLY | O_APPEND) < 0);
    assert(sc_rbb_open_rgb(rgb_req, O_WRONLY | O_CREAT) < 0);
    assert(sc_rbb_access_rgb(rgb_req, R_OK) == 0);
    assert(sc_rbb_open_rgb(rgb_req, O_RDONLY) < 0);

    /* PWV5 (16-bit packed RGB) and PWV3 (monochrome tinted blue) decoding + inline WHERE boundary */
    assert(sc_rbb_sql_inline_id("SELECT nowhere, overviewWaveFormData FROM Track WHERE trackId = 10") == 10);
    {
        /* Track 25: PWV5 in .EXT */
        uint8_t pwv5[1200 * 2];
        for (int i = 0; i < 1200; i++) {
            uint16_t val = (uint16_t)((7u << 13) | (0u << 10) | (7u << 7) | (31u << 2)); /* magenta */
            pwv5[i * 2 + 0] = (uint8_t)(val >> 8);
            pwv5[i * 2 + 1] = (uint8_t)val;
        }
        char p5path[PATH_MAX];
        path_fmt(p5path, sizeof p5path, anlzdir, "/ANLZ0005.EXT");
        uint32_t total5 = 28 + 24 + sizeof pwv5;
        uint8_t *b5 = (uint8_t *)calloc(1, total5);
        assert(b5 != NULL);
        memcpy(b5, "PMAI", 4); put_be32(b5 + 4, 28); put_be32(b5 + 8, total5);
        uint8_t *s5 = b5 + 28;
        memcpy(s5, "PWV5", 4); put_be32(s5 + 4, 24); put_be32(s5 + 8, 24 + sizeof pwv5);
        put_be32(s5 + 12, 2); put_be32(s5 + 16, 1200);
        memcpy(s5 + 24, pwv5, sizeof pwv5);
        write_file_all(p5path, b5, total5);
        free(b5);

        /* Rewrite .rb-bridge-browse with Track 25 and bump mtime so sc_rbb_ensure_drive refreshes */
        char idx2[2048];
        snprintf(idx2, sizeof idx2,
                 "RBB1 %s\n"
                 "T\t10\t240.000\t44100\t/PIONEER/USBANLZ/P001/00000001/ANLZ0000.DAT\n"
                 "T\t20\t180.500\t48000\t/PIONEER/USBANLZ/P001/00000001/ANLZ0001.DAT\n"
                 "T\t25\t120.000\t44100\t/PIONEER/USBANLZ/P001/00000001/ANLZ0005.DAT\n"
                 "T\t30\t120.000\t44100\t/PIONEER/USBANLZ/P001/00000001/ANLZ0002.DAT\n"
                 "A\tJcGzLMZmZphFwIWYUrbI7LyECvA\t/PIONEER/Artwork/00001/a1.jpg\n",
                 uuid);
        write_file_all(idx_path, idx2, strlen(idx2));
        pthread_mutex_lock(&sc_rbb_mu);
        sc_rbb_tbl[0].last_check = 0; /* force refresh immediately */
        sc_rbb_tbl[0].idx_size = 0;
        pthread_mutex_unlock(&sc_rbb_mu);

        path_fmt(rgb_req, sizeof rgb_req, ovdir, "/25.rgb");
        int fd5 = sc_rbb_open_rgb(rgb_req, O_RDONLY);
        assert(fd5 >= 0);
        assert(read(fd5, blob, sizeof blob) == SC_RBB_BLOB_LEN);
        close(fd5);
        verify_qcompress_blob(blob, sizeof blob, raw);
        assert(raw[24] == 255 && raw[25] == 0 && raw[26] == 255);
    }

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

    /* 5. Reused stmt pointer aliasing + drive_gen refresh + 8-drive eviction on single TLS worker */
    {
        struct tls_alias_arg ta = {mdb_path, root};
        pthread_t th;
        assert(pthread_create(&th, NULL, run_tls_alias_and_gen_test, &ta) == 0);
        assert(pthread_join(th, NULL) == 0);
    }

    /* 6. Review proof tests (Findings 2, 4, C-NOTE 5, RBB1 tag). */
    {
        int review_failures = 0;

        /* Restore active drive index on slot 0 (step 5 evicted it across UNINDEXED_0..7). */
        sc_rbb_note_path(mdb_path);
        assert(__atomic_load_n(&sc_rbb_active, __ATOMIC_RELAXED) == 1);

        /* Proof C (Finding 2): SceneGraph, EMain, EPlanckApp, QSGRenderThread must bypass ALL browse hooks,
         * and sc_rbb_note_path on a GUI thread must not register unindexed mounts or read .rb-bridge-browse. */
        char gui_stick[PATH_MAX], gui_dbdir[PATH_MAX], gui_mdb[PATH_MAX], gui_idx[PATH_MAX];
        path_fmt(gui_stick, sizeof gui_stick, root, "/GUI_STICK");
        path_fmt(gui_dbdir, sizeof gui_dbdir, gui_stick, "/Engine Library/Database2");
        mkdir_p(gui_dbdir);
        path_fmt(gui_mdb, sizeof gui_mdb, gui_dbdir, "/m.db");
        write_file_all(gui_mdb, "", 0);
        path_fmt(gui_idx, sizeof gui_idx, gui_dbdir, "/.rb-bridge-browse");
        char gui_idx_body[256];
        snprintf(gui_idx_body, sizeof gui_idx_body,
                 "RBB1 %s tag1\nT\t1\t60.0\t44100\t/PIONEER/USBANLZ/P001/00000001/ANLZ0000.DAT\n", uuid);
        write_file_all(gui_idx, gui_idx_body, strlen(gui_idx_body));

        char valid_art[PATH_MAX];
        path_fmt(valid_art, sizeof valid_art, stick, "/Engine Library/Artwork/JcGzLMZmZphFwIWYUrbI7LyECvA.jpg");
        path_fmt(rgb_req, sizeof rgb_req, ovdir, "/10.rgb");
        const char *gui_names[] = {"SceneGraph", "EMain", "EPlanckApp", "QSGRenderThread"};
        for (size_t i = 0; i < sizeof gui_names / sizeof *gui_names; i++) {
            struct gui_hook_arg ga = {gui_names[i], rgb_req, valid_art, gui_mdb, 0};
            pthread_t gth;
            assert(pthread_create(&gth, NULL, run_gui_browse_guard, &ga) == 0);
            assert(pthread_join(gth, NULL) == 0);
            if (ga.failures != 0) {
                fprintf(stderr, "REVIEW PROOF FAIL (Finding 2): GUI thread %s bypassed browse guard (%d failure(s))\n",
                        gui_names[i], ga.failures);
                review_failures += ga.failures;
            }
        }
        pthread_mutex_lock(&sc_rbb_mu);
        for (int i = 0; i < SC_RBB_MAX_DRIVES; i++) {
            if (sc_rbb_tbl[i].used && !strcmp(sc_rbb_tbl[i].mount, gui_stick)) {
                fprintf(stderr, "REVIEW PROOF FAIL (Finding 2): GUI sc_rbb_note_path registered drive %s\n", gui_stick);
                review_failures++;
            }
        }
        pthread_mutex_unlock(&sc_rbb_mu);

        /* Proof D (Finding 4): Symlinks, intermediate symlink dirs, and FIFOs under PIONEER/ must fail safely. */
        char outside_anlz[PATH_MAX], link_anlz[PATH_MAX], sym_dir[PATH_MAX], sym_sub[PATH_MAX];
        char sym_target[PATH_MAX], fifo_anlz[PATH_MAX], outside_art[PATH_MAX], link_art[PATH_MAX];
        path_fmt(outside_anlz, sizeof outside_anlz, root, "/outside.2EX");
        write_anlz_pwv6(outside_anlz, 1200, pwv6, 0);
        path_fmt(link_anlz, sizeof link_anlz, anlzdir, "/ANLZ0026.2EX");
        assert(symlink(outside_anlz, link_anlz) == 0);

        path_fmt(sym_sub, sizeof sym_sub, root, "/sym_outside_dir");
        mkdir_p(sym_sub);
        path_fmt(sym_target, sizeof sym_target, sym_sub, "/ANLZ0027.2EX");
        write_anlz_pwv6(sym_target, 1200, pwv6, 0);
        path_fmt(sym_dir, sizeof sym_dir, anlzdir, "/SYMDIR");
        assert(symlink(sym_sub, sym_dir) == 0);

        path_fmt(fifo_anlz, sizeof fifo_anlz, anlzdir, "/ANLZ0028.2EX");
        assert(mkfifo(fifo_anlz, 0600) == 0);

        path_fmt(outside_art, sizeof outside_art, root, "/outside.jpg");
        write_file_all(outside_art, "\xff\xd8\xff\xe0OUTSIDE", 11);
        path_fmt(link_art, sizeof link_art, artdir, "/sym.jpg");
        assert(symlink(outside_art, link_art) == 0);

        char idx3[4096];
        snprintf(idx3, sizeof idx3,
                 "RBB1 %s v1:99:1700000000:ondemand:st1\n"
                 "T\t10\t240.000\t44100\t/PIONEER/USBANLZ/P001/00000001/ANLZ0000.DAT\n"
                 "T\t26\t120.000\t44100\t/PIONEER/USBANLZ/P001/00000001/ANLZ0026.DAT\n"
                 "T\t27\t120.000\t44100\t/PIONEER/USBANLZ/P001/00000001/SYMDIR/ANLZ0027.DAT\n"
                 "T\t28\t120.000\t44100\t/PIONEER/USBANLZ/P001/00000001/ANLZ0028.DAT\n"
                 "T\t29\t120.000\t44100\t/PIONEER/USBANLZ/P001/00000001/ANLZ0029.DAT\n"
                 "A\tJcGzLMZmZphFwIWYUrbI7LyECvA\t/PIONEER/Artwork/00001/a1.jpg\n"
                 "A\tSYMLINKART\t/PIONEER/Artwork/00001/sym.jpg\n",
                 uuid);
        write_file_all(idx_path, idx3, strlen(idx3));
        pthread_mutex_lock(&sc_rbb_mu);
        for (int i = 0; i < SC_RBB_MAX_DRIVES; i++) {
            if (sc_rbb_tbl[i].used && sc_rbb_tbl[i].has_index) {
                sc_rbb_tbl[i].last_check = 0;
                sc_rbb_tbl[i].idx_size = 0;
            }
        }
        pthread_mutex_unlock(&sc_rbb_mu);

        /* Tagged RBB1 header must still parse pure uuid and open Track 10 + Artwork a1.jpg via sc_rbb_open_artwork. */
        path_fmt(rgb_req, sizeof rgb_req, ovdir, "/10.rgb");
        int tagged_fd = sc_rbb_open_rgb(rgb_req, O_RDONLY | O_CLOEXEC);
        assert(tagged_fd >= 0);
        close(tagged_fd);
        int art_fd = sc_rbb_open_artwork(valid_art, O_RDONLY | O_CLOEXEC);
        assert(art_fd >= 0);
        int fl = fcntl(art_fd, F_GETFL);
        assert(fl >= 0 && (fl & O_NONBLOCK) == 0);
        close(art_fd);

        /* Leaf symlink -> rejected */
        path_fmt(rgb_req, sizeof rgb_req, ovdir, "/26.rgb");
        int sym_fd = sc_rbb_open_rgb(rgb_req, O_RDONLY | O_CLOEXEC);
        if (sym_fd >= 0) {
            fprintf(stderr, "REVIEW PROOF FAIL (Finding 4): sc_rbb_open_rgb followed ANLZ symlink outside mount (fd=%d)\n", sym_fd);
            close(sym_fd);
            review_failures++;
        }

        /* Intermediate dir symlink -> rejected */
        path_fmt(rgb_req, sizeof rgb_req, ovdir, "/27.rgb");
        int symdir_fd = sc_rbb_open_rgb(rgb_req, O_RDONLY | O_CLOEXEC);
        if (symdir_fd >= 0) {
            fprintf(stderr, "REVIEW PROOF FAIL (Finding 4): sc_rbb_open_rgb followed intermediate symlink dir (fd=%d)\n", symdir_fd);
            close(symdir_fd);
            review_failures++;
        }

        /* FIFO at ANLZ -> nonblocking rejection (no hang!) */
        path_fmt(rgb_req, sizeof rgb_req, ovdir, "/28.rgb");
        assert(sc_rbb_open_rgb(rgb_req, O_RDONLY | O_CLOEXEC) < 0);

        /* Artwork symlink -> rejected by both sc_rbb_map_artwork and sc_rbb_open_artwork */
        char sym_art_req[PATH_MAX];
        path_fmt(sym_art_req, sizeof sym_art_req, stick, "/Engine Library/Artwork/SYMLINKART.jpg");
        assert(sc_rbb_map_artwork(sym_art_req, art_mapped) == 0);
        assert(sc_rbb_open_artwork(sym_art_req, O_RDONLY | O_CLOEXEC) < 0);

        /* C-NOTE 5a & 5b: Negative-cache TTL expires and duplicate cache slot is avoided even with prior empty slots. */
        path_fmt(rgb_req, sizeof rgb_req, ovdir, "/29.rgb");
        assert(sc_rbb_open_rgb(rgb_req, O_RDONLY | O_CLOEXEC) < 0);
        char p29[PATH_MAX];
        path_fmt(p29, sizeof p29, anlzdir, "/ANLZ0029.2EX");
        write_anlz_pwv6(p29, 1200, pwv6, 0);
        /* Still negatively cached right now: */
        assert(sc_rbb_open_rgb(rgb_req, O_RDONLY | O_CLOEXEC) < 0);
        /* Force negative TTL expiry and verify recovery + no duplicate cache slot on concurrent race. */
        pthread_mutex_lock(&sc_rbb_mu);
        uint64_t g0 = sc_rbb_tbl[0].gen;
        int neg_idx = -1;
        for (int i = 0; i < SC_RBB_CACHE_CAP; i++) {
            if (sc_rbb_cache[i].state == 2 && sc_rbb_cache[i].drive_gen == g0 && sc_rbb_cache[i].track_id == 29) {
                neg_idx = i;
                break;
            }
        }
        assert(neg_idx >= 0);
        sc_rbb_cache[neg_idx].neg_until = sc_rbb_mono_sec() - 1;
        pthread_mutex_unlock(&sc_rbb_mu);
        int rec_fd = sc_rbb_open_rgb(rgb_req, O_RDONLY | O_CLOEXEC);
        assert(rec_fd >= 0);
        close(rec_fd);
        /* Simulate concurrent insert race: clear entry 29 so lookup misses, then while building,
         * hook populates slot 7 with (g0, 29) and leaves slot 0 empty. Must NOT insert a duplicate! */
        pthread_mutex_lock(&sc_rbb_mu);
        for (int i = 0; i < SC_RBB_CACHE_CAP; i++) {
            if (sc_rbb_cache[i].state != 0 && sc_rbb_cache[i].drive_gen == g0 && sc_rbb_cache[i].track_id == 29)
                sc_rbb_cache[i].state = 0;
        }
        g_dup_race_gen = g0;
        sc_rbb_on_cache_insert_hook = inject_concurrent_dup_slot;
        pthread_mutex_unlock(&sc_rbb_mu);
        rec_fd = sc_rbb_open_rgb(rgb_req, O_RDONLY | O_CLOEXEC);
        assert(rec_fd >= 0);
        close(rec_fd);
        sc_rbb_on_cache_insert_hook = NULL;
        pthread_mutex_lock(&sc_rbb_mu);
        int matches_29 = 0;
        for (int i = 0; i < SC_RBB_CACHE_CAP; i++) {
            if (sc_rbb_cache[i].state != 0 && sc_rbb_cache[i].drive_gen == g0 && sc_rbb_cache[i].track_id == 29)
                matches_29++;
        }
        pthread_mutex_unlock(&sc_rbb_mu);
        assert(matches_29 == 1);

        assert(review_failures == 0);
    }

    /* Clean up temp dir */
    char cmd[PATH_MAX + 32];
    snprintf(cmd, sizeof cmd, "rm -rf '%s'", root);
    assert(system(cmd) == 0);
    printf("test_rekordbox_browse: PASS\n");
    return 0;
}
