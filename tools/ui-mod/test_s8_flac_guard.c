/* #336 S8: FLAC parser false-sync resilience and avcodec_decode_audio4 partial-decode guard.
 *
 * Build & run on build host (native AArch64 and armhf):
 *   gcc -std=gnu11 -O2 -Wall -Wextra -Werror -U_FORTIFY_SOURCE tools/ui-mod/test_s8_flac_guard.c -ldl -lpthread -lm -o /tmp/test_s8 && /tmp/test_s8
 */
#define _GNU_SOURCE
#define ENGINE_UI_HOOK_TEST
int hook_test_engine = 1;
#include "engine_ui_hook.c"
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static void build_streaminfo(uint8_t out[34], int min_bs, int max_bs, int max_fs,
                             int sr, int ch, int bps) {
    memset(out, 0, 34);
    out[0] = (uint8_t)((min_bs >> 8) & 0xff);
    out[1] = (uint8_t)(min_bs & 0xff);
    out[2] = (uint8_t)((max_bs >> 8) & 0xff);
    out[3] = (uint8_t)(max_bs & 0xff);
    out[7] = (uint8_t)((max_fs >> 16) & 0xff);
    out[8] = (uint8_t)((max_fs >> 8) & 0xff);
    out[9] = (uint8_t)(max_fs & 0xff);
    out[10] = (uint8_t)((sr >> 12) & 0xff);
    out[11] = (uint8_t)((sr >> 4) & 0xff);
    out[12] = (uint8_t)(((sr & 0x0f) << 4) | (((ch - 1) & 7) << 1) | (((bps - 1) >> 4) & 1));
    out[13] = (uint8_t)(((bps - 1) & 0x0f) << 4);
}

static size_t encode_utf8_num(uint8_t *dst, int64_t v) {
    if (v < 0x80) {
        dst[0] = (uint8_t)v;
        return 1;
    }
    if (v < 0x800) {
        dst[0] = (uint8_t)(0xc0 | (v >> 6));
        dst[1] = (uint8_t)(0x80 | (v & 0x3f));
        return 2;
    }
    if (v < 0x10000) {
        dst[0] = (uint8_t)(0xe0 | (v >> 12));
        dst[1] = (uint8_t)(0x80 | ((v >> 6) & 0x3f));
        dst[2] = (uint8_t)(0x80 | (v & 0x3f));
        return 3;
    }
    dst[0] = (uint8_t)(0xf0 | (v >> 18));
    dst[1] = (uint8_t)(0x80 | ((v >> 12) & 0x3f));
    dst[2] = (uint8_t)(0x80 | ((v >> 6) & 0x3f));
    dst[3] = (uint8_t)(0x80 | (v & 0x3f));
    return 4;
}

/* Build a syntactically valid FLAC frame with CRC-8 and CRC-16. */
static size_t build_flac_frame(uint8_t *dst, int is_var, int blocksize, int sr_code,
                               int ch_code, int bps_code, int64_t num,
                               uint8_t sf_byte, const uint8_t *payload, size_t payload_len) {
    size_t pos = 0;
    dst[pos++] = 0xff;
    dst[pos++] = (uint8_t)(0xf8 | (is_var ? 1 : 0));

    int bs_code = 7;
    if (blocksize == 192) bs_code = 1;
    else if (blocksize == 576) bs_code = 2;
    else if (blocksize == 1152) bs_code = 3;
    else if (blocksize == 2304) bs_code = 4;
    else if (blocksize == 4608) bs_code = 5;
    else if (blocksize == 256) bs_code = 8;
    else if (blocksize == 512) bs_code = 9;
    else if (blocksize == 1024) bs_code = 10;
    else if (blocksize == 2048) bs_code = 11;
    else if (blocksize == 4096) bs_code = 12;
    else if (blocksize <= 256) bs_code = 6;

    dst[pos++] = (uint8_t)((bs_code << 4) | (sr_code & 0x0f));
    dst[pos++] = (uint8_t)((ch_code << 4) | ((bps_code & 7) << 1));
    pos += encode_utf8_num(dst + pos, num);
    if (bs_code == 6) {
        dst[pos++] = (uint8_t)(blocksize - 1);
    } else if (bs_code == 7) {
        dst[pos++] = (uint8_t)((blocksize - 1) >> 8);
        dst[pos++] = (uint8_t)((blocksize - 1) & 0xff);
    }
    uint8_t c8 = fg_crc8_compute(dst, pos);
    dst[pos++] = c8;
    dst[pos++] = sf_byte;
    if (payload && payload_len) {
        memcpy(dst + pos, payload, payload_len);
        pos += payload_len;
    }
    uint16_t crc16 = fg_crc16_compute(dst, pos);
    dst[pos++] = (uint8_t)(crc16 >> 8);
    dst[pos++] = (uint8_t)(crc16 & 0xff);
    return pos;
}

