// Compile and run with ARMv7 gcc / qemu-arm on IN.
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>
#include <sqlite3.h>
#include <sys/wait.h>
#include <math.h>
#include <float.h>

static unsigned dsp_calls, dsp_seen_generation, dsp_seen_sampler_generation;
static void *dsp_wanted_owner;
static const void *dsp_wanted_event;

static unsigned fx_generation(const void *owner) {
    struct sc_dsp_fx_slot *f=sc_dsp_fx_lookup(owner,0);
    return f?(__atomic_load_n(&f->event,__ATOMIC_ACQUIRE)>>3):0;
}
typedef void (*test_channel_fn)(void *,uintptr_t,float *const *,uintptr_t,float *const *,uint32_t,
                              double,double,double,double,double,double,double,double);
static test_channel_fn nested_channel;
struct channel_case { float value; void *gate; struct channel_case *child; unsigned char native_padding[0x290]; };
static dsp_process_fn channel_gate;
static void mock_channel_process(void *self,uintptr_t a1,float *const *in,uintptr_t a3,
                                 float *const *out,uint32_t frames,
                                 double p0,double p1,double p2,double p3,double p4,double p5,double p6,double p7) {
    assert(a1==0x11223344&&a3==0x55667788);
    assert(p0==10.5&&p1==11.5&&p2==12.5&&p3==13.5&&p4==14.5&&p5==15.5&&p6==16.5&&p7==17.5);
    assert(dsp_current_channel&&dsp_current_channel->owner==(uintptr_t)self);
    struct channel_case *c=self;
    if(c->child) {
        nested_channel(c->child,a1,in,a3,out,frames,p0,p1,p2,p3,p4,p5,p6,p7);
        assert(dsp_current_channel&&dsp_current_channel->owner==(uintptr_t)self);
    }
    if(c->gate&&frames)channel_gate(c->gate,in,out,frames);
    for(unsigned ch=0;ch<2;ch++)for(uint32_t i=0;i<frames;i++)out[ch][i]=c->value;
}
static void call_channel(test_channel_fn fn,void *owner,float *const *in,float *const *out,uint32_t frames) {
    fn(owner,0x11223344,in,0x55667788,out,frames,10.5,11.5,12.5,13.5,14.5,15.5,16.5,17.5);
    assert(!dsp_current_channel);
}

struct channel_worker_args { test_channel_fn fn; struct channel_case *owner; };
static void *channel_worker(void *arg) {
    struct channel_worker_args *a=arg;
    float l[16]={0},r[16]={0},*p[2]={l,r};
    for(unsigned i=0;i<1000;i++) {
        call_channel(a->fn,a->owner,p,p,16);
        assert(l[15]==a->owner->value&&r[15]==a->owner->value);
    }
    return NULL;
}

static void mock_dsp_event(void *owner, const void *event) {
    assert(owner == dsp_wanted_owner && event == dsp_wanted_event);
    __atomic_add_fetch(&dsp_calls, 1u, __ATOMIC_RELAXED);
    __atomic_store_n(&dsp_seen_generation,
        fx_generation(owner), __ATOMIC_RELAXED);
    __atomic_store_n(&dsp_seen_sampler_generation,
        __atomic_load_n(&sc_dsp_state()->sampler_generation, __ATOMIC_ACQUIRE), __ATOMIC_RELAXED);
}

struct sampler_thread_args { char *image; void *handler; const void *event; };
static void *sampler_thread(void *arg) {
    const struct sampler_thread_args *a = arg;
    for (unsigned i = 0; i < 1500; i++) {
        unsigned slot = DSP_SAMPLER_IMMEDIATE + i % 3;
        ((dsp_event_fn)(a->image + dsp_hooks[slot].offset))(a->handler, a->event);
    }
    return NULL;
}

static const float sweep_samples[] = {
    NAN, INFINITY, -INFINITY, FLT_MAX, -FLT_MAX, 9.0f, -9.0f,
    8.0f, -8.0f, 0.25f, -0.5f, 0.0f, -0.0f
};
static const float sweep_expected[] = {
    0.0f, 0.0f, 0.0f, 8.0f, -8.0f, 8.0f, -8.0f,
    8.0f, -8.0f, 0.25f, -0.5f, 0.0f, -0.0f
};
static unsigned sweep_calls;
static uintptr_t sweep_owner_adjustment;
static uint32_t sweep_wanted_frames;
static void mock_sweep_process(void *self, float *const *in, float *const *out, uint32_t frames) {
    assert((uintptr_t)self == (uintptr_t)dsp_wanted_owner + sweep_owner_adjustment);
    assert(in == out && frames == sweep_wanted_frames);
    sweep_calls++;
    for (unsigned ch = 0; ch < 2; ch++)
        for (uint32_t i = 0; i < frames; i++)
            in[ch][i] = sweep_samples[(i + ch) % (sizeof sweep_samples / sizeof *sweep_samples)];
}

static unsigned gate_zero_calls, gate_nonzero_calls;
static float gate_seen_phase;
static void mock_gate_nonzero(void *self, float *const *in, float *const *out, uint32_t frames) {
    assert(frames > 0 && in == out); // displaced mov r2,r1 must still execute
    memcpy(&gate_seen_phase, (char *)self + SC_DSP_AUTOGATE_PHASE_OFFSET, sizeof gate_seen_phase);
    gate_nonzero_calls++;
}
static void mock_gate_zero(void *self, float *const *in, float *const *out, uint32_t frames) {
    (void)self;
    assert(frames == 0 && in == out);
    gate_zero_calls++;
}

static int common_bad_output;
static void mock_common_process(void *self, float *const *in, float *const *out, uint32_t frames) {
    assert(self == dsp_wanted_owner && in && out);
    for (int ch = 0; ch < 2; ch++)
        for (uint32_t i = 0; i < frames; i++)
            out[ch][i] = common_bad_output == 2 ? FLT_MAX :
                (common_bad_output ? (i & 1 ? INFINITY : NAN) : 1.0f);
}

