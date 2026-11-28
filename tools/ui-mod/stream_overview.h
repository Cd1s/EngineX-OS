#ifndef SC_STREAM_OVERVIEW_H
#define SC_STREAM_OVERVIEW_H

#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#define SC_STREAM_OV_MAGIC "EXOV1"
#define SC_STREAM_OV_HDR_LEN 17u
#define SC_STREAM_OV_RAW_LEN 3099u
#define SC_STREAM_OV_BINS 1024u
#define SC_STREAM_OV_MAX_ZLEN 16384u
#define SC_STREAM_OV_EDGE (64u << 10)

// Overridden in unit tests.
static const char *sc_stream_ov_dir = "/run/enginex-stream-overview";
static const char *sc_stream_ov_downloads = "/data/downloads/TIDAL/";
static const char *sc_stream_ov_downloads_rel = "../../../../downloads/TIDAL/";
static const char *sc_stream_ov_drives = "/media/";

static uint32_t sc_ov_rotr32(uint32_t x, int n) { return (x >> n) | (x << (32 - n)); }

struct sc_ov_sha256 {
    uint32_t state[8];
    uint64_t count;
    unsigned char buf[64];
};

static void sc_ov_sha256_block(uint32_t s[8], const unsigned char b[64]) {
    static const uint32_t k[64] = {
        0x428a2f98u, 0x71374491u, 0xb5c0fbcfu, 0xe9b5dba5u, 0x3956c25bu, 0x59f111f1u, 0x923f82a4u, 0xab1c5ed5u,
        0xd807aa98u, 0x12835b01u, 0x243185beu, 0x550c7dc3u, 0x72be5d74u, 0x80deb1feu, 0x9bdc06a7u, 0xc19bf174u,
        0xe49b69c1u, 0xefbe4786u, 0x0fc19dc6u, 0x240ca1ccu, 0x2de92c6fu, 0x4a7484aau, 0x5cb0a9dcu, 0x76f988dau,
        0x983e5152u, 0xa831c66du, 0xb00327c8u, 0xbf597fc7u, 0xc6e00bf3u, 0xd5a79147u, 0x06ca6351u, 0x14292967u,
        0x27b70a85u, 0x2e1b2138u, 0x4d2c6dfcu, 0x53380d13u, 0x650a7354u, 0x766a0abbu, 0x81c2c92eu, 0x92722c85u,
        0xa2bfe8a1u, 0xa81a664bu, 0xc24b8b70u, 0xc76c51a3u, 0xd192e819u, 0xd6990624u, 0xf40e3585u, 0x106aa070u,
        0x19a4c116u, 0x1e376c08u, 0x2748774cu, 0x34b0bcb5u, 0x391c0cb3u, 0x4ed8aa4au, 0x5b9cca4fu, 0x682e6ff3u,
        0x748f82eeu, 0x78a5636fu, 0x84c87814u, 0x8cc70208u, 0x90befffau, 0xa4506cebu, 0xbef9a3f7u, 0xc67178f2u,
    };
    uint32_t w[64], a = s[0], c0 = s[1], c = s[2], d = s[3], e = s[4], f = s[5], g = s[6], h = s[7];
    for (int i = 0; i < 16; i++)
        w[i] = ((uint32_t)b[i * 4] << 24) | ((uint32_t)b[i * 4 + 1] << 16) | ((uint32_t)b[i * 4 + 2] << 8) | b[i * 4 + 3];
    for (int i = 16; i < 64; i++) {
        uint32_t s0 = sc_ov_rotr32(w[i - 15], 7) ^ sc_ov_rotr32(w[i - 15], 18) ^ (w[i - 15] >> 3);
        uint32_t s1 = sc_ov_rotr32(w[i - 2], 17) ^ sc_ov_rotr32(w[i - 2], 19) ^ (w[i - 2] >> 10);
        w[i] = w[i - 16] + s0 + w[i - 7] + s1;
    }
    for (int i = 0; i < 64; i++) {
        uint32_t S1 = sc_ov_rotr32(e, 6) ^ sc_ov_rotr32(e, 11) ^ sc_ov_rotr32(e, 25);
        uint32_t ch = (e & f) ^ ((~e) & g);
        uint32_t t1 = h + S1 + ch + k[i] + w[i];
        uint32_t S0 = sc_ov_rotr32(a, 2) ^ sc_ov_rotr32(a, 13) ^ sc_ov_rotr32(a, 22);
        uint32_t maj = (a & c0) ^ (a & c) ^ (c0 & c);
        uint32_t t2 = S0 + maj;
        h = g; g = f; f = e; e = d + t1; d = c; c = c0; c0 = a; a = t1 + t2;
    }
    s[0] += a; s[1] += c0; s[2] += c; s[3] += d; s[4] += e; s[5] += f; s[6] += g; s[7] += h;
}