typedef struct fake_avctx {
    uint8_t raw[512];
} fake_avctx;

static void init_fake_avctx(fake_avctx *ctx, const uint8_t *extradata, int extradata_size) {
    memset(ctx, 0, sizeof *ctx);
    *(int *)(ctx->raw + FG_AVCTX_CODEC_ID_OFF) = FG_AV_CODEC_ID_FLAC;
    *(const uint8_t **)(ctx->raw + FG_AVCTX_EXTRADATA_OFF) = extradata;
    *(int *)(ctx->raw + FG_AVCTX_EXTRADATA_SIZE_OFF) = extradata_size;
}

static int orig_parse_calls;
static int fake_orig_parse(void *s, void *avctx, const uint8_t **poutbuf, int *poutbuf_size,
                           const uint8_t *buf, int buf_size) {
    (void)s; (void)avctx;
    orig_parse_calls++;
    *poutbuf = buf;
    *poutbuf_size = buf_size;
    return buf_size;
}

static void fake_orig_close(void *s) { (void)s; }

/* Simulate FFmpeg's av_parser_parse2 outer offset & loop contract around parser_parse. */
static void run_parser_stream(fg_parser_ctx *s, fake_avctx *avctx,
                              const uint8_t *stream, size_t stream_len,
                              size_t chunk_size,
                              int *out_count, int64_t *out_pts, int *out_dur,
                              size_t *out_lens, uint16_t *out_crcs, int max_out) {
    *out_count = 0;
    size_t pos = 0;
    int guard = 0;
    while (pos < stream_len && guard++ < 200000) {
        size_t n = stream_len - pos;
        if (n > chunk_size) n = chunk_size;
        size_t off = 0;
        while (off < n) {
            const uint8_t *out = NULL;
            int out_sz = 0;
            int idx = s->parser->parser_parse(s, avctx, &out, &out_sz, stream + pos + off, (int)(n - off));
            if (out_sz > 0) {
                s->frame_offset = s->next_frame_offset;
                s->next_frame_offset = s->cur_offset + idx;
                assert(*out_count < max_out);
                out_pts[*out_count] = s->pts;
                out_dur[*out_count] = s->duration;
                out_lens[*out_count] = (size_t)out_sz;
                out_crcs[*out_count] = fg_crc16_compute(out, (size_t)out_sz);
                (*out_count)++;
            }
            if (idx < 0) idx = 0;
            s->cur_offset += idx;
            off += (size_t)idx;
        }
        pos += n;
    }
    /* EOF flush passes (buf_size == 0) until no frame emitted. */
    for (int f = 0; f < 64; f++) {
        const uint8_t *out = NULL;
        int out_sz = 0;
        int idx = s->parser->parser_parse(s, avctx, &out, &out_sz, NULL, 0);
        if (out_sz <= 0) break;
        s->frame_offset = s->next_frame_offset;
        s->next_frame_offset = s->cur_offset + idx;
        assert(*out_count < max_out);
        out_pts[*out_count] = s->pts;
        out_dur[*out_count] = s->duration;
        out_lens[*out_count] = (size_t)out_sz;
        out_crcs[*out_count] = fg_crc16_compute(out, (size_t)out_sz);
        (*out_count)++;
    }
}

