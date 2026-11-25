/* #336 S8: FLAC parser false-sync resilience and avcodec_decode_audio4 partial-decode guard.
 *
 * Root cause in stock Engine 5.3.0 (Gen-1 armhf and Gen-2 aarch64, FFmpeg 4.4.3 /
 * libavformat.so.58.76.100 + libavcodec.so.58.134.100):
 *  1. Stock FFmpeg 4.4.3 flac_parser.c lacks upstream fixes 49597300e8 (#9185 subframe
 *     header check), 0912e79376 (#9621 missed CRC check), 5ca781598e (#7684 scoring),
 *     2d7eacee24 (#25766 variable-blocksize false syncs in fixed streams), 2ed5925e26
 *     (#9270 10-byte frames), and 681025faf1 (EOF flush leftovers). Quiet/dithered
 *     sections encoded by libFLAC (Rice k=0 zero residuals = 0xFF runs) can produce
 *     >= 4 false syncs inside one frame, exceeding FLAC_MAX_SEQUENTIAL_HEADERS (4)
 *     and causing flac_parser to emit a multi-frame "Junk frame" packet.
 *  2. Stock Engine's 16ffmpegFileReader::readSamples (0x17d5b50) calls legacy
 *     avcodec_decode_audio4 once per packet and frees the packet without consuming
 *     ret < pkt->size partial decodes. In FFmpeg 4.4.3 compat_decode(), an underread
 *     leaves unconsumed bytes in avci->ds.in_pkt and sets compat_decode_partial_size > 0;
 *     the next packet triggers AVERROR(EINVAL), the packet after that jams
 *     avci->bsf->internal->buffer_pkt, and every remaining packet until EOF fails with
 *     AVERROR_BUG (-558323010). The resulting short read makes audioCache->write (0x17cb978)
 *     return false and aborts the deck audio loader permanently (silence for the rest of
 *     the track while the pre-rendered waveform keeps advancing).
 *
 * This header is pure C11/POSIX with zero link-time FFmpeg dependencies and runs only on
 * Engine's non-RT background decoding/analysis threads (zero RT audio-callback cost).
 */
#ifndef ENGINEX_FLAC_GUARD_H
#define ENGINEX_FLAC_GUARD_H

#include <pthread.h>
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#define FG_AV_CODEC_ID_FLAC         0x1500c
#define FG_AVERROR_BUG              (-558323010) /* -MKTAG('B','U','G','!') */
#define FG_PARSER_FLAG_COMPLETE     0x0001
#define FG_PARSER_FLAG_USE_CODEC_TS 0x1000
#define FG_AV_INPUT_PADDING         64
#define FG_FLAC_MIN_FRAME           10
#define FG_FLAC_HDR_VERIFY          17 /* max 16-byte FLAC frame header + 1 subframe byte */
#define FG_FLAC_MAX_FIFO            (2u * 1024u * 1024u)
#define FG_FLAC_MAX_APPEND          16384u
#define FG_FLAC_SLOTS               64

/* Empirically verified against stock Engine 5.3.0 libavcodec.so.58.134.100
 * on both armhf (32-bit) and aarch64 (64-bit). */
#if defined(__LP64__) || defined(__aarch64__) || defined(__x86_64__)
#define FG_AVCTX_CODEC_ID_OFF       24
#define FG_AVCTX_EXTRADATA_OFF      88
#define FG_AVCTX_EXTRADATA_SIZE_OFF 96
#define FG_AVCTX_SAMPLE_RATE_OFF    400
#define FG_AVCTX_CHANNELS_OFF       404
#define FG_AVCTX_CH_LAYOUT_OFF      432
#else
#define FG_AVCTX_CODEC_ID_OFF       16
#define FG_AVCTX_EXTRADATA_OFF      68
#define FG_AVCTX_EXTRADATA_SIZE_OFF 72
#define FG_AVCTX_SAMPLE_RATE_OFF    344
#define FG_AVCTX_CHANNELS_OFF       348
#define FG_AVCTX_CH_LAYOUT_OFF      376
#endif

typedef struct fg_avpacket {
    void *buf;
    int64_t pts;
    int64_t dts;
    uint8_t *data;
    int size;
    int stream_index;
    int flags;
    void *side_data;
    int side_data_elems;
    int64_t duration;
    int64_t pos;
    int64_t convergence_duration;
} fg_avpacket;

typedef struct fg_avcodec_parser {
    int codec_ids[5];
    int priv_data_size;
    int (*parser_init)(void *s);
    int (*parser_parse)(void *s, void *avctx,
                        const uint8_t **poutbuf, int *poutbuf_size,
                        const uint8_t *buf, int buf_size);
    void (*parser_close)(void *s);
    int (*split)(void *avctx, const uint8_t *buf, int buf_size);
    struct fg_avcodec_parser *next;
} fg_avcodec_parser;

