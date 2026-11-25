/*
 * enginex-aacenc: the live stream's AAC encoder for the SHOUTcast mode.
 *
 *   enginex-aacenc <kbps>          s16le stereo 48 kHz PCM on stdin, AAC-LC in ADTS frames on stdout
 *   enginex-aacenc --check         exit 0 when the stock encoder opens, else print why and exit 3
 *
 * It ships no codec: every stock base image (Gen-1, Gen-2 armhf, AZ04) carries FFmpeg 4.4.3's
 * libavcodec.so.58 (LGPL 2.1, Engine's own copy in /usr/lib) with the native "aac" encoder built
 * in. The library is opened with dlopen at run time, so this program links only libc and libdl
 * and starts on a base without it, saying why instead of failing to load.
 * FFmpeg's headers (4.4.3, the version on the decks) give the struct layouts; build.sh fetches them.
 *
 * stream-sync starts it only while a SHOUTcast push is active, as its child: it inherits stream-sync's
 * CPU (the background core), SCHED_IDLE, nice 19 and idle I/O, and it never touches the audio path.
 * It reads one encoder frame (1024 samples) at a time and writes each AAC frame as soon as it exists.
 */
#include <dlfcn.h>
#include <errno.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include <libavcodec/avcodec.h>
#include <libavutil/channel_layout.h>
#include <libavutil/frame.h>
#include <libavutil/opt.h>

#define RATE 48000
#define CHANNELS 2

static struct {
	unsigned (*avcodec_version)(void);
	unsigned (*avutil_version)(void);
	const AVCodec *(*find_encoder_by_name)(const char *);
	AVCodecContext *(*alloc_context3)(const AVCodec *);
	int (*open2)(AVCodecContext *, const AVCodec *, AVDictionary **);
	int (*send_frame)(AVCodecContext *, const AVFrame *);
	int (*receive_packet)(AVCodecContext *, AVPacket *);
	AVPacket *(*packet_alloc)(void);
	void (*packet_unref)(AVPacket *);
	AVFrame *(*frame_alloc)(void);
	int (*frame_get_buffer)(AVFrame *, int);
	int (*frame_make_writable)(AVFrame *);
	int (*opt_set)(void *, const char *, const char *, int);
} av;

static int load(void)
{
	/* Engine's copy; the soname pins the ABI (major 58 = FFmpeg 4.x). */
	void *codec = dlopen("libavcodec.so.58", RTLD_NOW | RTLD_LOCAL);
	if (!codec) {
		fprintf(stderr, "enginex-aacenc: no stock AAC encoder: %s\n", dlerror());
		return -1;
	}
	void *util = dlopen("libavutil.so.56", RTLD_NOW | RTLD_LOCAL);
	if (!util) {
		fprintf(stderr, "enginex-aacenc: no stock AAC encoder: %s\n", dlerror());
		return -1;
	}
#define SYM(lib, field, name)                                                       \
	do {                                                                            \
		*(void **)&av.field = dlsym(lib, name);                                     \
		if (!av.field) {                                                            \
			fprintf(stderr, "enginex-aacenc: stock library lacks %s\n", name);      \
			return -1;                                                              \
		}                                                                           \
	} while (0)
	SYM(codec, avcodec_version, "avcodec_version");
	SYM(util, avutil_version, "avutil_version");
	SYM(codec, find_encoder_by_name, "avcodec_find_encoder_by_name");
	SYM(codec, alloc_context3, "avcodec_alloc_context3");
	SYM(codec, open2, "avcodec_open2");
	SYM(codec, send_frame, "avcodec_send_frame");
	SYM(codec, receive_packet, "avcodec_receive_packet");
	SYM(codec, packet_alloc, "av_packet_alloc");
	SYM(codec, packet_unref, "av_packet_unref");
	SYM(util, frame_alloc, "av_frame_alloc");
	SYM(util, frame_get_buffer, "av_frame_get_buffer");
	SYM(util, frame_make_writable, "av_frame_make_writable");
	SYM(util, opt_set, "av_opt_set");
#undef SYM
	/* The struct layouts below come from the 4.4 headers: refuse any other major version. */
	if (av.avcodec_version() >> 16 != LIBAVCODEC_VERSION_MAJOR || av.avutil_version() >> 16 != LIBAVUTIL_VERSION_MAJOR) {
		fprintf(stderr, "enginex-aacenc: stock libavcodec %u / libavutil %u, built for %d / %d\n",
			av.avcodec_version() >> 16, av.avutil_version() >> 16, LIBAVCODEC_VERSION_MAJOR, LIBAVUTIL_VERSION_MAJOR);
		return -1;
	}
	return 0;
}

static AVCodecContext *open_encoder(long kbps)
{
	const AVCodec *codec = av.find_encoder_by_name("aac");
	if (!codec) {
		fprintf(stderr, "enginex-aacenc: the stock libavcodec has no AAC encoder\n");
		return NULL;
	}
	AVCodecContext *c = av.alloc_context3(codec);
	if (!c)
		return NULL;
	c->bit_rate = kbps * 1000;
	c->sample_rate = RATE;
	c->channels = CHANNELS;
	c->channel_layout = AV_CH_LAYOUT_STEREO;
	c->sample_fmt = AV_SAMPLE_FMT_FLTP;
	c->profile = FF_PROFILE_AAC_LOW;
	/* "fast" is the cheapest of the encoder's search methods; ENGINEX_AAC_CODER exists for measurements. */
	const char *coder = getenv("ENGINEX_AAC_CODER");
	if (av.opt_set(c->priv_data, "aac_coder", coder && *coder ? coder : "fast", 0) < 0) {
		fprintf(stderr, "enginex-aacenc: unknown aac_coder\n");
		return NULL;
	}
	int err = av.open2(c, codec, NULL);
	if (err < 0) {
		fprintf(stderr, "enginex-aacenc: cannot open the AAC encoder (error %d)\n", err);
		return NULL;
	}
	if (c->frame_size <= 0 || c->frame_size > 4096) {
		fprintf(stderr, "enginex-aacenc: unexpected encoder frame size %d\n", c->frame_size);
		return NULL;
	}
	return c;
}