static void test_header_validation(void) {
    pthread_once(&fg_crc_once, fg_init_crc_tables);
    uint8_t ext[34];
    build_streaminfo(ext, 4096, 4096, 16384, 44100, 2, 16);
    fake_avctx avctx;
    init_fake_avctx(&avctx, ext, sizeof ext);
    fg_flac_streaminfo si;
    fg_parse_streaminfo(&avctx, &si);
    assert(si.valid && si.min_bs == 4096 && si.max_bs == 4096 && si.samplerate == 44100 && si.channels == 2 && si.bps == 16);

    uint8_t frame[64];
    fg_flac_frame_info fi;

    /* Valid fixed-blocksize frame (constant subframe 0x00). */
    uint8_t pay[4] = {0, 0, 0, 0};
    size_t flen = build_flac_frame(frame, 0, 4096, 9, 1, 4, 7, 0x00, pay, sizeof pay);
    assert(fg_parse_frame_header(frame, flen, &si, &fi));
    assert(!fi.is_var_size && fi.blocksize == 4096 && fi.frame_or_sample_num == 7);

    /* Valid short final frame with blocksize = 5 (< 16 samples, RFC 9639 legal!). */
    flen = build_flac_frame(frame, 0, 5, 9, 1, 4, 8, 0x00, pay, sizeof pay);
    assert(fg_parse_frame_header(frame, flen, &si, &fi));
    assert(fi.blocksize == 5 && fi.frame_or_sample_num == 8);

    /* Reject subframe LPC order > blocksize (order 12 > blocksize 5). */
    flen = build_flac_frame(frame, 0, 5, 9, 1, 4, 8, (uint8_t)((32 + 11) << 1), pay, sizeof pay);
    assert(!fg_parse_frame_header(frame, flen, &si, &fi));

    /* Reject invalid subframe zero bit = 1 (#9185). */
    flen = build_flac_frame(frame, 0, 4096, 9, 1, 4, 7, 0x80, pay, sizeof pay);
    assert(!fg_parse_frame_header(frame, flen, &si, &fi));

    /* Reject reserved subframe type 5 (#9185). */
    flen = build_flac_frame(frame, 0, 4096, 9, 1, 4, 7, (uint8_t)(5 << 1), pay, sizeof pay);
    assert(!fg_parse_frame_header(frame, flen, &si, &fi));

    /* Reject variable-blocksize false sync when STREAMINFO declares fixed blocksize (#25766). */
    flen = build_flac_frame(frame, 1, 4096, 9, 1, 4, 7 * 4096, 0x00, pay, sizeof pay);
    assert(!fg_parse_frame_header(frame, flen, &si, &fi));

    /* Reject corrupt CRC-8. */
    flen = build_flac_frame(frame, 0, 4096, 9, 1, 4, 7, 0x00, pay, sizeof pay);
    frame[5] ^= 0x01;
    assert(!fg_parse_frame_header(frame, flen, &si, &fi));

    puts("S8 FLAC header validation (#9185, #25766, short tail frame): PASS");
}