typedef struct fg_parser_ctx {
    void *priv_data;
    fg_avcodec_parser *parser;
    int64_t frame_offset;
    int64_t cur_offset;
    int64_t next_frame_offset;
    int pict_type;
    int repeat_pict;
    int64_t pts;
    int64_t dts;
    int64_t last_pts;
    int64_t last_dts;
    int fetch_timestamp;
    int cur_frame_start_index;
    int64_t cur_frame_offset[4];
    int64_t cur_frame_pts[4];
    int64_t cur_frame_dts[4];
    int flags;
    int64_t offset;
    int64_t cur_frame_end[4];
    int key_frame;
    int64_t convergence_duration;
    int dts_sync_point;
    int dts_ref_dts_delta;
    int pts_dts_delta;
    int64_t cur_frame_pos[4];
    int64_t pos;
    int64_t last_pos;
    int duration;
} fg_parser_ctx;

#if defined(__LP64__) || defined(__aarch64__) || defined(__x86_64__)
_Static_assert(offsetof(fg_avpacket, data) == 24 && offsetof(fg_avpacket, size) == 32, "64-bit AVPacket ABI");
_Static_assert(offsetof(fg_avcodec_parser, parser_parse) == 32, "64-bit AVCodecParser ABI");
_Static_assert(offsetof(fg_parser_ctx, flags) == 184 && offsetof(fg_parser_ctx, duration) == 312, "64-bit AVCodecParserContext ABI");
#else
_Static_assert(offsetof(fg_avpacket, data) == 24 && offsetof(fg_avpacket, size) == 28, "32-bit AVPacket ABI");
_Static_assert(offsetof(fg_avcodec_parser, parser_parse) == 28, "32-bit AVCodecParser ABI");
_Static_assert(offsetof(fg_parser_ctx, flags) == 176 && offsetof(fg_parser_ctx, duration) == 304, "32-bit AVCodecParserContext ABI");
#endif

typedef struct fg_flac_streaminfo {
    int valid;
    int min_bs;
    int max_bs;
    int max_framesize;
    int samplerate;
    int channels;
    int bps;
} fg_flac_streaminfo;

typedef struct fg_flac_frame_info {
    int is_var_size;
    int blocksize;
    int samplerate;
    int channels;
    int bps;
    int64_t frame_or_sample_num;
} fg_flac_frame_info;

typedef struct fg_flac_stream {
    uint8_t *buf;
    size_t cap;
    size_t head;
    size_t len;
    uint8_t *out;
    size_t out_cap;
    int have_sync;
    fg_flac_frame_info cur_fi;
    int last_fixed_bs;
    size_t scan_pos;
    uint16_t scan_crc;
    int eof_done;
} fg_flac_stream;

static uint8_t fg_crc8_tab[256];
static uint16_t fg_crc16_tab[256];
static pthread_once_t fg_crc_once = PTHREAD_ONCE_INIT;

static void fg_init_crc_tables(void) {
    for (unsigned i = 0; i < 256; i++) {
        uint8_t c8 = (uint8_t)i;
        for (int b = 0; b < 8; b++)
            c8 = (c8 & 0x80u) ? (uint8_t)((c8 << 1) ^ 0x07u) : (uint8_t)(c8 << 1);
        fg_crc8_tab[i] = c8;

        uint16_t c16 = (uint16_t)(i << 8);
        for (int b = 0; b < 8; b++)
            c16 = (c16 & 0x8000u) ? (uint16_t)((c16 << 1) ^ 0x8005u) : (uint16_t)(c16 << 1);
        fg_crc16_tab[i] = c16;
    }
}

static inline uint8_t fg_crc8_compute(const uint8_t *p, size_t n) {
    uint8_t c = 0;
    for (size_t i = 0; i < n; i++) c = fg_crc8_tab[c ^ p[i]];
    return c;
}

static inline uint16_t fg_crc16_step(uint16_t c, uint8_t b) {
    return (uint16_t)((c << 8) ^ fg_crc16_tab[((c >> 8) ^ b) & 0xffu]);
}

static uint16_t fg_crc16_compute(const uint8_t *p, size_t n) {
    uint16_t c = 0;
    for (size_t i = 0; i < n; i++) c = fg_crc16_step(c, p[i]);
    return c;
}