static void sc_ov_sha256_init(struct sc_ov_sha256 *ctx) {
    static const uint32_t init[8] = {0x6a09e667u, 0xbb67ae85u, 0x3c6ef372u, 0xa54ff53au,
                                     0x510e527fu, 0x9b05688cu, 0x1f83d9abu, 0x5be0cd19u};
    memcpy(ctx->state, init, sizeof init);
    ctx->count = 0;
}

static void sc_ov_sha256_update(struct sc_ov_sha256 *ctx, const unsigned char *data, size_t len) {
    size_t idx = (size_t)(ctx->count & 63u);
    ctx->count += len;
    if (idx && idx + len >= 64u) {
        memcpy(ctx->buf + idx, data, 64u - idx);
        sc_ov_sha256_block(ctx->state, ctx->buf);
        data += 64u - idx;
        len -= 64u - idx;
        idx = 0;
    }
    while (len >= 64u) {
        sc_ov_sha256_block(ctx->state, data);
        data += 64u;
        len -= 64u;
    }
    if (len) memcpy(ctx->buf + idx, data, len);
}

static void sc_ov_sha256_hex(struct sc_ov_sha256 *ctx, char hex_out[65]) {
    uint64_t total_bits = ctx->count * 8u;
    unsigned char pad[64] = {0x80};
    size_t idx = (size_t)(ctx->count & 63u);
    size_t pad_len = idx < 56u ? 56u - idx : 120u - idx;
    sc_ov_sha256_update(ctx, pad, pad_len);
    unsigned char len_be[8];
    for (int i = 0; i < 8; i++) len_be[i] = (unsigned char)(total_bits >> (56 - 8 * i));
    sc_ov_sha256_update(ctx, len_be, 8);
    static const char digits[] = "0123456789abcdef";
    for (int i = 0; i < 8; i++) {
        for (int s = 28; s >= 0; s -= 4) *hex_out++ = digits[(ctx->state[i] >> s) & 15u];
    }
    *hex_out = '\0';
}

static uint32_t sc_ov_crc32(const unsigned char *data, size_t len) {
    uint32_t crc = 0xffffffffu;
    for (size_t i = 0; i < len; i++) {
        crc ^= data[i];
        for (int b = 0; b < 8; b++) crc = (crc >> 1) ^ (0xedb88320u & (uint32_t)(-(int32_t)(crc & 1u)));
    }
    return ~crc;
}

static uint32_t sc_ov_le32(const unsigned char *p) {
    return (uint32_t)p[0] | ((uint32_t)p[1] << 8) | ((uint32_t)p[2] << 16) | ((uint32_t)p[3] << 24);
}

static uint64_t sc_ov_be64(const unsigned char *p) {
    uint64_t v = 0;
    for (int i = 0; i < 8; i++) v = (v << 8) | p[i];
    return v;
}

static int sc_ov_pread_full(int fd, unsigned char *buf, size_t want, off_t off) {
    size_t got = 0;
    while (got < want) {
        ssize_t n = pread(fd, buf + got, want - got, off + (off_t)got);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) return 0;
        got += (size_t)n;
    }
    return 1;
}

// Returns pointer to 40 lowercase hex chars if path is <prefix><40hex>.<audio-ext>, else NULL.
static const char *sc_stream_ov_download_key(const char *path, const char *prefix) {
    if (!path || !prefix) return NULL;
    size_t n = strlen(prefix);
    if (strncmp(path, prefix, n)) return NULL;
    const char *key = path + n;
    if (strspn(key, "0123456789abcdef") != 40) return NULL;
    const char *ext = key + 40;
    return !strcmp(ext, ".flac") || !strcmp(ext, ".mp3") || !strcmp(ext, ".wav") ||
           !strcmp(ext, ".aif") || !strcmp(ext, ".aiff") || !strcmp(ext, ".aifc") ||
           !strcmp(ext, ".ogg") ? key : NULL;
}