static void test_false_sync_resilience(void) {
    pthread_once(&fg_crc_once, fg_init_crc_tables);
    uint8_t ext[34];
    build_streaminfo(ext, 4096, 4096, 16384, 44100, 2, 16);
    fake_avctx avctx;
    init_fake_avctx(&avctx, ext, sizeof ext);

    /* Craft a 12-frame valid FLAC stream where frame 4 contains 10 false sync headers
     * (exceeding FFmpeg 4.4.3's FLAC_MAX_SEQUENTIAL_HEADERS == 4):
     * - false headers with bad subframe byte (#9185)
     * - false headers with valid CRC-8 & valid subframe byte (#9621)
     * - false headers with variable-blocksize bit (#25766)
     * - false header claiming exact successor frame_num=5 (only CRC-16 rejects it!)
     * Plus frame 11 is a short 5-sample final frame, followed by a 128-byte ID3v1 tag. */
    uint8_t stream[65536];
    size_t expected_len[12];
    size_t spos = 0;

    for (int f = 0; f < 12; f++) {
        int bs = (f == 11) ? 5 : 4096;
        uint8_t pay[4096];
        size_t plen = 4;
        memset(pay, 0x55, plen);
        if (f == 4) {
            plen = 0;
            uint8_t fake[32];
            uint8_t z[4] = {0, 0, 0, 0};
            for (int k = 0; k < 10; k++) {
                memset(pay + plen, 0xff, 64); /* libFLAC Rice k=0 run */
                plen += 64;
                size_t fl = 0;
                if (k < 3) {
                    /* #9185: valid CRC-8, bad subframe byte 0xff */
                    fl = build_flac_frame(fake, 0, 4096, 9, 1, 4, 100 + k, 0xff, z, sizeof z);
                } else if (k < 6) {
                    /* #25766: variable-blocksize false sync */
                    fl = build_flac_frame(fake, 1, 4096, 9, 1, 4, 4096 * (k + 1), 0x00, z, sizeof z);
                } else if (k < 9) {
                    /* #9621: valid CRC-8 + valid subframe byte, random frame num */
                    fl = build_flac_frame(fake, 0, 4096, 9, 1, 4, 50 + k, 0x02, z, sizeof z);
                } else {
                    /* Worst case: valid CRC-8 + valid subframe + exact successor frame_num=5!
                     * Only CRC-16 == 0 over frame 4 distinguishes this from real frame 5. */
                    fl = build_flac_frame(fake, 0, 4096, 9, 1, 4, 5, 0x02, z, sizeof z);
                }
                memcpy(pay + plen, fake, fl - 2);
                plen += fl - 2;
            }
        }
        expected_len[f] = build_flac_frame(stream + spos, 0, bs, 9, 1, 4, f, 0x00, pay, plen);
        spos += expected_len[f];
    }
    /* Append a 128-byte ID3v1 tag at EOF. */
    memset(stream + spos, 0, 128);
    memcpy(stream + spos, "TAGTestTrack", 12);
    spos += 128;

    static const size_t chunks[] = {1, 7, 37, 1024, 16384};
    for (size_t ci = 0; ci < sizeof chunks / sizeof *chunks; ci++) {
        fg_avcodec_parser orig = {
            .codec_ids = {FG_AV_CODEC_ID_FLAC, 0, 0, 0, 0},
            .parser_parse = fake_orig_parse,
            .parser_close = fake_orig_close,
        };
        fg_parser_ctx pctx;
        memset(&pctx, 0, sizeof pctx);
        pctx.parser = &orig;
        pctx.flags = FG_PARSER_FLAG_USE_CODEC_TS;
        fg_wrap_flac_parser(&pctx);
        assert(pctx.parser == &fg_flac_wrapper);

        int n_out = 0;
        int64_t pts[32];
        int dur[32];
        size_t lens[32];
        uint16_t crcs[32];
        run_parser_stream(&pctx, &avctx, stream, spos, chunks[ci],
                          &n_out, pts, dur, lens, crcs, 32);

        assert(n_out == 12);
        for (int f = 0; f < 12; f++) {
            assert(lens[f] == expected_len[f]);
            assert(crcs[f] == 0);
            assert(dur[f] == (f == 11 ? 5 : 4096));
            assert(pts[f] == (int64_t)f * 4096);
        }
        pctx.parser->parser_close(&pctx);
    }
    puts("S8 FLAC parser 10x false-sync + short tail + ID3v1 across 5 chunk sizes: PASS");
}