/* writeall writes the whole buffer; 0 on success. */
static int writeall(const uint8_t *p, size_t n)
{
	while (n > 0) {
		ssize_t w = write(1, p, n);
		if (w < 0) {
			if (errno == EINTR)
				continue;
			return -1;
		}
		p += w;
		n -= (size_t)w;
	}
	return 0;
}

/* readfull fills the buffer; returns the bytes read (short only at end of input). */
static size_t readfull(uint8_t *p, size_t n)
{
	size_t got = 0;
	while (got < n) {
		ssize_t r = read(0, p + got, n - got);
		if (r < 0) {
			if (errno == EINTR)
				continue;
			break;
		}
		if (r == 0)
			break;
		got += (size_t)r;
	}
	return got;
}

/* adts writes the 7-byte ADTS header (MPEG-4, AAC LC, 48 kHz, stereo, no CRC) for a payload of n bytes. */
static void adts(uint8_t h[7], int n)
{
	const int len = n + 7, profile = 1 /* LC - 1 */, rate = 3 /* 48000 */, ch = CHANNELS;
	h[0] = 0xff;
	h[1] = 0xf1;
	h[2] = (uint8_t)((profile << 6) | (rate << 2) | (ch >> 2));
	h[3] = (uint8_t)(((ch & 3) << 6) | ((len >> 11) & 3));
	h[4] = (uint8_t)((len >> 3) & 0xff);
	h[5] = (uint8_t)(((len & 7) << 5) | 0x1f);
	h[6] = 0xfc;
}

/* drain writes every packet the encoder has ready; -1 when stdout is gone. */
static int drain(AVCodecContext *c, AVPacket *pkt)
{
	for (;;) {
		int err = av.receive_packet(c, pkt);
		if (err == AVERROR(EAGAIN) || err == AVERROR_EOF)
			return 0;
		if (err < 0) {
			fprintf(stderr, "enginex-aacenc: encode error %d\n", err);
			return -1;
		}
		uint8_t h[7];
		if (pkt->size > 8191 - 7) { /* cannot happen at <= 320 kbps; ADTS holds 13 bits */
			av.packet_unref(pkt);
			continue;
		}
		adts(h, pkt->size);
		int bad = writeall(h, sizeof h) || writeall(pkt->data, (size_t)pkt->size);
		av.packet_unref(pkt);
		if (bad)
			return -1;
	}
}

int main(int argc, char **argv)
{
	signal(SIGPIPE, SIG_DFL);
	if (argc != 2) {
		fprintf(stderr, "usage: enginex-aacenc <kbps 32..320> | --check\n");
		return 2;
	}
	const int check = strcmp(argv[1], "--check") == 0;
	char *end = NULL;
	long kbps = check ? 192 : strtol(argv[1], &end, 10);
	if (!check && (*argv[1] == 0 || *end != 0 || kbps < 32 || kbps > 320)) {
		fprintf(stderr, "enginex-aacenc: bitrate must be 32..320 kbps\n");
		return 2;
	}
	if (load() != 0)
		return 3;
	AVCodecContext *c = open_encoder(kbps);
	if (!c)
		return 3;
	if (check)
		return 0;

	AVFrame *f = av.frame_alloc();
	AVPacket *pkt = av.packet_alloc();
	if (!f || !pkt)
		return 4;
	f->nb_samples = c->frame_size;
	f->format = c->sample_fmt;
	f->channel_layout = c->channel_layout;
	f->channels = CHANNELS;
	f->sample_rate = RATE;
	if (av.frame_get_buffer(f, 0) < 0)
		return 4;

	const size_t want = (size_t)c->frame_size * CHANNELS * 2;
	uint8_t *in = malloc(want);
	if (!in)
		return 4;
	int64_t pts = 0;
	for (;;) {
		size_t got = readfull(in, want);
		size_t frames = got / (CHANNELS * 2);
		if (frames == 0)
			break;
		if (av.frame_make_writable(f) < 0)
			return 4;
		float *l = (float *)f->data[0], *r = (float *)f->data[1];
		for (size_t i = 0; i < (size_t)c->frame_size; i++) {
			int16_t a = 0, b = 0;
			if (i < frames) {
				a = (int16_t)(in[4 * i] | in[4 * i + 1] << 8);
				b = (int16_t)(in[4 * i + 2] | in[4 * i + 3] << 8);
			}
			l[i] = a * (1.0f / 32768.0f);
			r[i] = b * (1.0f / 32768.0f);
		}
		f->pts = pts;
		pts += c->frame_size;
		if (av.send_frame(c, f) < 0 || drain(c, pkt) < 0)
			return 1;
		if (got < want)
			break;
	}
	/* End of input: flush the encoder's delayed frames. */
	if (av.send_frame(c, NULL) < 0 || drain(c, pkt) < 0)
		return 1;
	return 0;
}
