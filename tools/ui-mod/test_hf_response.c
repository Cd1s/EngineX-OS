/* Issue #172: stepped-sine measurements of the actual EngineX wrappers.
 * Vendor processing is an identity callback (Flanger: a known 0.7 scalar).
 * This measures EngineX's contribution, not the complete device response.
 * arm-linux-gnueabihf-gcc -std=c11 -O2 -Wall -Wextra -Werror \
 *   -Wno-unused-function tools/ui-mod/test_hf_response.c -ldl -pthread -lm -o test_hf_response
 * ./test_hf_response
 */
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <math.h>
#include <stdlib.h>
#include <sys/mman.h>

enum { RATE = 96000, BLOCK = 128 };
static const double pi = 3.14159265358979323846;
static unsigned failures;
static float wave[RATE];
static double sine[RATE], cosine[RATE];

#define CHECK(c) do { if (!(c)) { fprintf(stderr, "%d: %s\n", __LINE__, #c); failures++; } } while (0)

static uint32_t copy_master(void *host, void *ctx, int ic, const float *const *in,
                            int oc, float *const *out, int frames)
{
    (void)host; (void)ctx;
    CHECK(ic == oc);
    for (int ch = 0; ch < oc; ch++)
        memcpy(out[ch], in[ch], (size_t)frames * sizeof(float));
    return 0x172;
}

static void copy_common(void *self, float *const *in, float *const *out, uint32_t frames)
{
    (void)self;
    for (int ch = 0; ch < 2; ch++)
        memcpy(out[ch], in[ch], (size_t)frames * sizeof(float));
}

static void scalar_flanger(void *self, float *const *in, float *const *out, uint32_t frames)
{
    (void)self; (void)out;
    for (int ch = 0; ch < 2; ch++)
        for (unsigned i = 0; i < frames; i++) in[ch][i] *= 0.7f;
}

enum { MASTER_OFF, COMMON, SWEEP, FLANGER, LIMIT_QUIET, LIMIT_FULL,
       LIMIT_OVER, AUTO_GAIN, ECHO_ARMED, CASES };
static const char *const names[] = {"master_off", "common_identity", "sweep_guard",
    "flanger_scalar_makeup", "limiter_0.1", "limiter_1.0", "limiter_2.0",
    "auto_gain_0.1", "echo_armed_music"};

static double measure(unsigned mode, unsigned hz)
{
    static int common, flanger;
    memset(sc_dsp_state(), 0, sizeof *sc_dsp_state());
    memset(master_lim, 0, sizeof master_lim);
    memset(master_gain, 0, sizeof master_gain);
    memset(master_echo, 0, sizeof master_echo);
    memset(master_pfx, 0, sizeof master_pfx);
    four_deck_pad_on = 0;
    fx_selector_supported = fx_selector_master_bypass = pioneer_fx_mode = 0;
    echo_enabled = mode == ECHO_ARMED;
    lim_enabled = mode >= LIMIT_QUIET && mode <= LIMIT_OVER;
    gain_enabled = mode == AUTO_GAIN;
    dsp_originals[DSP_MASTER] = (void *)copy_master;
    dsp_originals[DSP_COMMON] = (void *)copy_common;
    dsp_originals[DSP_FLANGER] = (void *)scalar_flanger;
    const float amplitude = mode == LIMIT_FULL ? 1.0f : mode == LIMIT_OVER ? 2.0f : 0.1f;
    for (unsigned i = 0; i < RATE; i++) {
        double phase = 2.0 * pi * (double)((i * hz) % RATE) / RATE;
        sine[i] = sin(phase); cosine[i] = cos(phase);
        wave[i] = amplitude * sine[i];
    }
    /* Whole seconds keep all stimulus frequencies coherent with the DFT.
     * Auto-Gain's 2 s attack needs more settling than the 100 ms makeup. */
    unsigned settle = mode == AUTO_GAIN ? 20 * RATE : 2 * RATE;
    double re = 0, im = 0, input_energy = 0, output_energy = 0, peak = 0;
    unsigned changed = 0;
    for (unsigned frame = 0; frame < settle + RATE; frame += BLOCK) {
        float l[BLOCK], r[BLOCK], ol[BLOCK], or_[BLOCK];
        for (unsigned i = 0; i < BLOCK; i++) l[i] = r[i] = wave[(frame + i) % RATE];
        const float *in[] = {l, r};
        float *input[] = {l, r}, *out[] = {ol, or_};
        if (mode == COMMON) {
            if (frame == 0) sc_dsp_fx_mark_transition(&common, SC_DSP_FX_TRANSITION_SWITCH);
            hook_fx_common_chain(&common, input, out, BLOCK);
        } else if (mode == SWEEP || mode == FLANGER) {
            if (mode == SWEEP) sc_dsp_sweep_fx_post_process(NULL, input, BLOCK);
            else hook_flanger(&flanger, input, NULL, BLOCK);
            memcpy(ol, l, sizeof ol); memcpy(or_, r, sizeof or_);
        } else {
            CHECK(hook_master_output(NULL, NULL, 2, in, 2, out, BLOCK) == 0x172);
        }
        if (frame < settle) continue;
        for (unsigned i = 0; i < BLOCK; i++) {
            unsigned p = (frame + i) % RATE;
            double y = ol[i], x = wave[p];
            CHECK(isfinite(y) && ol[i] == or_[i]);
            changed += memcmp(&ol[i], &wave[p], sizeof(float)) != 0;
            re += y * sine[p]; im += y * cosine[p];
            input_energy += x * x; output_energy += y * y;
            if (fabs(y) > peak) peak = fabs(y);
        }
    }
    double fundamental = 20 * log10(2 * hypot(re, im) / (RATE * amplitude));
    double rms = 10 * log10(output_energy / input_energy);
    printf("%s,%u,%.1f,%.6f,%.6f,%.6f,%u\n",
           names[mode], hz, amplitude, fundamental, rms, peak, changed);
    if (mode == MASTER_OFF || mode == COMMON || mode == SWEEP ||
        mode == LIMIT_QUIET || mode == ECHO_ARMED) CHECK(changed == 0);
    if (mode == FLANGER) CHECK(fabs(fundamental) < 0.002);
    if (mode == AUTO_GAIN) CHECK(fabs(fundamental - 20 * log10(SC_GAIN_MAX)) < 0.002);
    if (mode >= LIMIT_QUIET && mode <= LIMIT_OVER) CHECK(peak <= SC_LIM_CEIL + 1e-6);
    return fundamental;
}