static void test_edge_streams(void) {
    pthread_once(&fg_crc_once, fg_init_crc_tables);

    /* 1. Single-frame FLAC file + trailing ID3v1. */
    {
        uint8_t ext[34];
        build_streaminfo(ext, 4096, 4096, 1024, 44100, 2, 16);
        fake_avctx avctx;
        init_fake_avctx(&avctx, ext, sizeof ext);
        uint8_t stream[256];
        uint8_t pay[4] = {1, 2, 3, 4};
        size_t flen = build_flac_frame(stream, 0, 1024, 9, 1, 4, 0, 0x00, pay, sizeof pay);
        memset(stream + flen, 0, 128);
        memcpy(stream + flen, "TAGSingle", 9);

        fg_avcodec_parser orig = {.codec_ids = {FG_AV_CODEC_ID_FLAC}, .parser_parse = fake_orig_parse, .parser_close = fake_orig_close};
        fg_parser_ctx pctx;
        memset(&pctx, 0, sizeof pctx);
        pctx.parser = &orig;
        pctx.flags = FG_PARSER_FLAG_USE_CODEC_TS;
        fg_wrap_flac_parser(&pctx);

        int n_out = 0; int64_t pts[8]; int dur[8]; size_t lens[8]; uint16_t crcs[8];
        run_parser_stream(&pctx, &avctx, stream, flen + 128, 64, &n_out, pts, dur, lens, crcs, 8);
        assert(n_out == 1 && lens[0] == flen && crcs[0] == 0 && dur[0] == 1024 && pts[0] == 0);
        pctx.parser->parser_close(&pctx);
    }

    /* 2. Variable-blocksize stream with leading garbage (mid-frame seek resync) + 10-byte frames (#9270). */
    {
        uint8_t ext[34];
        build_streaminfo(ext, 576, 4096, 4096, 48000, 2, 16);
        fake_avctx avctx;
        init_fake_avctx(&avctx, ext, sizeof ext);
        uint8_t stream[2048];
        size_t spos = 73;
        memset(stream, 0xaa, spos); /* mid-frame junk after seek */

        int bss[4] = {1024, 576, 4096, 256};
        int64_t sample = 0;
        size_t elen[4];
        for (int i = 0; i < 4; i++) {
            uint8_t z[1] = {0};
            /* 10-byte minimal frame when num < 128: 4 hdr + 1 utf8 + 1 crc8 + 1 sf + 1 pay + 2 crc16 = 10 */
            elen[i] = build_flac_frame(stream + spos, 1, bss[i], 10, 1, 4, sample, 0x00, z, 1);
            if (i == 0) assert(elen[i] == 10);
            spos += elen[i];
            sample += bss[i];
        }

        fg_avcodec_parser orig = {.codec_ids = {FG_AV_CODEC_ID_FLAC}, .parser_parse = fake_orig_parse, .parser_close = fake_orig_close};
        fg_parser_ctx pctx;
        memset(&pctx, 0, sizeof pctx);
        pctx.parser = &orig;
        pctx.flags = FG_PARSER_FLAG_USE_CODEC_TS;
        fg_wrap_flac_parser(&pctx);

        int n_out = 0; int64_t pts[8]; int dur[8]; size_t lens[8]; uint16_t crcs[8];
        run_parser_stream(&pctx, &avctx, stream, spos, 29, &n_out, pts, dur, lens, crcs, 8);
        assert(n_out == 4);
        int64_t exp_pts = 0;
        for (int i = 0; i < 4; i++) {
            assert(lens[i] == elen[i] && crcs[i] == 0 && dur[i] == bss[i] && pts[i] == exp_pts);
            exp_pts += bss[i];
        }
        pctx.parser->parser_close(&pctx);
    }

    /* 3. Fixed-blocksize stream where STREAMINFO has min_bs < max_bs (last_fixed_bs PTS test). */
    {
        uint8_t ext[34];
        build_streaminfo(ext, 123, 4096, 4096, 44100, 2, 16);
        fake_avctx avctx;
        init_fake_avctx(&avctx, ext, sizeof ext);
        uint8_t stream[512];
        size_t spos = 0;
        uint8_t z[4] = {0, 0, 0, 0};
        spos += build_flac_frame(stream + spos, 0, 4096, 9, 1, 4, 0, 0x00, z, 4);
        spos += build_flac_frame(stream + spos, 0, 4096, 9, 1, 4, 1, 0x00, z, 4);
        spos += build_flac_frame(stream + spos, 0, 123,  9, 1, 4, 2, 0x00, z, 4);

        fg_avcodec_parser orig = {.codec_ids = {FG_AV_CODEC_ID_FLAC}, .parser_parse = fake_orig_parse, .parser_close = fake_orig_close};
        fg_parser_ctx pctx;
        memset(&pctx, 0, sizeof pctx);
        pctx.parser = &orig;
        pctx.flags = FG_PARSER_FLAG_USE_CODEC_TS;
        fg_wrap_flac_parser(&pctx);

        int n_out = 0; int64_t pts[8]; int dur[8]; size_t lens[8]; uint16_t crcs[8];
        run_parser_stream(&pctx, &avctx, stream, spos, 64, &n_out, pts, dur, lens, crcs, 8);
        assert(n_out == 3 && pts[0] == 0 && pts[1] == 4096 && pts[2] == 8192 && dur[2] == 123);
        pctx.parser->parser_close(&pctx);
    }

    /* 4. Complete container frames (Ogg/MKA) pass straight through to original parser. */
    {
        fake_avctx avctx;
        init_fake_avctx(&avctx, NULL, 0);
        fg_avcodec_parser orig = {.codec_ids = {FG_AV_CODEC_ID_FLAC}, .parser_parse = fake_orig_parse, .parser_close = fake_orig_close};
        fg_parser_ctx pctx;
        memset(&pctx, 0, sizeof pctx);
        pctx.parser = &orig;
        pctx.flags = FG_PARSER_FLAG_COMPLETE;
        fg_wrap_flac_parser(&pctx);

        uint8_t pkt[16] = {1};
        const uint8_t *out = NULL;
        int out_sz = 0;
        orig_parse_calls = 0;
        int rc = pctx.parser->parser_parse(&pctx, &avctx, &out, &out_sz, pkt, sizeof pkt);
        assert(rc == (int)sizeof pkt && out_sz == (int)sizeof pkt && orig_parse_calls == 1);
        pctx.parser->parser_close(&pctx);
    }

    puts("S8 FLAC edge streams (1-frame, var-bs, 10-byte, seek resync, Ogg passthrough): PASS");
}