static void fg_parse_streaminfo(const void *avctx, fg_flac_streaminfo *si) {
    memset(si, 0, sizeof *si);
    if (!avctx) return;
    const uint8_t *ext = *(const uint8_t *const *)((const char *)avctx + FG_AVCTX_EXTRADATA_OFF);
    int esz = *(const int *)((const char *)avctx + FG_AVCTX_EXTRADATA_SIZE_OFF);
    if (!ext || esz < 34) return;
    if (ext[0] == 'f' && ext[1] == 'L' && ext[2] == 'a' && ext[3] == 'C') {
        if (esz < 42) return;
        ext += 8;
    }
    int min_bs = ((int)ext[0] << 8) | ext[1];
    int max_bs = ((int)ext[2] << 8) | ext[3];
    if (min_bs < 16 || max_bs < min_bs) return;
    int max_fs = ((int)ext[7] << 16) | ((int)ext[8] << 8) | ext[9];
    int sr = ((int)ext[10] << 12) | ((int)ext[11] << 4) | (ext[12] >> 4);
    int ch = ((ext[12] >> 1) & 7) + 1;
    int bps = (((ext[12] & 1) << 4) | (ext[13] >> 4)) + 1;
    if (sr <= 0 || sr > 655350 || ch < 1 || ch > 8 || bps < 4 || bps > 32) return;
    si->valid = 1;
    si->min_bs = min_bs;
    si->max_bs = max_bs;
    si->max_framesize = max_fs;
    si->samplerate = sr;
    si->channels = ch;
    si->bps = bps;
}

/* Decode a FLAC UTF-8 coded frame or sample number (RFC 9639, 1..7 bytes). */
static int fg_decode_utf8(const uint8_t *p, size_t avail, int64_t *out, size_t *used) {
    if (avail < 1) return 0;
    uint8_t b0 = p[0];
    if ((b0 & 0x80u) == 0) {
        *out = b0;
        *used = 1;
        return 1;
    }
    if ((b0 & 0xc0u) == 0x80u || b0 == 0xffu) return 0;
    size_t extra = 0;
    uint64_t v = 0;
    if ((b0 & 0xe0u) == 0xc0u)      { extra = 1; v = b0 & 0x1fu; }
    else if ((b0 & 0xf0u) == 0xe0u) { extra = 2; v = b0 & 0x0fu; }
    else if ((b0 & 0xf8u) == 0xf0u) { extra = 3; v = b0 & 0x07u; }
    else if ((b0 & 0xfcu) == 0xf8u) { extra = 4; v = b0 & 0x03u; }
    else if ((b0 & 0xfeu) == 0xfcu) { extra = 5; v = b0 & 0x01u; }
    else if (b0 == 0xfeu)           { extra = 6; v = 0; }
    else return 0;
    if (avail < 1 + extra) return 0;
    for (size_t i = 1; i <= extra; i++) {
        if ((p[i] & 0xc0u) != 0x80u) return 0;
        v = (v << 6) | (p[i] & 0x3fu);
    }
    *out = (int64_t)v;
    *used = 1 + extra;
    return 1;
}

/* Validate FLAC frame header (RFC 9639) + CRC-8 + first subframe header byte
 * (#9185 / 49597300e8) + STREAMINFO fixed-blocksize strategy (#25766 / 2d7eacee24). */
static int fg_parse_frame_header(const uint8_t *p, size_t avail,
                                 const fg_flac_streaminfo *si,
                                 fg_flac_frame_info *fi) {
    static const int sr_tab[12] = {
        0, 88200, 176400, 192000, 8000, 16000, 22050, 24000, 32000, 44100, 48000, 96000
    };
    static const int bps_tab[8] = { 0, 8, 12, 0, 16, 20, 24, 0 };

    if (avail < 7) return 0;
    if (p[0] != 0xffu || (p[1] & 0xfeu) != 0xf8u) return 0;
    int is_var = p[1] & 1;
    if (si->valid && si->min_bs == si->max_bs && is_var) return 0;

    int bs_code = p[2] >> 4;
    int sr_code = p[2] & 0x0f;
    if (bs_code == 0 || sr_code == 15) return 0;

    int ch_code = p[3] >> 4;
    int bps_code = (p[3] >> 1) & 7;
    if (ch_code >= 11 || bps_code == 3 || bps_code == 7 || (p[3] & 1)) return 0;

    int channels = (ch_code < 8) ? (ch_code + 1) : 2;
    int bps = bps_tab[bps_code];
    if (bps == 0) bps = si->valid ? si->bps : 0;

    int64_t num = 0;
    size_t ulen = 0;
    if (!fg_decode_utf8(p + 4, avail - 4, &num, &ulen)) return 0;
    if (!is_var && num > 0x7fffffffLL) return 0;
    size_t pos = 4 + ulen;

    int blocksize = 0;
    if (bs_code == 1) blocksize = 192;
    else if (bs_code >= 2 && bs_code <= 5) blocksize = 576 << (bs_code - 2);
    else if (bs_code == 6) {
        if (pos + 1 > avail) return 0;
        blocksize = (int)p[pos++] + 1;
    } else if (bs_code == 7) {
        if (pos + 2 > avail) return 0;
        blocksize = (((int)p[pos] << 8) | p[pos + 1]) + 1;
        pos += 2;
    } else {
        blocksize = 256 << (bs_code - 8);
    }
    if (blocksize <= 0 || blocksize > 65535 || (si->valid && blocksize > si->max_bs)) return 0;

    int sr = 0;
    if (sr_code < 12) sr = sr_tab[sr_code];
    else if (sr_code == 12) {
        if (pos + 1 > avail) return 0;
        sr = (int)p[pos++] * 1000;
    } else if (sr_code == 13) {
        if (pos + 2 > avail) return 0;
        sr = ((int)p[pos] << 8) | p[pos + 1];
        pos += 2;
    } else {
        if (pos + 2 > avail) return 0;
        sr = (((int)p[pos] << 8) | p[pos + 1]) * 10;
        pos += 2;
    }
    if (sr == 0) sr = si->valid ? si->samplerate : 0;
    if (sr <= 0 || sr > 655350) return 0;

    /* Need header CRC-8 byte + first subframe header byte. */
    if (pos + 2 > avail) return 0;
    if (fg_crc8_compute(p, pos + 1) != 0) return 0;

    uint8_t sf = p[pos + 1];
    if (sf & 0x80u) return 0; /* subframe zero bit must be 0 */
    uint8_t stype = (sf >> 1) & 0x3fu;
    if (!(stype == 0 || stype == 1 || (stype >= 8 && stype <= 12) || stype >= 32)) return 0;
    if (stype >= 8 && stype <= 12 && (int)(stype - 8) > blocksize) return 0;
    if (stype >= 32 && (int)((stype & 31u) + 1u) > blocksize) return 0;

    fi->is_var_size = is_var;
    fi->blocksize = blocksize;
    fi->samplerate = sr;
    fi->channels = channels;
    fi->bps = bps;
    fi->frame_or_sample_num = num;
    return 1;
}