// Hashes uint64_le(size) || head_64k [|| tail_64k if size > 64 KiB].
static int sc_stream_ov_fingerprint(const char *path, char hex_out[65]) {
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) return 0;
    struct stat st;
    if (fstat(fd, &st) || !S_ISREG(st.st_mode) || st.st_size < 0) {
        close(fd);
        return 0;
    }
    uint64_t size = (uint64_t)st.st_size;
    unsigned char len_le[8];
    for (int i = 0; i < 8; i++) len_le[i] = (unsigned char)(size >> (8 * i));

    struct sc_ov_sha256 sha;
    sc_ov_sha256_init(&sha);
    sc_ov_sha256_update(&sha, len_le, 8);

    unsigned char *buf = malloc(SC_STREAM_OV_EDGE);
    if (!buf) {
        close(fd);
        return 0;
    }
    size_t first = size < SC_STREAM_OV_EDGE ? (size_t)size : SC_STREAM_OV_EDGE;
    if (first > 0) {
        if (!sc_ov_pread_full(fd, buf, first, 0)) {
            free(buf);
            close(fd);
            return 0;
        }
        sc_ov_sha256_update(&sha, buf, first);
    }
    if (size > SC_STREAM_OV_EDGE) {
        if (!sc_ov_pread_full(fd, buf, SC_STREAM_OV_EDGE, (off_t)(size - SC_STREAM_OV_EDGE))) {
            free(buf);
            close(fd);
            return 0;
        }
        sc_ov_sha256_update(&sha, buf, SC_STREAM_OV_EDGE);
    }
    free(buf);
    close(fd);
    sc_ov_sha256_hex(&sha, hex_out);
    return 1;
}

static int sc_stream_ov_validate_raw(const unsigned char *raw) {
    if (sc_ov_be64(raw) != SC_STREAM_OV_BINS || sc_ov_be64(raw + 8) != SC_STREAM_OV_BINS) return 0;
    uint64_t step_bits = sc_ov_be64(raw + 16);
    uint64_t exp = (step_bits >> 52) & 0x7ffu;
    if ((step_bits >> 63) || exp == 0x7ffu || !step_bits) return 0; // require positive finite double without libm
    unsigned char maxb[3] = {0, 0, 0};
    for (size_t i = 0; i < SC_STREAM_OV_BINS; i++) {
        for (int c = 0; c < 3; c++) {
            unsigned char v = raw[24 + i * 3 + (size_t)c];
            if (v > 127) return 0;
            if (v > maxb[c]) maxb[c] = v;
        }
    }
    const unsigned char *trl = raw + SC_STREAM_OV_RAW_LEN - 3;
    return trl[0] == maxb[0] && trl[1] == maxb[1] && trl[2] == maxb[2];
}

// Accumulates chunked OA_OVERVIEW signals emitted incrementally by OfflineAnalyzer
// (header >Q(chunk_bins) >Q(total_bins=1024) >d(step) + chunk_bins*3 + 3-byte trailer)
// into a complete 1024-bin overview before emitting or swapping at OA_DONE.
struct sc_stream_ov_accum {
    int passthrough;
    size_t bins;
    unsigned char raw[SC_STREAM_OV_RAW_LEN];
};

static void sc_stream_ov_accum_reset(struct sc_stream_ov_accum *a) {
    if (a) memset(a, 0, sizeof *a);
}

static int sc_stream_ov_accum_push(struct sc_stream_ov_accum *a, const unsigned char *data, size_t len) {
    if (!a || a->passthrough || !data || len < 27u) return 0;
    uint64_t chunk_n = sc_ov_be64(data);
    uint64_t total_n = sc_ov_be64(data + 8);
    if (total_n != SC_STREAM_OV_BINS || chunk_n == 0 || chunk_n > SC_STREAM_OV_BINS) return 0;
    if (len != 24u + (size_t)chunk_n * 3u + 3u) return 0;
    if (a->bins + (size_t)chunk_n > SC_STREAM_OV_BINS) return 0;
    memcpy(a->raw + 16, data + 16, 8);
    memcpy(a->raw + 24 + a->bins * 3u, data + 24, (size_t)chunk_n * 3u);
    memcpy(a->raw + SC_STREAM_OV_RAW_LEN - 3u, data + len - 3u, 3);
    a->bins += (size_t)chunk_n;
    return 1;
}