/* ---- Part 2 mock reproducing FFmpeg 4.4.3 compat_decode() partial-decode deadlock ---- */
typedef struct mock_compat_ctx {
    int partial_size;
    int bsf_jammed;
    int flushes;
    int decodes_ok;
} mock_compat_ctx;

static void mock_flush(void *raw) {
    mock_compat_ctx *c = (mock_compat_ctx *)raw;
    c->bsf_jammed = 0;
    c->flushes++;
}

/* Packet byte[0] selects behavior:
 *   0x01: normal single frame (consumes full pkt->size, got=1)
 *   0x02: multi-frame "Junk frame" (consumes 14 bytes < pkt->size, leaves rest -> partial!)
 *   0x03: junk prefix (consumes 10 bytes, got=0) followed by valid frame at offset 10 (byte[10]=0x01)
 *   0x04: codec priming (consumes full pkt->size, got=0)
 * Mirrors FFmpeg 4.4.3 libavcodec/decode.c:compat_decode() lines 791-855:
 *   - mismatch with partial_size -> sets partial_size=0, returns -EINVAL, leaves bsf_jammed=1!
 *   - once bsf_jammed=1 -> every subsequent packet returns AVERROR_BUG until avcodec_flush_buffers! */
static int mock_compat_decode(void *raw_ctx, void *frame, int *got_frame, const void *raw_pkt) {
    (void)frame;
    mock_compat_ctx *c = (mock_compat_ctx *)raw_ctx;
    const fg_avpacket *pkt = (const fg_avpacket *)raw_pkt;
    *got_frame = 0;

    if (c->partial_size > 0 && c->partial_size != pkt->size) {
        c->partial_size = 0;
        c->bsf_jammed = 1; /* simulates leftover unconsumed avci->ds.in_pkt / bsf->buffer_pkt */
        return -22; /* AVERROR(EINVAL) */
    }
    if (c->bsf_jammed) {
        return FG_AVERROR_BUG;
    }

    uint8_t tag = (pkt->size > 0 && pkt->data) ? pkt->data[0] : 0;
    if (tag == 0x02 && pkt->size > 14) {
        *got_frame = 1;
        c->decodes_ok++;
        c->partial_size = pkt->size - 14;
        return 14;
    }
    if (tag == 0x03 && pkt->size > 10) {
        *got_frame = 0;
        c->partial_size = pkt->size - 10;
        return 10;
    }
    if (tag == 0x04) {
        *got_frame = 0;
        c->partial_size = 0;
        return pkt->size;
    }
    *got_frame = 1;
    c->decodes_ok++;
    c->partial_size = 0;
    return pkt->size;
}