static inline int fg_is_successor(const fg_flac_frame_info *a, const fg_flac_frame_info *b) {
    if (a->is_var_size != b->is_var_size) return 0;
    if (a->samplerate != b->samplerate) return 0;
    if (a->channels != b->channels) return 0;
    if (a->bps != b->bps) return 0;
    int64_t expected = a->is_var_size ? (a->frame_or_sample_num + a->blocksize)
                                      : (a->frame_or_sample_num + 1);
    return b->frame_or_sample_num == expected;
}

static size_t fg_max_frame_bound(const fg_flac_streaminfo *si) {
    size_t m = 262144u;
    if (si->valid && si->max_framesize > 0) {
        size_t s = (size_t)si->max_framesize + 4096u;
        m = s > 131072u ? s : 131072u;
    }
    if (m > FG_FLAC_MAX_FIFO - 64u) m = FG_FLAC_MAX_FIFO - 64u;
    return m;
}

static int fg_fifo_append(fg_flac_stream *st, const uint8_t *src, size_t n) {
    if (n == 0) return 1;
    if (st->len + n > FG_FLAC_MAX_FIFO) return 0;
    if (st->head + st->len + n > st->cap) {
        if (st->head > 0) {
            if (st->len > 0) memmove(st->buf, st->buf + st->head, st->len);
            st->head = 0;
        }
        if (st->len + n > st->cap) {
            size_t ncap = st->cap ? st->cap : 32768u;
            while (ncap < st->len + n) ncap *= 2u;
            if (ncap > FG_FLAC_MAX_FIFO) ncap = FG_FLAC_MAX_FIFO;
            uint8_t *nb = (uint8_t *)realloc(st->buf, ncap);
            if (!nb) return 0;
            st->buf = nb;
            st->cap = ncap;
        }
    }
    memcpy(st->buf + st->head + st->len, src, n);
    st->len += n;
    return 1;
}

static void fg_fifo_drain(fg_flac_stream *st, size_t n) {
    if (n >= st->len) {
        st->head = 0;
        st->len = 0;
    } else {
        st->head += n;
        st->len -= n;
        if (st->head >= 65536u && st->head >= st->cap / 2u) {
            memmove(st->buf, st->buf + st->head, st->len);
            st->head = 0;
        }
    }
}