static int sc_stream_ov_accum_finish(struct sc_stream_ov_accum *a, size_t *out_len) {
    if (!a || a->bins == 0 || a->bins > SC_STREAM_OV_BINS) return 0;
    uint64_t bins = (uint64_t)a->bins, total = SC_STREAM_OV_BINS;
    for (int i = 0; i < 8; i++) {
        a->raw[i] = (unsigned char)(bins >> (56 - 8 * i));
        a->raw[8 + i] = (unsigned char)(total >> (56 - 8 * i));
    }
    size_t body_end = 24u + a->bins * 3u;
    if (body_end != SC_STREAM_OV_RAW_LEN - 3u)
        memmove(a->raw + body_end, a->raw + SC_STREAM_OV_RAW_LEN - 3u, 3);
    if (out_len) *out_len = body_end + 3u;
    return 1;
}

static int sc_stream_ov_parse(const unsigned char *buf, size_t len, const unsigned char **raw_out,
                                const unsigned char **z_out, size_t *z_len_out) {
    if (!buf || len < SC_STREAM_OV_HDR_LEN + SC_STREAM_OV_RAW_LEN + 6u) return 0;
    if (memcmp(buf, SC_STREAM_OV_MAGIC, 5)) return 0;
    uint32_t raw_len = sc_ov_le32(buf + 5);
    uint32_t z_len = sc_ov_le32(buf + 9);
    uint32_t want_crc = sc_ov_le32(buf + 13);
    if (raw_len != SC_STREAM_OV_RAW_LEN || z_len <= 6u || z_len > SC_STREAM_OV_MAX_ZLEN) return 0;
    if ((size_t)SC_STREAM_OV_HDR_LEN + (size_t)raw_len + (size_t)z_len != len) return 0;
    const unsigned char *payload = buf + SC_STREAM_OV_HDR_LEN;
    if (sc_ov_crc32(payload, (size_t)raw_len + (size_t)z_len) != want_crc) return 0;
    if (!sc_stream_ov_validate_raw(payload)) return 0;
    const unsigned char *z = payload + raw_len;
    // Qt qCompress prefix: 4-byte BE uncompressed size (3099 = 0x00000c1b) followed by zlib header.
    if (z[0] != 0 || z[1] != 0 || z[2] != 0x0c || z[3] != 0x1b) return 0;
    if ((z[4] & 0x0fu) != 8u || ((((uint16_t)z[4] << 8) | z[5]) % 31u) != 0) return 0;
    if (raw_out) *raw_out = payload;
    if (z_out) *z_out = z;
    if (z_len_out) *z_len_out = z_len;
    return 1;
}

// Non-blocking read of <dir>/<fp>.ov. Returns malloc'd buffer on valid EXOV1,
// setting *status_out = 1. On missing/late sidecar returns NULL with *status_out = 0;
// on corrupt/unreadable sidecar returns NULL with *status_out = -1.
static unsigned char *sc_stream_ov_load(const char *audio_path, const unsigned char **raw_out,
                                        const unsigned char **z_out, size_t *z_len_out, int *status_out) {
    if (status_out) *status_out = 0;
    if (!audio_path || !sc_stream_ov_download_key(audio_path, sc_stream_ov_downloads)) return NULL;
    char fp[65];
    if (!sc_stream_ov_fingerprint(audio_path, fp)) return NULL;
    char sidecar[PATH_MAX];
    if (snprintf(sidecar, sizeof sidecar, "%s/%s.ov", sc_stream_ov_dir, fp) >= (int)sizeof sidecar) return NULL;
    int fd = open(sidecar, O_RDONLY | O_CLOEXEC | O_NOFOLLOW | O_NONBLOCK);
    if (fd < 0) {
        if (status_out && errno != ENOENT) *status_out = -1;
        return NULL;
    }
    struct stat st;
    unsigned char *buf = NULL;
    size_t size = 0, got = 0;
    if (!fstat(fd, &st) && S_ISREG(st.st_mode) &&
        st.st_size >= (off_t)(SC_STREAM_OV_HDR_LEN + SC_STREAM_OV_RAW_LEN + 6u) &&
        st.st_size <= (off_t)(SC_STREAM_OV_HDR_LEN + SC_STREAM_OV_RAW_LEN + SC_STREAM_OV_MAX_ZLEN)) {
        size = (size_t)st.st_size;
        buf = malloc(size);
        while (buf && got < size) {
            ssize_t n = read(fd, buf + got, size - got);
            if (n < 0 && errno == EINTR) continue;
            if (n <= 0) break;
            got += (size_t)n;
        }
    }
    close(fd);
    if (!buf || got != size || !sc_stream_ov_parse(buf, size, raw_out, z_out, z_len_out)) {
        free(buf);
        if (status_out) *status_out = -1;
        return NULL;
    }
    if (status_out) *status_out = 1;
    return buf;
}