static void test_compat_decode_recovery(void) {
    uint8_t multi_buf[463];
    memset(multi_buf, 0, sizeof multi_buf);
    multi_buf[0] = 0x02;

    uint8_t normal_buf[64];
    memset(normal_buf, 0, sizeof normal_buf);
    normal_buf[0] = 0x01;

    fg_avpacket p_multi = {.data = multi_buf, .size = sizeof multi_buf};
    fg_avpacket p_norm  = {.data = normal_buf, .size = sizeof normal_buf};

    /* BEFORE (unguarded, stock Engine single-call loop): permanent AVERROR_BUG silence! */
    mock_compat_ctx raw_ctx;
    memset(&raw_ctx, 0, sizeof raw_ctx);
    int got = 0;
    assert(mock_compat_decode(&raw_ctx, NULL, &got, &p_multi) == 14 && got == 1);
    assert(mock_compat_decode(&raw_ctx, NULL, &got, &p_norm) == -22 && got == 0);
    for (int i = 0; i < 15; i++) {
        assert(mock_compat_decode(&raw_ctx, NULL, &got, &p_norm) == FG_AVERROR_BUG && got == 0);
    }
    assert(raw_ctx.decodes_ok == 1);

    /* AFTER (guarded by fg_decode_audio4_guarded): recovers cleanly, zero AVERROR_BUG! */
    mock_compat_ctx guard_ctx;
    memset(&guard_ctx, 0, sizeof guard_ctx);
    assert(fg_decode_audio4_guarded(&guard_ctx, NULL, &got, &p_multi, mock_compat_decode, mock_flush) == 14 && got == 1);
    for (int i = 0; i < 15; i++) {
        int rc = fg_decode_audio4_guarded(&guard_ctx, NULL, &got, &p_norm, mock_compat_decode, mock_flush);
        assert(rc == (int)sizeof normal_buf && got == 1);
    }
    assert(guard_ctx.decodes_ok == 16 && guard_ctx.flushes == 1);

    /* Junk prefix (got == 0, ret < size) drains remaining frame immediately inside same call. */
    uint8_t prefix_buf[40];
    memset(prefix_buf, 0, sizeof prefix_buf);
    prefix_buf[0] = 0x03;
    prefix_buf[10] = 0x01;
    fg_avpacket p_prefix = {.data = prefix_buf, .size = sizeof prefix_buf};
    int rc = fg_decode_audio4_guarded(&guard_ctx, NULL, &got, &p_prefix, mock_compat_decode, mock_flush);
    assert(rc == (int)sizeof prefix_buf && got == 1 && guard_ctx.flushes == 1);

    /* Normal codec priming (got == 0, ret == pkt->size) never flushes or interferes. */
    uint8_t prime_buf[32];
    memset(prime_buf, 0, sizeof prime_buf);
    prime_buf[0] = 0x04;
    fg_avpacket p_prime = {.data = prime_buf, .size = sizeof prime_buf};
    rc = fg_decode_audio4_guarded(&guard_ctx, NULL, &got, &p_prime, mock_compat_decode, mock_flush);
    assert(rc == (int)sizeof prime_buf && got == 0 && guard_ctx.flushes == 1);

    /* Unexpected external AVERROR_BUG retries once after flush. */
    guard_ctx.bsf_jammed = 1;
    rc = fg_decode_audio4_guarded(&guard_ctx, NULL, &got, &p_norm, mock_compat_decode, mock_flush);
    assert(rc == (int)sizeof normal_buf && got == 1 && guard_ctx.flushes == 2);

    fg_dec_forget(&guard_ctx);
    assert(__atomic_load_n(&fg_partial_active, __ATOMIC_ACQUIRE) == 0);
    puts("S8 compat_decode partial-decode & AVERROR_BUG recovery: PASS");
}

int main(void) {
    test_header_validation();
    test_false_sync_resilience();
    test_edge_streams();
    test_compat_decode_recovery();
    puts("ALL S8 FLAC GUARD TESTS PASSED");
    return 0;
}