static int fg_emit_frame(fg_flac_stream *st, fg_parser_ctx *s, void *avctx,
                         const fg_flac_streaminfo *si,
                         const fg_flac_frame_info *fi,
                         size_t start, size_t frame_len,
                         const uint8_t **poutbuf, int *poutbuf_size) {
    static const uint64_t ch_layouts[8] = {
        0x004ULL, /* MONO */
        0x003ULL, /* STEREO */
        0x007ULL, /* SURROUND */
        0x033ULL, /* QUAD */
        0x607ULL, /* 5POINT0 */
        0x60fULL, /* 5POINT1 */
        0x70fULL, /* 6POINT1 */
        0x63fULL  /* 7POINT1 */
    };
    size_t need = frame_len + FG_AV_INPUT_PADDING;
    if (need > st->out_cap) {
        size_t ncap = st->out_cap ? st->out_cap : 16384u;
        while (ncap < need) ncap *= 2u;
        uint8_t *no = (uint8_t *)realloc(st->out, ncap);
        if (!no) return 0;
        st->out = no;
        st->out_cap = ncap;
    }
    memcpy(st->out, st->buf + st->head + start, frame_len);
    memset(st->out + frame_len, 0, FG_AV_INPUT_PADDING);

    if (avctx) {
        int *srp = (int *)((char *)avctx + FG_AVCTX_SAMPLE_RATE_OFF);
        int *chp = (int *)((char *)avctx + FG_AVCTX_CHANNELS_OFF);
        uint64_t *clp = (uint64_t *)((char *)avctx + FG_AVCTX_CH_LAYOUT_OFF);
        if (fi->channels >= 1 && fi->channels <= 8 && (*chp != fi->channels || *clp == 0)) {
            *chp = fi->channels;
            *clp = ch_layouts[fi->channels - 1];
        }
        if (fi->samplerate > 0) *srp = fi->samplerate;
    }
    s->duration = fi->blocksize;
    if (s->flags & FG_PARSER_FLAG_USE_CODEC_TS) {
        if (fi->is_var_size) {
            s->pts = fi->frame_or_sample_num;
        } else {
            int64_t bs;
            if (si->valid && si->min_bs == si->max_bs && si->min_bs > 0) {
                bs = si->min_bs;
            } else if (st->last_fixed_bs > 0) {
                bs = st->last_fixed_bs;
            } else {
                bs = fi->blocksize;
            }
            if (st->last_fixed_bs == 0 && fi->blocksize > 0)
                st->last_fixed_bs = fi->blocksize;
            s->pts = fi->frame_or_sample_num * bs;
        }
    }
    *poutbuf = st->out;
    *poutbuf_size = (int)frame_len;
    fg_fifo_drain(st, start + frame_len);
    return 1;
}

/* Try to emit a complete, CRC-16-verified FLAC frame from st->buf without consuming new input.
 * Returns 1 if a frame was emitted (*poutbuf_size > 0), 0 if more bytes are needed (or EOF done). */