#if defined(__arm__)
/* Optional stock fixture: execute the unmodified, self-contained interpolation
 * kernels from the exact 5.3.0 armhf Engine. No Engine process/device is started.
 * The caller must verify the full fixture SHA-256 documented in the report.
 * These three functions have no external references or PC-relative literals.
 * Their addresses are in the first ELF PT_LOAD, whose file offset equals VA.
 * RTTI/vtable identifies Linear, Hermitian and HermitianClipped respectively.
 */
static void measure_stock(const char *path)
{
    static const unsigned address[] = {0x17b5818, 0x17b5850, 0x17b58d4};
    static const unsigned length[] = {0x38, 0x84, 0xa4};
    static const uint32_t expected[] = {0xf8d0c7ea, 0x11e96d45, 0xfacef112};
    static const char *const name[] = {"linear", "hermitian", "hermitian_clipped"};
    static const unsigned rates[] = {44100, 48000, 96000};
    static const unsigned hz[] = {1000, 16000, 18000, 20000};
    FILE *file = fopen(path, "rb");
    if (!file) { perror(path); failures++; return; }
    unsigned char ident[20];
    if (fread(ident, 1, sizeof ident, file) != sizeof ident ||
        memcmp(ident, "\177ELF\1\1\1", 7) || ident[18] != 40) {
        fprintf(stderr, "expected stock ARM ELF32 fixture\n"); failures++; fclose(file); return;
    }
    puts("stock_kernel,input_rate,hz,fundamental_db,rms_db");
    for (unsigned k = 0; k < 3; k++) {
        unsigned char *code = mmap(NULL, 4096, PROT_READ | PROT_WRITE,
                                  MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
        if (code == MAP_FAILED) { perror("mmap"); failures++; break; }
        if (fseek(file, address[k], SEEK_SET) || fread(code, 1, length[k], file) != length[k]) {
            fprintf(stderr, "cannot load stock kernel\n"); failures++; munmap(code, 4096); break;
        }
        uint32_t hash = 2166136261u;
        for (unsigned i = 0; i < length[k]; i++) hash = (hash ^ code[i]) * 16777619u;
        if (hash != expected[k] || mprotect(code, 4096, PROT_READ | PROT_EXEC)) {
            fprintf(stderr, "stock kernel mismatch or execution unavailable\n");
            failures++; munmap(code, 4096); break;
        }
        __builtin___clear_cache((char *)code, (char *)code + length[k]);
        typedef float (*kernel_fn)(void *, float **, unsigned, float);
        kernel_fn kernel = (kernel_fn)(void *)code;
        for (unsigned sr = 0; sr < 3; sr++) for (unsigned f = 0; f < 4; f++) {
            double re = 0, im = 0, energy = 0;
            for (unsigned i = 0; i < RATE; i++) {
                unsigned long long position = (unsigned long long)i * rates[sr];
                unsigned index = position / RATE;
                float fraction = (float)(position % RATE) / RATE;
                float samples[4], *buffer = samples;
                for (int n = 0; n < 4; n++)
                    samples[n] = 0.1 * sin(2 * pi * hz[f] * ((double)index + n - 1) / rates[sr]);
                double y = kernel(NULL, &buffer, 1, fraction);
                double phase = 2 * pi * (double)((i * hz[f]) % RATE) / RATE;
                CHECK(isfinite(y));
                re += y * sin(phase); im += y * cos(phase); energy += y * y;
            }
            double db = 20 * log10(2 * hypot(re, im) / (RATE * 0.1));
            printf("%s,%u,%u,%.6f,%.6f\n", name[k], rates[sr], hz[f], db,
                   10 * log10(energy / (RATE * 0.005)));
            if (rates[sr] == RATE) CHECK(fabs(db) < 0.00001);
        }
        munmap(code, 4096);
    }
    fclose(file);
}
#endif

int main(int argc, char **argv)
{
    static const unsigned frequencies[] = {20, 100, 1000, 10000, 16000, 18000, 20000};
    puts("case,hz,input_peak,fundamental_db,rms_db,output_peak,changed_samples");
    for (unsigned mode = 0; mode < CASES; mode++) {
        double reference = 0;
        for (unsigned f = 0; f < sizeof frequencies / sizeof *frequencies; f++) {
            double gain = measure(mode, frequencies[f]);
            if (frequencies[f] == 1000) reference = gain;
            /* A limiter is nonlinear; allow sampled-peak/alias differences,
             * but reject anything close to the reported 6 dB HF loss. */
            if (frequencies[f] >= 16000) CHECK(fabs(gain - reference) < 0.25);
        }
    }
#if defined(__arm__)
    if (argc == 2) measure_stock(argv[1]);
#else
    (void)argc; (void)argv;
#endif
    fprintf(stderr, "test_hf_response: %s (%u failures)\n", failures ? "FAIL" : "PASS", failures);
    return failures ? EXIT_FAILURE : EXIT_SUCCESS;
}