// Execute the actual ARM entry patches and displaced prologues, followed by
// small ABI-correct native tails into mocks. No vendor DSP needs to be emulated.
static void test_dsp_hooks(void) {
    size_t page = (size_t)sysconf(_SC_PAGESIZE), image_size = 0x1600000;
    char *image = mmap(NULL, image_size, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(image != MAP_FAILED);
    const uint32_t dispatcher[] = {
        0xe1c021d0, 0xe590c00c, 0xe52db004, 0xe28db000, 0xe3130001, 0xe08c00c3,
        0x179c30c3, 0x17932002, 0xe28bd000, 0xe49db004, 0xe12fff12
    };
    for (unsigned i = 0; i < DSP_AUTOGATE; i++)
        memcpy(image + dsp_hooks[i].offset, dispatcher, sizeof dispatcher);
    memcpy(image + dsp_hooks[DSP_AUTOGATE].offset, dsp_autogate_prefix, sizeof dsp_autogate_prefix);
    uint32_t jump_nonzero[] = {0xe51ff004, (uint32_t)(uintptr_t)mock_gate_nonzero};
    uint32_t jump_zero[] = {0xe51ff004, (uint32_t)(uintptr_t)mock_gate_zero};
    memcpy(image + dsp_hooks[DSP_AUTOGATE].offset + 12, jump_nonzero, sizeof jump_nonzero);
    memcpy(image + dsp_hooks[DSP_AUTOGATE].offset + 0xe4, jump_zero, sizeof jump_zero);
    // Undo the real common-chain strd prologue before tail-calling a C mock.
    const uint32_t common[] = {0xe16d42f4, 0xe1cd60f8, 0xe1cd40d0, 0xe1cd60d8,
                              0xe28dd024, 0xe51ff004, (uint32_t)(uintptr_t)mock_common_process};
    memcpy(image + dsp_hooks[DSP_COMMON].offset, common, sizeof common);
    // Actual SweepFx prefix and native owner adjustment, then restore fp before
    // entering a C mock. r2 must become the original input pointer, not output.
    for (unsigned i = DSP_DUB_ECHO; i <= DSP_WASH; i++) {
        const uint32_t sweep[] = {0xe52db004, 0xe1a02001, 0xe28db000,
            i == DSP_DUB_ECHO ? 0xe2800060 : 0xe2800070,
            0xe28bd000, 0xe49db004, 0xe51ff004, (uint32_t)(uintptr_t)mock_sweep_process};
        memcpy(image + dsp_hooks[i].offset, sweep, sizeof sweep);
    }
    const uint32_t channel_code[] = {0xe16d42f0,0xe1a05000,0xe5900288,0xe1a00005,0xe1cd40d0,0xe28dd020,
                                     0xe51ff004,(uint32_t)(uintptr_t)mock_channel_process};
    memcpy(image+dsp_hooks[DSP_CHANNEL].offset,channel_code,sizeof channel_code);
    // Every checked byte, including byte 12 of a dispatcher, gates the WHOLE group.
    for (unsigned i = 0; i < DSP_HOOK_COUNT; i++) {
        size_t size;
        dsp_expected(i, &size);
        image[dsp_hooks[i].offset + size - 1] ^= 1;
        assert(!install_dsp_hooks((uintptr_t)image, page));
        image[dsp_hooks[i].offset + size - 1] ^= 1;
        for (unsigned j = 0; j < DSP_HOOK_COUNT; j++) {
            const void *expected = dsp_expected(j, &size);
            assert(!memcmp(image + dsp_hooks[j].offset, expected, size));
            assert(!dsp_originals[j]);
        }
    }
    assert(!mprotect(image, image_size, PROT_READ | PROT_EXEC));
    __builtin___clear_cache(image, image + image_size);
    assert(install_dsp_hooks((uintptr_t)image, page));
    assert(!install_dsp_hooks((uintptr_t)image, page)); // repeat install leaves original stubs intact
    for (unsigned i = 0; i < DSP_HOOK_COUNT; i++) {
        uint32_t *at = (void *)(image + dsp_hooks[i].offset);
        assert(at[0] == 0xe51ff004 && at[1] == (uint32_t)(uintptr_t)dsp_hooks[i].hook);
        if (i == DSP_AUTOGATE) assert(at[2] == 0xe320f000);
    }
    memset(sc_dsp_state(), 0, sizeof *sc_dsp_state());
    uint32_t owner[8] = {0}, handler[6] = {0};
    unsigned char raw_event[0x40] = {0};
    // Deliberately unaligned payload; guards must use memcpy rather than casts.
    void *event = raw_event + 1;
    handler[3] = (uintptr_t)owner;
    handler[4] = (uintptr_t)mock_dsp_event;
    dsp_wanted_owner = owner;
    dsp_wanted_event = event;
    for (unsigned i = 0; i < DSP_MIX; i++) {
        unsigned before = dsp_calls, gen = fx_generation(dsp_wanted_owner);
        ((dsp_event_fn)(image + dsp_hooks[i].offset))(handler, event);
        assert(dsp_calls == before + 1 && dsp_seen_generation == gen + 1);
        assert(sc_dsp_fx_lookup(dsp_wanted_owner,1)->remaining == 0); // UI never edits audio progress
    }
    // Preserve the dispatcher's virtual target resolution and signed this adjustment.
    uint32_t vtable[] = {(uintptr_t)mock_dsp_event};
    owner[0] = (uintptr_t)vtable; handler[4] = 0; handler[5] = 1;
    ((dsp_event_fn)(image + dsp_hooks[DSP_SWITCH].offset))(handler, event);
    handler[3] = (uintptr_t)(owner + 1); handler[4] = (uintptr_t)mock_dsp_event; handler[5] = (uint32_t)-8;
    ((dsp_event_fn)(image + dsp_hooks[DSP_SWITCH].offset))(handler, event);
    handler[3] = (uintptr_t)owner; handler[5] = 0;

    const double values[] = {NAN, INFINITY, -INFINITY, 0.0, -0.0, -0.5, 0.5};
    for (unsigned i = DSP_MIX; i <= DSP_MOTOR_RAMP_TIME; i++) {
        dsp_event_fn call = (void *)(image + dsp_hooks[i].offset);
        size_t offset = i == DSP_MIX ? 0x0c : (i == DSP_PARAMETER ? 0x20 : 0x08);
        for (size_t j = 0; j < sizeof values / sizeof *values; j++) {
            double value = values[j]; float f = (float)value;
            if (i < DSP_NUDGE) memcpy((char *)event + offset, &f, sizeof f);
            else memcpy((char *)event + offset, &value, sizeof value);
            unsigned before = dsp_calls;
            call(handler, event);
            int accepted = sc_dsp_finite_f64(value) && (i != DSP_TEMPO || value > 0.0);
            if ((i == DSP_RAMP_TIME || i == DSP_MOTOR_RAMP_TIME) && value < 0.0) accepted = 0;
            assert(dsp_calls == before + (unsigned)accepted);
        }
        unsigned before = dsp_calls;
        call(handler, NULL);
        assert(dsp_calls == before);
    }
    assert(DSP_HOOK_COUNT == 20);
    puts("ARM DSP install: all 20 prefix guards, repeat install, event ABI/virtual/adjusted this, finite controls: PASS");

    unsigned fx_generation_before = fx_generation(dsp_wanted_owner);
    uint32_t fx_remaining = sc_dsp_fx_lookup(dsp_wanted_owner,1)->remaining;
    // Count before calling the original, without inspecting payloads or arming FX.
    dsp_wanted_event = NULL;
    for (unsigned i = DSP_SAMPLER_IMMEDIATE; i <= DSP_SAMPLER_PLAY; i++) {
        unsigned before = dsp_calls, gen = sc_dsp_state()->sampler_generation;
        ((dsp_event_fn)(image + dsp_hooks[i].offset))(handler, NULL);
        assert(dsp_calls == before + 1 && dsp_seen_sampler_generation == gen + 1);
    }
    dsp_wanted_event = event;
    uint32_t sampler_before = sc_dsp_state()->sampler_generation;
    sc_dsp_sampler_mark_event(owner, 0); sc_dsp_sampler_mark_event(owner, 4);
    assert(sc_dsp_state()->sampler_generation == sampler_before);
    struct sampler_thread_args args = {image, handler, event};
    pthread_t sampler_workers[4];
    unsigned calls_before = dsp_calls;
    for (unsigned i = 0; i < 4; i++) assert(!pthread_create(&sampler_workers[i], NULL, sampler_thread, &args));
    for (unsigned i = 0; i < 4; i++) assert(!pthread_join(sampler_workers[i], NULL));
    assert(sc_dsp_state()->sampler_generation == sampler_before + 6000 && dsp_calls == calls_before + 6000);
    __atomic_store_n(&sc_dsp_state()->sampler_generation, UINT32_MAX, __ATOMIC_RELEASE);
    ((dsp_event_fn)(image + dsp_hooks[DSP_SAMPLER_PLAY].offset))(handler, event);
    assert(sc_dsp_state()->sampler_generation == 0 && dsp_seen_sampler_generation == 0);
    assert(fx_generation(dsp_wanted_owner) == fx_generation_before && sc_dsp_fx_lookup(dsp_wanted_owner,1)->remaining == fx_remaining);
    puts("ARM Sampler: all three dispatchers, mark-before-forward, null payload, four producers, wrap and FX isolation: PASS");

    for (unsigned slot = DSP_DUB_ECHO; slot <= DSP_WASH; slot++) {
        float in_l[32], in_r[32], untouched_l[32], untouched_r[32];
        float *in_pair[2] = {in_l, in_r}, *untouched[2] = {untouched_l, untouched_r};
        dsp_process_fn call = (void *)(image + dsp_hooks[slot].offset);
        sweep_owner_adjustment = slot == DSP_DUB_ECHO ? 0x60 : 0x70;
        for (unsigned alias = 0; alias < 2; alias++) {
            for (unsigned i = 0; i < 32; i++) {
                in_l[i] = in_r[i] = 17.0f;
                untouched_l[i] = untouched_r[i] = 23.0f;
            }
            sweep_wanted_frames = 27;
            call(owner, in_pair, alias ? in_pair : untouched, sweep_wanted_frames);
            for (unsigned ch = 0; ch < 2; ch++) for (unsigned i = 0; i < 27; i++) {
                float expected = sweep_expected[(i + ch) % (sizeof sweep_expected / sizeof *sweep_expected)];
                assert(!memcmp(&in_pair[ch][i], &expected, sizeof expected));
            }
            for (unsigned i = 0; i < 32; i++) assert(untouched_l[i] == 23.0f && untouched_r[i] == 23.0f);
            assert(in_l[27] == 17.0f && in_r[31] == 17.0f);
            sweep_wanted_frames = 0;
            call(owner, in_pair, untouched, 0);
            assert(in_l[27] == 17.0f);
        }
    }
    assert(sweep_calls == 8);
    float single_sample = NAN, *partial_buffers[2] = {&single_sample, NULL};
    sc_dsp_sweep_fx_post_process(NULL, NULL, 1);
    sc_dsp_sweep_fx_post_process(NULL, partial_buffers, 0);
    assert(!sc_dsp_finite_f32(single_sample));
    sc_dsp_sweep_fx_post_process(NULL, partial_buffers, 1);
    assert(single_sample == 0.0f);
    // Include signaling NaNs and subnormals, beyond the usual math.h constants.
    const uint32_t raw_bits[] = {0x7f800001, 0xff800001, 0x7fc00123, 0x80000000, 0x00000001};
    const uint32_t wanted_bits[] = {0, 0, 0, 0x80000000, 0x00000001};
    float bit_samples[5]; memcpy(bit_samples, raw_bits, sizeof raw_bits);
    float *bit_buffers[2] = {bit_samples, NULL};
    sc_dsp_sweep_fx_post_process(NULL, bit_buffers, 5);
    assert(!memcmp(bit_samples, wanted_bits, sizeof wanted_bits));
    assert(fx_generation(dsp_wanted_owner) == fx_generation_before && sc_dsp_fx_lookup(dsp_wanted_owner,1)->remaining == fx_remaining);
    puts("ARM DubEcho/Wash: prologue/this adjustment, input/output ABI, NaN/Inf/extreme clamp, zeros and bounds: PASS");

    unsigned char gate[512]={0},other_gate[512]={0};
    float phase=.75f;memcpy(gate+0x1e8,&phase,4);memcpy(other_gate+0x1e8,&phase,4);
    struct channel_case ca={.value=1,.gate=gate},cb={.value=2,.gate=other_gate};
    float left[256]={0},right[256]={0},*in[2]={left,right};
    float ol[256],oright[256],*out[2]={ol,oright};
    test_channel_fn channel=(void *)(image+dsp_hooks[DSP_CHANNEL].offset);
    channel_gate=(void *)(image+dsp_hooks[DSP_AUTOGATE].offset);
    call_channel(channel,&ca,in,out,256);assert(ol[0]==1&&gate_seen_phase==.75f);
    call_channel(channel,&cb,in,out,256);assert(ol[0]==2&&gate_seen_phase==.75f);
    sc_dsp_fx_mark_transition(&ca,SC_DSP_FX_TRANSITION_TURN_OFF);ca.value=0;
    call_channel(channel,&cb,in,out,256);assert(ol[0]==2&&gate_seen_phase==.75f);
    call_channel(channel,&ca,in,out,64);assert(ol[0]==1&&oright[63]==65.0f/128&&gate_seen_phase==0);
    call_channel(channel,&ca,in,out,192);assert(ol[0]==.5f&&ol[63]==1.0f/128&&ol[64]==0);
    call_channel(channel,&ca,in,out,0);
    channel_gate(gate,in,NULL,0);assert(gate_zero_calls==1);
    assert(gate_nonzero_calls==5);
    ca.gate=cb.gate=NULL;ca.value=1;ca.child=&cb;nested_channel=channel;
    call_channel(channel,&ca,in,out,256);assert(ol[255]==1);ca.child=NULL;
    struct channel_worker_args audio_args[2]={{channel,&ca},{channel,&cb}};
    pthread_t audio_threads[2];
    for(unsigned i=0;i<2;i++)assert(!pthread_create(&audio_threads[i],NULL,channel_worker,&audio_args[i]));
    for(unsigned i=0;i<2;i++)assert(!pthread_join(audio_threads[i],NULL));
    puts("ARM ChannelDJFx: 6 general/stack arguments, d0-d7, nested TLS restoration, per-owner OFF ramp/AutoGate isolation: PASS");

    dsp_wanted_owner=owner;
    dsp_process_fn process=(void *)(image+dsp_hooks[DSP_COMMON].offset);
    common_bad_output=0;process(owner,in,out,160);assert(ol[0]==1&&oright[159]==1);
    sc_dsp_fx_mark_transition(owner,SC_DSP_FX_TRANSITION_SWITCH);
    process(owner,in,in,160);assert(left[0]==1); // common cannot consume channel progress
    common_bad_output=1;
    for(unsigned i=0;i<160;i++){left[i]=.25f;right[i]=.5f;}
    process(owner,in,out,160);assert(ol[0]==.25f&&oright[1]==.5f);
    process(owner,in,in,160);assert(left[0]==0&&right[1]==0);
    common_bad_output=0;
    puts("ARM common tail: original prologue/ABI, stateless finite guard, alias sanitation without stealing transitions: PASS");
    for (unsigned i = 0; i < DSP_HOOK_COUNT; i++) {
        assert(!munmap(dsp_originals[i], page)); dsp_originals[i] = NULL;
    }
    memset(sc_dsp_state(), 0, sizeof *sc_dsp_state());
    assert(!munmap(image, image_size));

}

__attribute__((naked)) static void invoke_stub(void *feature __attribute__((unused)),
    void *frame __attribute__((unused)), void *stub __attribute__((unused)),
    uint32_t *out __attribute__((unused))) {
    __asm__("push {r4, r5, r7, r8, fp, lr}\n"
            "mov r7, r0\nmov fp, r1\nmov r8, r2\nmov r5, r3\n"
            "mov r0, #17\nmov r1, #23\nmov r2, #29\nmov r4, #0\nblx r8\nstm r5, {r0, r1, r2, r3}\n"
            "pop {r4, r5, r7, r8, fp, pc}");
}

static void test_stems(void) {
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    char *image = mmap(NULL, 0x2a00000, PROT_READ | PROT_WRITE,
                       MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(image != MAP_FAILED);
    uint32_t enabled[] = {0xe51b2328, 0xe51b3324, 0xe12fff1e};
    uint32_t ordinary[] = {0x05873004, 0xe12fff1e}; // streq r3,[r7,#4]; bx lr
    memcpy(image + 0x8429b0, enabled, sizeof enabled);
    memcpy(image + 0x84264c, ordinary, sizeof ordinary);
    char *code_page = image + 0x84264c / page * page;
    assert(mprotect(code_page, page, PROT_READ | PROT_EXEC) == 0);
    __builtin___clear_cache(code_page, code_page + page);
    image[0x258e185] = 1;
    prepare_stems_feature((uintptr_t)image, page);
    uint32_t *feature = (void *)(image + 0x29bc83c);
    uint32_t *other = feature + 8;
    uint32_t false_ptr = (uint32_t)(uintptr_t)(image + 0x258e186);
    feature[1] = other[1] = false_ptr;
    uint32_t frame[0x100] = {0}, out[4];
    char *fp = (char *)frame + 0x350;
    *(uint32_t *)(fp - 808) = 1234;
    *(uint32_t *)(fp - 804) = 5678;
    *(uint32_t *)(fp - 832) = false_ptr;
    void *stub = (void *)(uintptr_t)stems_feature_jump[1];
    invoke_stub(feature, fp, stub, out);
    assert(feature[1] == (uint32_t)(uintptr_t)(image + 0x258e185));
    assert(*(char *)(uintptr_t)feature[1] == 1);
    assert(out[0] == 17 && out[1] == 23 && out[2] == 1234 && out[3] == 5678);
    invoke_stub(other, fp, stub, out);
    assert(other[1] == false_ptr);
    assert(out[0] == 17 && out[1] == 23 && out[2] == 29 && out[3] == false_ptr);
    puts("ARM Stems binding, other-feature isolation and register replay: PASS");
    munmap(image, 0x2a00000);
}

__attribute__((naked)) static void invoke_config(void *config __attribute__((unused)),
    void *stub __attribute__((unused)), uint32_t *out __attribute__((unused))) {
    __asm__("push {r4, lr}\nmov r4, r2\nblx r1\nstm r4, {r0, r2, r3}\npop {r4, pc}");
}

static void test_config(void) {
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    char *image = mmap(NULL, 0x1000000, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(image != MAP_FAILED);
    uint32_t resume = 0xe12fff1e;
    memcpy(image + 0xf63044, &resume, 4);
    *(uint32_t *)(image + 0xf63124) = 123;
    *(uint32_t *)(image + 0xf63120) = 456;
    assert(!mprotect(image + 0xf63000, page, PROT_READ | PROT_EXEC));
    __builtin___clear_cache(image + 0xf63000, image + 0xf64000);
    prepare_config((uintptr_t)image, page);
    const unsigned fields[] = {0xdb, 0x24c, 0x24e, 0x276, 0x289};
    for (int model = 0; model < 6; model++) {
        uchar config[0x500], expected[0x500];
        memset(config, 0xa0 + model, sizeof config);
        memcpy(expected, config, sizeof expected);
        for (size_t i = 0; i < sizeof fields / sizeof *fields; i++) expected[fields[i]] = i == 4 ? 0 : 1;
        uint32_t out[3];
        invoke_config(config, (void *)(uintptr_t)config_jump[1], out);
        assert(!memcmp(config, expected, sizeof config));
        assert(out[0] == (uintptr_t)config && out[1] == 123 && out[2] == 456);
    }
    munmap(image, 0x1000000);
    puts("Common config hook, six input layouts, literal/register replay: PASS");
}

static int exec_count;
static int real_sqlite_exec(void *db, const char *sql, int (*cb)(void *, int, char **, char **), void *arg, char **err) {
    exec_count++;
    return sqlite3_exec(db, sql, cb, arg, err);
}

static void emit_tail(char *at, const uint32_t *prefix, size_t count, const void *fn) {
    memcpy(at, prefix, count * 4);
    uint32_t jump[] = {0xe51ff004, (uint32_t)(uintptr_t)fn};
    memcpy(at + count * 4, jump, sizeof jump);
}

static int pragma(sqlite3 *db, const char *sql) {
    sqlite3_stmt *stmt = NULL;
    assert(sqlite3_prepare_v2(db, sql, -1, &stmt, NULL) == SQLITE_OK);
    assert(sqlite3_step(stmt) == SQLITE_ROW);
    int value = sqlite3_column_int(stmt, 0);
    assert(sqlite3_finalize(stmt) == SQLITE_OK);
    return value;
}

static void test_telemetry_names(void) {
    const char *yes[]={"events.db","posthog.db","/data/analytics/events.db",
        "file:/data/analytics/posthog.db?mode=rwc","file:events.db?cache=shared"};
    const char *no[]={NULL,"","/media/USB/events.db-collection/Engine Library/Database2/m.db",
        "/media/posthog.db/archive/m.db","/tmp/myevents.db","/tmp/events.db-wal","/tmp/posthog.db.bak",
        "file:/data/events.db/Database2/m.db?mode=rw","/tmp/events.db?literal-filename","file:/tmp/m.db?x=events.db"};
    for(size_t i=0;i<sizeof yes/sizeof *yes;i++)assert(is_telemetry_sqlite_filename(yes[i]));
    for(size_t i=0;i<sizeof no/sizeof *no;i++)assert(!is_telemetry_sqlite_filename(no[i]));
    puts("Telemetry SQLite exact basename, URI query and normal-library ancestor boundaries: PASS");
}

static void test_sqlite(void) {
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    char *image = mmap(NULL, 0x2a00000, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(image != MAP_FAILED);
    // Vendor entry prologues, followed by ABI-correct tails into real SQLite 3.45.3.
    uint32_t open_code[] = {0xe52db004, 0xe3a03000, 0xe28db000, 0xe28bd000, 0xe49db004};
    uint32_t v2_code[] = {0xe52db004, 0xe28db000, 0xe28bd000, 0xe49db004};
    uint32_t exec_code[] = {0xe16d42f4, 0xe1a05002, 0xe1cd40d0, 0xe28dd024};
    emit_tail(image + 0x1cba858, open_code, 5, sqlite3_open);
    emit_tail(image + 0x1cba874, v2_code, 4, sqlite3_open_v2);
    emit_tail(image + 0x1c91f74, exec_code, 4, real_sqlite_exec);
    uint32_t *api = (void *)(image + 0x29980a4);
    api[70] = (uintptr_t)image + 0x1cba858;
    api[135] = (uintptr_t)image + 0x1cba874;
    api[55] = (uintptr_t)image + 0x1c91f74;
    image[0x1c91f74] ^= 1;
    install_sqlite_hooks((uintptr_t)image, page);
    assert(*(uint32_t *)(image + 0x1cba858) == open_code[0]);
    image[0x1c91f74] ^= 1;
    assert(!mprotect(image + 0x1cba000, page, PROT_READ | PROT_EXEC));
    assert(!mprotect(image + 0x1c91000, page, PROT_READ | PROT_EXEC));
    __builtin___clear_cache(image, image + 0x2a00000);
    api[55] = 0;
    install_sqlite_hooks((uintptr_t)image, page);
    assert(*(uint32_t *)(image + 0x1cba858) == open_code[0]);
    api[55] = (uintptr_t)image + 0x1c91f74;
    install_sqlite_hooks((uintptr_t)image, page);
    sqlite_open_fn open_fn = (void *)(image + 0x1cba858);
    sqlite_open_v2_fn v2_fn = (void *)(image + 0x1cba874);
    void *db = NULL;
    char path[128];
    snprintf(path, sizeof path, "/tmp/sc5000-hook-test-%ld.db", (long)getpid());
    unlink(path);
    assert(v2_fn(path, &db, SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE, "unix") == SQLITE_OK);
    assert(exec_count == 1);
    assert(pragma(db, "PRAGMA synchronous") == 1);
    assert(pragma(db, "PRAGMA temp_store") == 2);
    assert(pragma(db, "PRAGMA cache_size") == -8000);
    assert(sqlite3_exec(db, "CREATE TABLE t(v); INSERT INTO t VALUES(42);", NULL, NULL, NULL) == SQLITE_OK);
    assert(sqlite3_close(db) == SQLITE_OK);
    assert(open_fn(path, &db) == SQLITE_OK && exec_count == 2);
    assert(pragma(db, "SELECT v FROM t") == 42);
    assert(sqlite3_close(db) == SQLITE_OK);
    assert(v2_fn(path, &db, SQLITE_OPEN_READONLY, NULL) == SQLITE_OK && exec_count == 3);
    assert(pragma(db, "PRAGMA cache_size") == -8000);
    assert(sqlite3_close(db) == SQLITE_OK);
    unlink(path);
    const char *memory[] = {"/nonexistent/analytics/posthog.db", "/nonexistent/analytics/events.db", ":memory:"};
    for (size_t i = 0; i < sizeof memory / sizeof *memory; i++) {
        assert(v2_fn(memory[i], &db, SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE, NULL) == SQLITE_OK);
        assert(!strcmp(sqlite3_db_filename(db, "main"), ""));
        assert(sqlite3_close(db) == SQLITE_OK);
    }
    assert(exec_count == 3);
    assert(open_fn(memory[0], &db) == SQLITE_OK && exec_count == 3);
    assert(sqlite3_close(db) == SQLITE_OK);
    assert(v2_fn("/nonexistent/hook-test/m.db", &db, SQLITE_OPEN_READONLY, NULL) != SQLITE_OK);
    assert(exec_count == 3);
    if (db) sqlite3_close(db);
    db = NULL;
    assert(v2_fn(path, &db, SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE, "nonexistent-vfs") != SQLITE_OK);
    if (db) sqlite3_close(db);
    tune_database(SQLITE_OK, path, NULL);
    assert(exec_count == 3);
    char ordinary_dir[128],ordinary_db[160];
    snprintf(ordinary_dir,sizeof ordinary_dir,"/tmp/events.db-collection-%ld",(long)getpid());
    assert(!mkdir(ordinary_dir,0700));
    snprintf(ordinary_db,sizeof ordinary_db,"%s/m.db",ordinary_dir);
    assert(v2_fn(ordinary_db,&db,SQLITE_OPEN_READWRITE|SQLITE_OPEN_CREATE,NULL)==SQLITE_OK);
    assert(!strcmp(sqlite3_db_filename(db,"main"),ordinary_db));
    assert(sqlite3_exec(db,"CREATE TABLE kept(v); INSERT INTO kept VALUES(73)",NULL,NULL,NULL)==SQLITE_OK);
    assert(sqlite3_close(db)==SQLITE_OK);
    assert(sqlite3_open(ordinary_db,(sqlite3 **)&db)==SQLITE_OK);
    assert(pragma(db,"SELECT v FROM kept")==73&&sqlite3_close(db)==SQLITE_OK);
    unlink(ordinary_db);rmdir(ordinary_dir);
    munmap(image, 0x2a00000);
    puts("Installed ARM SQLite hooks, real PRAGMA readback, memory telemetry, errors/readonly: PASS");
}

static void test_files(void) {
    char path[PATH_MAX], target[PATH_MAX], mapped[PATH_MAX];
    snprintf(path, sizeof path, "/tmp/sc5000-hook-%ld/CrashReportDB/report.lock", (long)getpid());
    hook_test_engine = 1;
    const char *p = volatile_path(AT_FDCWD, path, mapped);
    assert(p == mapped && strstr(p, "/tmp/sc5000-engine-volatile-") == p);
    strcpy(target, mapped);
    assert(volatile_path(AT_FDCWD, target, mapped) == target);
    int (*openers[])(const char *, int, ...) = {hook_open, hook_open64};
    int fd;
    for (size_t i = 0; i < sizeof openers / sizeof *openers; i++) {
        fd = openers[i](path, O_CREAT | O_RDWR | O_TRUNC, 0600);
        assert(fd >= 0 && write(fd, "ok", 2) == 2);
        close(fd);
    }
    struct stat st;
    assert(stat(path, &st) == -1 && stat(target, &st) == 0 && st.st_size == 2);
    char *slash = strrchr(path, '/'); *slash = 0;
    // Resolve relative openat through an existing directory without bypassing redirection.
    char *dest_slash = strrchr(target, '/'); *dest_slash = 0;
    int dirfd = open(target, O_RDONLY | O_DIRECTORY);
    assert(dirfd >= 0);
    // An analytics directory used as dirfd must retain filename/flags/mode.
    char analytics[PATH_MAX];
    snprintf(analytics, sizeof analytics, "/tmp/sc5000-hook-%ld/analytics", (long)getpid());
    mkdir(path, 0700); // Missing original parents are intentional; do not create them.
    char root[128]; snprintf(root, sizeof root, "/tmp/sc5000-hook-%ld", (long)getpid());
    assert(!mkdir(root, 0700) || errno == EEXIST);
    assert(!mkdir(analytics, 0700));
    int ad = open(analytics, O_RDONLY | O_DIRECTORY);
    assert(ad >= 0);
    int (*at_openers[])(int, const char *, int, ...) = {hook_openat, hook_openat64};
    for (size_t i = 0; i < sizeof at_openers / sizeof *at_openers; i++) {
        fd = at_openers[i](ad, "events.db-journal", O_CREAT | O_WRONLY, 0640);
        assert(fd >= 0 && !fstat(fd, &st) && (st.st_mode & 0777) == 0640);
        close(fd);
    }
    assert(faccessat(ad, "events.db-journal", F_OK, 0) == -1);
    close(ad); close(dirfd);
    assert(volatile_path(AT_FDCWD, "/tmp/ordinary.db", mapped) != mapped);
    pid_t pid = 12345;
    char *argv[] = {"crashpad_handler", NULL};
    assert(posix_spawn(&pid, "/usr/bin/crashpad_handler", NULL, NULL, argv, environ) == ENOENT && pid == 12345);
    assert(execv("/usr/bin/crashpad_handler", argv) == -1 && errno == ENOENT);
    argv[0] = "true";
    assert(posix_spawn(&pid, "/bin/true", NULL, NULL, argv, environ) == 0);
    int status; assert(waitpid(pid, &status, 0) == pid && WIFEXITED(status) && !WEXITSTATUS(status));
    hook_test_engine = 0;
    assert(volatile_path(AT_FDCWD, "/data/analytics/posthog.db", mapped) != mapped);
    assert(hook_openat(-99, "events.db", O_RDONLY) == -1 && errno == EBADF);
    puts("File/openat64 isolation, mode/flags, Crashpad rejection and ordinary spawn: PASS");
}

static void *named_worker(void *unused) {
    (void)unused;
    assert(pthread_setname_np(pthread_self(), "Analysis Queue") == 0);
    cpu_set_t cpus; assert(pthread_getaffinity_np(pthread_self(), sizeof cpus, &cpus) == 0);
    assert(!CPU_ISSET(0, &cpus) && CPU_COUNT(&cpus) == 1);
    int policy; struct sched_param p;
    assert(!pthread_getschedparam(pthread_self(), &policy, &p) && policy == SCHED_IDLE);
    assert(getpriority(PRIO_PROCESS, 0) == 19);
    assert((syscall(SYS_ioprio_get, 1, 0) >> 13) == 3);
    return NULL;
}

static void *foreign_named_worker(void *fd) {
    char c;
    assert(read(*(int *)fd, &c, 1) == 1);
    cpu_set_t cpus; assert(!pthread_getaffinity_np(pthread_self(), sizeof cpus, &cpus));
    assert(!CPU_ISSET(0, &cpus));
    assert(getpriority(PRIO_PROCESS, 0) == 10);
    return NULL;
}

struct qt_thread_case { const char *name; int time64, kind; };

static void *qt_named_worker(void *arg) {
    const struct qt_thread_case *test = arg;
    int (*name_fn)(int, ...) = test->time64 ? hook_prctl_time64 : hook_prctl;
    assert(!name_fn(PR_SET_NAME, (unsigned long)test->name));
    char name[16];
    assert(!name_fn(PR_GET_NAME, (unsigned long)name));
    assert(!strcmp(name, test->name));
    struct sched_param normal = {.sched_priority = 0}, actual;
    // Qt applies its requested scheduling policy after setting the name.
    assert(!pthread_setschedparam(pthread_self(), SCHED_OTHER, &normal));
    int policy;
    assert(!pthread_getschedparam(pthread_self(), &policy, &actual));
    assert(policy == (test->kind == 2 ? SCHED_IDLE : SCHED_OTHER));
    assert(actual.sched_priority == 0);
    assert(getpriority(PRIO_PROCESS, 0) == (test->kind == 2 ? 19 : 10));
    cpu_set_t wanted = engine_cpus, got, cpu1;
    CPU_CLR(0, &wanted);
    if (test->kind == 2) {
        int last = CPU_SETSIZE - 1;
        while (!CPU_ISSET(last, &wanted)) last--;
        CPU_ZERO(&wanted); CPU_SET(last, &wanted);
        assert((syscall(SYS_ioprio_get, 1, 0) >> 13) == 3);
    }
    assert(!pthread_getaffinity_np(pthread_self(), sizeof got, &got));
    assert(CPU_EQUAL(&got, &wanted));
    CPU_ZERO(&cpu1); CPU_SET(1, &cpu1);
    assert(!pthread_setaffinity_np(pthread_self(), sizeof cpu1, &cpu1));
    assert(!pthread_getaffinity_np(pthread_self(), sizeof got, &got));
    assert(CPU_EQUAL(&got, &wanted));
    assert(name_fn(PR_SET_NAME, 0UL, 0UL, 0UL, 0UL) == -1 && errno == EFAULT);
    return NULL;
}

static void test_threads(void) {
    hook_test_engine = 1;
    cpu_set_t saved, cpu1;
    assert(!sched_getaffinity(0, sizeof saved, &saved) && CPU_ISSET(1, &saved));
    CPU_ZERO(&cpu1); CPU_SET(1, &cpu1);
    assert(!sched_setaffinity(0, sizeof cpu1, &cpu1));
    initialize_engine_cpus();
    long online = sysconf(_SC_NPROCESSORS_ONLN);
    assert(online > 1 && online <= CPU_SETSIZE);
    assert(CPU_COUNT(&engine_cpus) == online && CPU_ISSET((int)online - 1, &engine_cpus));
    int before = getpriority(PRIO_PROCESS, 0);
    pthread_t thread;
    assert(!pthread_create(&thread, NULL, named_worker, NULL));
    assert(!pthread_join(thread, NULL));
    assert(getpriority(PRIO_PROCESS, 0) == before);
    int fds[2]; assert(!pipe(fds));
    assert(!pthread_create(&thread, NULL, foreign_named_worker, &fds[0]));
    assert(!pthread_setname_np(thread, "QQuickPixmapRea"));
    assert(getpriority(PRIO_PROCESS, 0) == before);
    assert(write(fds[1], "x", 1) == 1);
    assert(!pthread_join(thread, NULL));
    close(fds[0]); close(fds[1]);
    const struct qt_thread_case cases[] = {
        {"Analysis Queue ", 0, 2}, {"Freeze Checker ", 1, 2},
        {"QQuickPixmapRea", 0, 1}, {"FileInfoThread", 1, 1},
    };
    for (size_t i = 0; i < sizeof cases / sizeof *cases; i++) {
        assert(!pthread_create(&thread, NULL, qt_named_worker, (void *)&cases[i]));
        assert(!pthread_join(thread, NULL));
    }
    cpu_set_t main_cpus;
    assert(!sched_getaffinity(0, sizeof main_cpus, &main_cpus) && CPU_EQUAL(&main_cpus, &cpu1));
    assert(getpriority(PRIO_PROCESS, 0) == before);
    assert(hook_prctl_time64(PR_GET_DUMPABLE, 0UL, 0UL, 0UL, 0UL) >= 0);
    assert(thread_class("AudioOut") == 0 && thread_class("AudioRenderer") == 0);
    assert(thread_class("QQuickPixmapRea") == 1 && thread_class("Freeze Checker") == 2);
    hook_test_engine = 0;
    struct sched_param normal = {.sched_priority = 0};
    assert(!pthread_setschedparam(pthread_self(), SCHED_OTHER, &normal));
    assert(!sched_setaffinity(0, sizeof saved, &saved));
    engine_cpus_ready = 0;
    puts("pthread/prctl/time64 naming, Qt policy override, online CPUs and caller isolation: PASS");
}

struct mock_sw_params { int type; unsigned long start, stop, silence_threshold, silence_size; };
static int mock_get_rc, mock_set_rc, mock_apply_rc, mock_seen_type;
static char alsa_order[16];
static unsigned alsa_count;
static void *mock_pcm;

static int mock_tstamp_get(const void *params, int *type) {
    alsa_order[alsa_count++] = 'g';
    if (!mock_get_rc) *type = ((const struct mock_sw_params *)params)->type;
    return mock_get_rc;
}

static int mock_tstamp_set(void *pcm, void *params, int type) {
    assert(pcm == mock_pcm);
    alsa_order[alsa_count++] = 's';
    mock_seen_type = type;
    if (!mock_set_rc) ((struct mock_sw_params *)params)->type = type;
    return mock_set_rc;
}

static int mock_sw_apply(void *pcm, void *params) {
    assert(pcm == mock_pcm && params);
    alsa_order[alsa_count++] = 'a';
    return mock_apply_rc;
}

static void test_alsa(void) {
    void *old_sw = alsa_sw_cache, *old_set = alsa_tstamp_set_cache, *old_get = alsa_tstamp_get_cache;
    alsa_sw_cache = mock_sw_apply;
    alsa_tstamp_set_cache = mock_tstamp_set;
    alsa_tstamp_get_cache = mock_tstamp_get;
    char pcm; mock_pcm = &pcm;
    hook_test_engine = 1;
    for (int type = 0; type <= 2; type++) {
        struct mock_sw_params params, wanted;
        memset(&params, 0x5a, sizeof params);
        params.type = type; params.silence_threshold = 0;
        memcpy(&wanted, &params, sizeof wanted);
        wanted.type = type == 0 ? 2 : type;
        alsa_count = 0; memset(alsa_order, 0, sizeof alsa_order);
        assert(snd_pcm_sw_params(&pcm, &params) == 0);
        assert(!strcmp(alsa_order, type == 0 ? "gsa" : "ga"));
        assert(!memcmp(&params, &wanted, sizeof params));
    }
    struct mock_sw_params params = {.type = 0, .silence_threshold = 0};
    alsa_count = 0; mock_set_rc = -EINVAL;
    assert(snd_pcm_sw_params(&pcm, &params) == -EINVAL && alsa_count == 2 && params.type == 0);
    mock_set_rc = 0; mock_get_rc = -EINVAL; mock_apply_rc = -EBUSY; alsa_count = 0;
    assert(snd_pcm_sw_params(&pcm, &params) == -EBUSY && alsa_count == 2 && params.type == 0);
    mock_get_rc = 0; mock_apply_rc = 0;
    for (int type = -1; type <= 3; type++) {
        alsa_count = 0;
        assert(!snd_pcm_sw_params_set_tstamp_type(&pcm, &params, type));
        assert(mock_seen_type == (type == 0 ? 2 : type));
    }
    hook_test_engine = 0; params.type = 0; alsa_count = 0;
    assert(!snd_pcm_sw_params(&pcm, &params) && alsa_count == 1 && params.type == 0);
    alsa_count = 0;
    assert(!snd_pcm_sw_params_set_tstamp_type(&pcm, &params, 0) && mock_seen_type == 0);
    alsa_sw_cache = old_sw; alsa_tstamp_set_cache = old_set; alsa_tstamp_get_cache = old_get;
    puts("ALSA setup: realtime -> RAW, preserve explicit clocks/silence, call order and errors: PASS");
}

static int dns_calls;
static const char *dns_node, *dns_service;
static const struct addrinfo *dns_hints;

static int mock_dns(const char *node, const char *service, const struct addrinfo *hints, struct addrinfo **result) {
    assert(node == dns_node && service == dns_service && hints == dns_hints);
    dns_calls++;
    *result = NULL;
    return EAI_AGAIN;
}

static void test_update_dns(void) {
    void *old = getaddrinfo_cache; getaddrinfo_cache = mock_dns;
    hook_test_engine = 1;
    const char *blocked[] = {"autoupdate.airmusictech.com", "AUTOUPDATE.AIRMUSICTECH.COM", "autoupdate.airmusictech.com."};
    struct addrinfo hints = {.ai_family = AF_UNSPEC}, *result;
    for (size_t i = 0; i < sizeof blocked / sizeof *blocked; i++) {
        result = &hints;
        assert(getaddrinfo(blocked[i], "443", &hints, &result) == EAI_NONAME && !result);
    }
    assert(!dns_calls);
    const char *allowed[] = {"device.inmusicbrands.com", "auth.profile.inmusicbrands.com",
        "ipv4.connman.inmusicbrands.com", "api.github.com", "notautoupdate.airmusictech.com",
        "autoupdate.airmusictech.com.example", NULL};
    dns_service = "443"; dns_hints = &hints;
    for (size_t i = 0; i < sizeof allowed / sizeof *allowed; i++) {
        dns_node = allowed[i];
        assert(getaddrinfo(dns_node, dns_service, dns_hints, &result) == EAI_AGAIN);
    }
    hook_test_engine = 0; dns_node = blocked[0];
    assert(getaddrinfo(dns_node, dns_service, dns_hints, &result) == EAI_AGAIN);
    assert(dns_calls == (int)(sizeof allowed / sizeof *allowed) + 1);
    getaddrinfo_cache = old;
    puts("Update DNS: exact official updater host blocked; auth/GitHub/connman/other processes preserved: PASS");
}

static void test_patch_files(const char *filename) {
    FILE *f = fopen(filename, "rb"); assert(f);
    assert(!fseek(f, 0, SEEK_END)); long size = ftell(f); assert(size > 0);
    rewind(f); char *image = malloc((size_t)size); assert(image);
    assert(fread(image, 1, (size_t)size, f) == (size_t)size); fclose(f);
    ElfW(Ehdr) *header = (void *)image;
    struct dl_phdr_info info = {.dlpi_addr = (uintptr_t)image,
        .dlpi_phdr = (void *)(image + header->e_phoff), .dlpi_phnum = header->e_phnum};
    uintptr_t found = 0;
    engine_base(&info, sizeof info, &found);
    assert(found == (uintptr_t)image);
    char id[] = "\xaf\x07\x38\x65\xa3\x31\x8d\x80\x7a\x83\x03\xf7\x16\x15\x2e\xac\x92\x3a\x4c\x6f";
    char *at = memmem(image, (size_t)size, id, 20); assert(at);
    *at ^= 1; found = 0;
    engine_base(&info, sizeof info, &found);
    assert(found == 0);
    *at ^= 1;
    for (size_t i = 0; i < sizeof engine_edits / sizeof *engine_edits; i++) {
        const struct engine_edit *e = &engine_edits[i];
        assert(e->offset + e->size <= (size_t)size);
        int state = edit_state((uintptr_t)image, e);
        assert(state == 1 || state == 2);
        memcpy(image + e->offset, e->replacement, e->size);
        assert(edit_state((uintptr_t)image, e) == 2);
        image[e->offset] ^= 0xff;
        assert(edit_state((uintptr_t)image, e) == 0);
    }
    for (unsigned i = 0; i < DSP_HOOK_COUNT; i++) {
        size_t bytes;
        const void *expected = dsp_expected(i, &bytes);
        assert(dsp_hooks[i].offset + bytes <= (size_t)size);
        assert(!memcmp(image + dsp_hooks[i].offset, expected, bytes));
    }
    free(image);
    printf("Stock/patched/mismatched edit classification: %s PASS\n", filename);
}

int main(int argc, char **argv) {
    test_dsp_hooks();
    test_stems();
    test_config();
    test_telemetry_names();
    test_sqlite();
    test_files();
    test_threads();
    test_alsa();
    test_update_dns();
    for (int i = 1; i < argc; i++) test_patch_files(argv[i]);
    return 0;
}