static int fg_flac_try_emit(fg_flac_stream *st, fg_parser_ctx *s, void *avctx,
                            const fg_flac_streaminfo *si, int is_eof,
                            const uint8_t **poutbuf, int *poutbuf_size) {
    size_t hdr_margin = is_eof ? 7u : FG_FLAC_HDR_VERIFY;
    size_t max_frame = fg_max_frame_bound(si);

    if (st->have_sync) {
        const uint8_t *p = st->buf + st->head;
        if (st->len >= FG_FLAC_MIN_FRAME + hdr_margin) {
            size_t limit = st->len - hdr_margin;
            while (st->scan_pos < FG_FLAC_MIN_FRAME && st->scan_pos < st->len) {
                st->scan_crc = fg_crc16_step(st->scan_crc, p[st->scan_pos++]);
            }
            while (st->scan_pos <= limit) {
                if (st->scan_crc == 0 && p[st->scan_pos] == 0xffu &&
                    (p[st->scan_pos + 1] & 0xfeu) == 0xf8u) {
                    fg_flac_frame_info next_fi;
                    if (fg_parse_frame_header(p + st->scan_pos, st->len - st->scan_pos, si, &next_fi) &&
                        fg_is_successor(&st->cur_fi, &next_fi)) {
                        fg_flac_frame_info cur = st->cur_fi;
                        size_t flen = st->scan_pos;
                        if (!fg_emit_frame(st, s, avctx, si, &cur, 0, flen, poutbuf, poutbuf_size))
                            return 0;
                        st->cur_fi = next_fi;
                        st->scan_pos = 0;
                        st->scan_crc = 0;
                        return 1;
                    }
                }
                st->scan_crc = fg_crc16_step(st->scan_crc, p[st->scan_pos++]);
            }
        }
        if (is_eof && st->len >= FG_FLAC_MIN_FRAME) {
            size_t flen = st->len;
            /* Strip trailing ID3v1 tag if the frame CRC-16 validates right before it. */
            if (flen >= 128u + FG_FLAC_MIN_FRAME &&
                memcmp(p + flen - 128u, "TAG", 3) == 0 &&
                fg_crc16_compute(p, flen - 128u) == 0) {
                flen -= 128u;
            }
            fg_flac_frame_info cur = st->cur_fi;
            if (!fg_emit_frame(st, s, avctx, si, &cur, 0, flen, poutbuf, poutbuf_size))
                return 0;
            /* Drop any leftover false-sync bytes at EOF (upstream 681025faf1). */
            fg_fifo_drain(st, st->len);
            st->have_sync = 0;
            st->scan_pos = 0;
            st->scan_crc = 0;
            st->eof_done = 1;
            return 1;
        }
        if (st->len > max_frame) {
            /* Genuinely damaged frame exceeded max_frame without a valid CRC-16 successor:
             * drop the corrupt first byte and resync cleanly on the next valid pair. */
            fg_fifo_drain(st, 1);
            st->have_sync = 0;
            st->scan_pos = 0;
            st->scan_crc = 0;
        } else {
            return 0;
        }
    }

    /* Unsynchronized: locate the first valid (H_a, H_b) pair linked by CRC-16 == 0. */
    const uint8_t *p = st->buf + st->head;
    if (st->len < FG_FLAC_MIN_FRAME) {
        if (is_eof) {
            fg_fifo_drain(st, st->len);
            st->eof_done = 1;
        }
        return 0;
    }

    size_t max_a = st->len > hdr_margin ? st->len - hdr_margin : 0;
    unsigned tried = 0;
    size_t scan_budget = 0;
    for (size_t off_a = 0; off_a <= max_a && tried < 64u && scan_budget < 2097152u; off_a++) {
        if (p[off_a] != 0xffu || (p[off_a + 1] & 0xfeu) != 0xf8u) continue;
        fg_flac_frame_info fi_a;
        if (!fg_parse_frame_header(p + off_a, st->len - off_a, si, &fi_a)) continue;
        if (si->valid && (fi_a.samplerate != si->samplerate ||
                          fi_a.channels != si->channels ||
                          (si->bps && fi_a.bps && fi_a.bps != si->bps))) {
            continue;
        }
        tried++;
        size_t scan_end = st->len > hdr_margin ? st->len - hdr_margin : 0;
        if (scan_end > off_a + max_frame) scan_end = off_a + max_frame;
        if (off_a + FG_FLAC_MIN_FRAME <= scan_end) {
            uint16_t crc = fg_crc16_compute(p + off_a, FG_FLAC_MIN_FRAME);
            scan_budget += FG_FLAC_MIN_FRAME;
            for (size_t off_b = off_a + FG_FLAC_MIN_FRAME; off_b <= scan_end; off_b++) {
                if (crc == 0 && p[off_b] == 0xffu && (p[off_b + 1] & 0xfeu) == 0xf8u) {
                    fg_flac_frame_info fi_b;
                    if (fg_parse_frame_header(p + off_b, st->len - off_b, si, &fi_b) &&
                        fg_is_successor(&fi_a, &fi_b)) {
                        if (!fg_emit_frame(st, s, avctx, si, &fi_a, off_a, off_b - off_a,
                                           poutbuf, poutbuf_size))
                            return 0;
                        st->have_sync = 1;
                        st->cur_fi = fi_b;
                        st->scan_pos = 0;
                        st->scan_crc = 0;
                        return 1;
                    }
                }
                if (off_b < st->len) {
                    crc = fg_crc16_step(crc, p[off_b]);
                    if (++scan_budget >= 2097152u) break;
                }
            }
        }
        if (is_eof && st->len - off_a >= FG_FLAC_MIN_FRAME) {
            size_t flen = st->len - off_a;
            if (flen >= 128u + FG_FLAC_MIN_FRAME &&
                memcmp(p + off_a + flen - 128u, "TAG", 3) == 0 &&
                fg_crc16_compute(p + off_a, flen - 128u) == 0) {
                flen -= 128u;
            }
            if (fg_crc16_compute(p + off_a, flen) == 0) {
                if (!fg_emit_frame(st, s, avctx, si, &fi_a, off_a, flen, poutbuf, poutbuf_size))
                    return 0;
                fg_fifo_drain(st, st->len);
                st->eof_done = 1;
                return 1;
            }
        }
    }

    if (is_eof) {
        fg_fifo_drain(st, st->len);
        st->eof_done = 1;
    } else if (st->len > max_frame) {
        /* Keep only the trailing window when scanning non-FLAC / damaged input. */
        fg_fifo_drain(st, st->len - max_frame / 2u);
    }
    return 0;
}

/* ---- Parser wrapper table (maps AVCodecParserContext* -> fg_flac_stream*) ---- */
static pthread_mutex_t fg_parser_mu = PTHREAD_MUTEX_INITIALIZER;
static struct { void *ctx; fg_flac_stream *st; } fg_parser_slots[FG_FLAC_SLOTS];
static fg_avcodec_parser fg_flac_wrapper;
static fg_avcodec_parser *fg_orig_flac_parser;

static fg_flac_stream *fg_parser_lookup(void *ctx) {
    fg_flac_stream *st = NULL;
    pthread_mutex_lock(&fg_parser_mu);
    for (int i = 0; i < FG_FLAC_SLOTS; i++) {
        if (fg_parser_slots[i].ctx == ctx) {
            st = fg_parser_slots[i].st;
            break;
        }
    }
    pthread_mutex_unlock(&fg_parser_mu);
    return st;
}