static int sc_stream_ov_read_raw(const char *audio_path, unsigned char raw_out[SC_STREAM_OV_RAW_LEN], int *status_out) {
    const unsigned char *raw = NULL;
    unsigned char *buf = sc_stream_ov_load(audio_path, &raw, NULL, NULL, status_out);
    if (!buf) return 0;
    memcpy(raw_out, raw, SC_STREAM_OV_RAW_LEN);
    free(buf);
    return 1;
}

struct sc_stream_ov_sqlite {
    int (*exec)(void *, const char *, int (*)(void *, int, char **, char **), void *, char **);
    const char *(*filename)(void *, const char *);
};

struct sc_stream_ov_row {
    int ok;
    char key_ext[64];
};

static int sc_stream_ov_row_cb(void *arg, int n, char **v, char **names) {
    (void)names;
    struct sc_stream_ov_row *row = arg;
    if (n != 4 || !v[0] || !v[2] || !v[3] || v[3][0] != '1') return 0;
    if (strncmp(v[0], "streaming://TIDAL/Track/", 24)) return 0;
    if (v[1] && !strncmp(v[1], "rekordbox [", 11)) return 0;
    const char *key = sc_stream_ov_download_key(v[2], sc_stream_ov_downloads_rel);
    if (!key) return 0;
    snprintf(row->key_ext, sizeof row->key_ext, "%s", key);
    row->ok = 1;
    return 0;
}

static int sc_stream_ov_changes(void *arg, int n, char **v, char **names) {
    (void)names;
    if (n == 1 && v[0]) *(int *)arg = !strcmp(v[0], "1");
    return 0;
}

// Upgrades an already-analyzed streamed track's overviewWaveFormData on load #2+
// if its peak sidecar finished late after OfflineAnalyzer completed on load #1.
// Touches ONLY overviewWaveFormData; leaves rekordbox, drive, and unanalyzed rows untouched.
static inline const char *sc_stream_ov_fill_track(const struct sc_stream_ov_sqlite *q, void *db, long long track) {
    if (!q || !q->exec || !q->filename || !db || track <= 0) return NULL;
    char ready[PATH_MAX];
    if (snprintf(ready, sizeof ready, "%s/.ready", sc_stream_ov_dir) >= (int)sizeof ready) return NULL;
    struct stat st;
    if (stat(ready, &st)) return NULL;

    const char *main_path = q->filename(db, "main");
    if (main_path && !strncmp(main_path, sc_stream_ov_drives, strlen(sc_stream_ov_drives))) return NULL;

    char query[256];
    snprintf(query, sizeof query,
             "SELECT t.uri, coalesce(t.label,''), t.path, p.trackData IS NOT NULL "
             "FROM Track t JOIN PerformanceData p ON p.trackId = t.id WHERE t.id = %lld",
             track);
    struct sc_stream_ov_row row = {.ok = 0};
    if (q->exec(db, query, sc_stream_ov_row_cb, &row, NULL) || !row.ok) return NULL;

    char audio_path[PATH_MAX];
    if (snprintf(audio_path, sizeof audio_path, "%s%s", sc_stream_ov_downloads, row.key_ext) >= (int)sizeof audio_path)
        return NULL;

    const unsigned char *z = NULL;
    size_t z_len = 0;
    int status = 0;
    unsigned char *buf = sc_stream_ov_load(audio_path, NULL, &z, &z_len, &status);
    if (!buf) return NULL;

    char *sql = malloc(320 + 4 * z_len);
    if (!sql) {
        free(buf);
        return NULL;
    }
    static const char digits[] = "0123456789abcdef";
    char *p = sql + sprintf(sql, "UPDATE PerformanceData SET overviewWaveFormData = X'");
    for (size_t i = 0; i < z_len; i++) {
        *p++ = digits[z[i] >> 4];
        *p++ = digits[z[i] & 15];
    }
    p += sprintf(p, "' WHERE trackId = %lld AND trackData IS NOT NULL AND overviewWaveFormData IS NOT X'", track);
    for (size_t i = 0; i < z_len; i++) {
        *p++ = digits[z[i] >> 4];
        *p++ = digits[z[i] & 15];
    }
    memcpy(p, "'; SELECT changes()", 20);

    int changed = 0;
    int err = q->exec(db, sql, sc_stream_ov_changes, &changed, NULL);
    free(sql);
    free(buf);
    return err ? "update failed" : (changed ? "updated" : NULL);
}

#endif