static int fg_flac_safe_parse(void *raw_s, void *avctx,
                              const uint8_t **poutbuf, int *poutbuf_size,
                              const uint8_t *buf, int buf_size) {
    fg_parser_ctx *s = (fg_parser_ctx *)raw_s;
    fg_flac_stream *st = fg_parser_lookup(raw_s);
    if (!st || !s || !avctx || (s->flags & FG_PARSER_FLAG_COMPLETE) ||
        *(const int *)((const char *)avctx + FG_AVCTX_CODEC_ID_OFF) != FG_AV_CODEC_ID_FLAC) {
        if (fg_orig_flac_parser && fg_orig_flac_parser->parser_parse)
            return fg_orig_flac_parser->parser_parse(raw_s, avctx, poutbuf, poutbuf_size, buf, buf_size);
        *poutbuf = NULL;
        *poutbuf_size = 0;
        return buf_size;
    }

    pthread_once(&fg_crc_once, fg_init_crc_tables);
    fg_flac_streaminfo si;
    fg_parse_streaminfo(avctx, &si);

    /* Phase 1: emit an already-buffered complete frame without consuming new input
     * so av_parser_parse2 computes exact next_frame_offset = cur_offset - rem. */
    if (!st->eof_done &&
        fg_flac_try_emit(st, s, avctx, &si, buf_size <= 0, poutbuf, poutbuf_size)) {
        size_t rem = st->len;
        if (rem > 0 && rem < (1u << 28)) return -(int)rem;
        return 0;
    }

    /* Phase 2: buffer incoming bytes and advance cur_offset. */
    *poutbuf = NULL;
    *poutbuf_size = 0;
    if (buf_size <= 0) return 0;
    st->eof_done = 0;

    size_t take = (size_t)buf_size;
    if (take > FG_FLAC_MAX_APPEND) take = FG_FLAC_MAX_APPEND;
    if (!fg_fifo_append(st, buf, take)) {
        /* Extremely large damaged span: keep bounded and stay alive. */
        fg_fifo_drain(st, st->len);
        st->have_sync = 0;
        st->scan_pos = 0;
        st->scan_crc = 0;
        (void)fg_fifo_append(st, buf, take);
    }
    return (int)take;
}

static void fg_flac_safe_close(void *raw_s) {
    fg_flac_stream *st = NULL;
    fg_avcodec_parser *orig = NULL;
    pthread_mutex_lock(&fg_parser_mu);
    for (int i = 0; i < FG_FLAC_SLOTS; i++) {
        if (fg_parser_slots[i].ctx == raw_s) {
            st = fg_parser_slots[i].st;
            fg_parser_slots[i].ctx = NULL;
            fg_parser_slots[i].st = NULL;
            break;
        }
    }
    orig = fg_orig_flac_parser;
    pthread_mutex_unlock(&fg_parser_mu);

    if (st) {
        free(st->buf);
        free(st->out);
        free(st);
    }
    if (raw_s && orig) {
        ((fg_parser_ctx *)raw_s)->parser = orig;
        if (orig->parser_close) orig->parser_close(raw_s);
    }
}

static void fg_wrap_flac_parser(void *raw_s) {
    if (!raw_s) return;
    fg_parser_ctx *s = (fg_parser_ctx *)raw_s;
    if (!s->parser || s->parser->codec_ids[0] != FG_AV_CODEC_ID_FLAC) return;

    fg_flac_stream *st = (fg_flac_stream *)calloc(1, sizeof *st);
    if (!st) return;

    pthread_mutex_lock(&fg_parser_mu);
    if (!fg_orig_flac_parser) {
        fg_orig_flac_parser = s->parser;
        fg_flac_wrapper = *s->parser;
        fg_flac_wrapper.parser_parse = fg_flac_safe_parse;
        fg_flac_wrapper.parser_close = fg_flac_safe_close;
    }
    int slot = -1;
    for (int i = 0; i < FG_FLAC_SLOTS; i++) {
        if (!fg_parser_slots[i].ctx) {
            fg_parser_slots[i].ctx = raw_s;
            fg_parser_slots[i].st = st;
            slot = i;
            break;
        }
    }
    pthread_mutex_unlock(&fg_parser_mu);

    if (slot < 0) {
        free(st);
        return;
    }
    s->parser = &fg_flac_wrapper;
}

/* ---- Part 2: avcodec_decode_audio4 partial-decode & AVERROR_BUG recovery ---- */
typedef int (*fg_decode_audio4_fn)(void *, void *, int *, const void *);
typedef void (*fg_flush_buffers_fn)(void *);

static pthread_mutex_t fg_dec_mu = PTHREAD_MUTEX_INITIALIZER;
static struct { void *avctx; int partial_rem; } fg_dec_slots[FG_FLAC_SLOTS];
static int fg_partial_active;

static void fg_dec_set_partial(void *avctx, int rem) {
    if (!avctx || rem <= 0) return;
    pthread_mutex_lock(&fg_dec_mu);
    int free_i = -1;
    for (int i = 0; i < FG_FLAC_SLOTS; i++) {
        if (fg_dec_slots[i].avctx == avctx) {
            fg_dec_slots[i].partial_rem = rem;
            pthread_mutex_unlock(&fg_dec_mu);
            return;
        }
        if (free_i < 0 && !fg_dec_slots[i].avctx) free_i = i;
    }
    if (free_i >= 0) {
        fg_dec_slots[free_i].avctx = avctx;
        fg_dec_slots[free_i].partial_rem = rem;
        __atomic_fetch_add(&fg_partial_active, 1, __ATOMIC_RELEASE);
    }
    pthread_mutex_unlock(&fg_dec_mu);
}

static int fg_dec_take_partial(void *avctx) {
    if (!avctx || __atomic_load_n(&fg_partial_active, __ATOMIC_ACQUIRE) == 0) return 0;
    int rem = 0;
    pthread_mutex_lock(&fg_dec_mu);
    for (int i = 0; i < FG_FLAC_SLOTS; i++) {
        if (fg_dec_slots[i].avctx == avctx) {
            rem = fg_dec_slots[i].partial_rem;
            fg_dec_slots[i].avctx = NULL;
            fg_dec_slots[i].partial_rem = 0;
            __atomic_fetch_sub(&fg_partial_active, 1, __ATOMIC_RELEASE);
            break;
        }
    }
    pthread_mutex_unlock(&fg_dec_mu);
    return rem;
}

static void fg_dec_forget(void *avctx) {
    (void)fg_dec_take_partial(avctx);
}

/* Guard single-call avcodec_decode_audio4 callers (Engine's 16ffmpegFileReader) against
 * FFmpeg 4.4.3 compat_decode()'s permanent AVERROR_BUG deadlock after any partial decode
 * (0 <= ret < pkt->size). Safe for both refcounted and non-refcounted AVFrames because
 * flush is deferred to the start of the next decode call once the caller has consumed
 * and freed the returned frame. */
static int fg_decode_audio4_guarded(void *avctx, void *frame, int *got_frame,
                                    const void *raw_pkt,
                                    fg_decode_audio4_fn real_decode,
                                    fg_flush_buffers_fn real_flush) {
    /* Restrict recovery strictly to FLAC contexts so MP3/AAC/ALAC/WAV pass straight through. */
    if (!avctx || *(const int *)((const char *)avctx + FG_AVCTX_CODEC_ID_OFF) != FG_AV_CODEC_ID_FLAC)
        return real_decode(avctx, frame, got_frame, raw_pkt);
    const fg_avpacket *pkt = (const fg_avpacket *)raw_pkt;
    int pending_rem = fg_dec_take_partial(avctx);
    if (pending_rem > 0) {
        /* Reset compat_decode_partial_size to 0 (guaranteed size mismatch -> immediate
         * EINVAL before codec execution), then flush ds.in_pkt / bsf->buffer_pkt. */
        uint8_t dummy_bytes[2] = {0, 0};
        fg_avpacket dummy;
        memset(&dummy, 0, sizeof dummy);
        dummy.data = dummy_bytes;
        dummy.size = (pending_rem == 1) ? 2 : 1;
        int dummy_got = 0;
        (void)real_decode(avctx, frame, &dummy_got, &dummy);
        if (real_flush) real_flush(avctx);
    }

    int ret = real_decode(avctx, frame, got_frame, raw_pkt);
    if (ret == FG_AVERROR_BUG && real_flush && pkt && pkt->size > 0) {
        real_flush(avctx);
        if (got_frame) *got_frame = 0;
        ret = real_decode(avctx, frame, got_frame, raw_pkt);
    }

    if (pkt && pkt->size > 0 && ret >= 0 && ret < pkt->size) {
        int got = got_frame ? *got_frame : 0;
        if (!got) {
            /* No frame was returned yet, so frame/to_free is unoccupied: drain remaining
             * sub-frames from this packet now so we don't drop a valid frame inside it. */
            fg_avpacket sub = *pkt;
            int total = ret;
            int guard_iters = 0;
            while (total < pkt->size && guard_iters < 16) {
                sub.data = pkt->data + total;
                sub.size = pkt->size - total;
                int g = 0;
                int rc = real_decode(avctx, frame, &g, &sub);
                guard_iters++;
                if (rc < 0) {
                    if (real_flush) real_flush(avctx);
                    if (got_frame) *got_frame = 0;
                    return rc;
                }
                if (rc == 0) {
                    fg_dec_set_partial(avctx, sub.size);
                    if (got_frame) *got_frame = g;
                    return g ? pkt->size : 0;
                }
                total += rc;
                if (g) {
                    if (got_frame) *got_frame = 1;
                    if (total < pkt->size) fg_dec_set_partial(avctx, pkt->size - total);
                    return total;
                }
            }
            if (total < pkt->size) fg_dec_set_partial(avctx, pkt->size - total);
            return total;
        }
        /* Frame is live for the caller: defer flush until the next call on avctx. */
        fg_dec_set_partial(avctx, pkt->size - ret);
    }
    return ret;
}

#endif /* ENGINEX_FLAC_GUARD_H */
