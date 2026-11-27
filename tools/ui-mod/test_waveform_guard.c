// Zoomed waveform guards (#336 S5): failing-before and passing-after proof for
// LibrarianTrackData::startWaveformBuilder (+0x120/+0x1e8 latch & dead-job
// recovery) and RMSNodeCache::populateNode (null GL context, hidden layer
// !isVisible(), and post-check FBO/shader validation).
//
// Build & run on native armhf or aarch64:
//   gcc -O2 -Wall -Wextra -I. -o test_waveform_guard test_waveform_guard.c -ldl -lpthread
//   ./test_waveform_guard
#define ENGINE_UI_HOOK_TEST
int hook_test_engine;
#include "engine_ui_hook.c"
#include <assert.h>

static char tmp_dir[] = "/tmp/waveform-guard-XXXXXX";
static char tmp_conf[sizeof tmp_dir + 32];

// Mock C++ objects matching the verified Engine struct layouts.
struct mock_ctrl {
    uint8_t pad[WF_CTRL_USE_COUNT_OFF];
    int32_t use_count;
};

struct mock_builder {
    uint8_t pad0[WF_BUILDER_STATE_OFF];
    int32_t state;
    uint8_t pad1[WF_BUILDER_TREE_ROOT_OFF - WF_BUILDER_STATE_OFF - sizeof(int32_t)];
    void *tree_root;
};

struct mock_track_data {
    uint8_t pad0[WF_LTD_BUILDER_PTR_OFF];
    void *builder_ptr;
    void *builder_ctrl;
    uint8_t started;
};

_Static_assert(offsetof(struct mock_ctrl, use_count) == WF_CTRL_USE_COUNT_OFF, "ctrl use_count off");
_Static_assert(offsetof(struct mock_builder, state) == WF_BUILDER_STATE_OFF, "builder state off");
_Static_assert(offsetof(struct mock_builder, tree_root) == WF_BUILDER_TREE_ROOT_OFF, "builder tree off");
_Static_assert(offsetof(struct mock_track_data, builder_ptr) == WF_LTD_BUILDER_PTR_OFF, "ltd ptr off");
_Static_assert(offsetof(struct mock_track_data, builder_ctrl) == WF_LTD_BUILDER_CTRL_OFF, "ltd ctrl off");
_Static_assert(offsetof(struct mock_track_data, started) == WF_LTD_STARTED_OFF, "ltd started off");

struct mock_renderer {
    uint8_t pad0[WF_REND_SHADER_OFF];
    void *shader_program;
};

struct mock_rms_node {
    uint8_t pad0[WF_RMS_FBO_OFF];
    void *fbo;
    void *renderer;
    uint8_t pad1[WF_RMS_VIS_OFF - WF_RMS_REND_OFF - sizeof(void *)];
    uint8_t is_visible;
    uint8_t dirty;
};

struct mock_tile_entry {
    void *transform;
    struct mock_rms_node *rms;
};

struct mock_rms_cache {
    uint8_t pad0[WF_CACHE_ITEM_OFF];
    void *item;
};

_Static_assert(offsetof(struct mock_renderer, shader_program) == WF_REND_SHADER_OFF, "rend shader off");
_Static_assert(offsetof(struct mock_rms_node, fbo) == WF_RMS_FBO_OFF, "rms fbo off");
_Static_assert(offsetof(struct mock_rms_node, renderer) == WF_RMS_REND_OFF, "rms rend off");
_Static_assert(offsetof(struct mock_rms_node, is_visible) == WF_RMS_VIS_OFF, "rms vis off");
_Static_assert(offsetof(struct mock_rms_node, dirty) == WF_RMS_VIS_OFF + 1, "rms dirty off");
_Static_assert(offsetof(struct mock_tile_entry, rms) == WF_ENTRY_RMS_OFF, "entry rms off");
_Static_assert(offsetof(struct mock_rms_cache, item) == WF_CACHE_ITEM_OFF, "cache item off");

// Controllable stubs mirroring stock Engine's exact buggy functions.
static int stub_decoder_ready;
static int stub_swb_calls;
static int stub_swb_jobs_launched;
static struct mock_builder stub_builders[8];
static struct mock_ctrl stub_ctrls[8];
static int stub_builder_idx;

static void stock_start_waveform_builder(void *self, int flag) {
    (void)flag;
    stub_swb_calls++;
    struct mock_track_data *td = (struct mock_track_data *)self;
    // Exact stock bug: reads old started, unconditionally writes 1 BEFORE
    // checking getAudioDecoder() / cache / metadata.
    uint8_t old = td->started;
    td->started = 1;
    if (old != 0) return;
    if (!stub_decoder_ready) return; // early exit leaves td->started == 1!
    int idx = stub_builder_idx++;
    memset(&stub_builders[idx], 0, sizeof stub_builders[idx]);
    stub_ctrls[idx].use_count = 2; // LibrarianTrackData + WaveformBuilderJob
    td->builder_ptr = &stub_builders[idx];
    td->builder_ctrl = &stub_ctrls[idx];
    stub_swb_jobs_launched++;
}

static void *stub_gl_ctx;
static uint8_t stub_item_vis;
static uint32_t stub_fbo_tex;
static int stub_pop_calls;
static int stub_force_null_shader;
static int stub_force_null_fbo;
static struct mock_renderer stub_renderers[8];
static int stub_rend_idx;
static int stub_fbo_token;

static void *fake_gl_current_context(void) { return stub_gl_ctx; }
static uint8_t fake_item_is_visible(const void *item) { (void)item; return stub_item_vis; }
static uint32_t fake_fbo_texture(const void *fbo) { (void)fbo; return stub_fbo_tex; }

static int stock_populate_node(void *cache, void *tile_idx, void *node_ptr_ptr) {
    (void)cache;
    (void)tile_idx;
    stub_pop_calls++;
    struct mock_tile_entry *entry = *(struct mock_tile_entry **)node_ptr_ptr;
    struct mock_rms_node *rms = entry->rms;
    // Exact stock bug 1: stores WaveFormRenderer (+0xec/+0x140) with shaderProgram == NULL
    // BEFORE checking QOpenGLContext::currentContext().
    if (!rms->renderer) {
        struct mock_renderer *r = &stub_renderers[stub_rend_idx++];
        r->shader_program = NULL;
        rms->renderer = r;
        if (stub_gl_ctx && !stub_force_null_shader)
            r->shader_program = (void *)0x5aade2;
    }
    if (!rms->fbo && !stub_force_null_fbo)
        rms->fbo = &stub_fbo_token;
    // Exact stock bug 2: records item->isVisible() into +0xf5/+0x151 and returns 1
    // even when hidden or when GL context/shader init failed.
    rms->is_visible = stub_item_vis;
    rms->dirty = 1;
    return 1;
}

static void reset_stubs(void) {
    stub_decoder_ready = 0;
    stub_swb_calls = 0;
    stub_swb_jobs_launched = 0;
    stub_builder_idx = 0;
    stub_gl_ctx = (void *)0x61c70;
    stub_item_vis = 1;
    stub_fbo_tex = 42;
    stub_pop_calls = 0;
    stub_force_null_shader = 0;
    stub_force_null_fbo = 0;
    stub_rend_idx = 0;
    wf_start_orig = stock_start_waveform_builder;
    wf_pop_orig = stock_populate_node;
    wf_gl_current_context = fake_gl_current_context;
    wf_item_is_visible = fake_item_is_visible;
    wf_fbo_texture = fake_fbo_texture;
    wf_qt_syms_resolved = 1;
}

// 1. Failing-before proof: demonstrate all 4 stock bugs without our guards.
static void test_stock_bugs_reproduce(void) {
    reset_stubs();
    struct mock_builder init_b = {0};
    struct mock_ctrl init_c = {.use_count = 1};
    struct mock_track_data td = {0};
    td.builder_ptr = &init_b;
    td.builder_ctrl = &init_c;

    // Stock Bug 1A: setTrackData calls startWaveformBuilder while decoder == NULL.
    stock_start_waveform_builder(&td, 0);
    assert(td.started == 1);
    assert(stub_swb_jobs_launched == 0);
    // Deck load calls startWaveformBuilder once decoder IS ready -> stock drops it forever!
    stub_decoder_ready = 1;
    stock_start_waveform_builder(&td, 1);
    assert(stub_swb_jobs_launched == 0);
    assert(td.builder_ptr == &init_b);

    // Stock Bug 1B: WaveformBuilderJob::run() exits silently on open failure
    // (use_count drops to 1, state == 0, tree_root == NULL) -> stock never retries!
    reset_stubs();
    stub_decoder_ready = 1;
    td.started = 0;
    td.builder_ptr = &init_b;
    td.builder_ctrl = &init_c;
    stock_start_waveform_builder(&td, 1);
    assert(stub_swb_jobs_launched == 1);
    ((struct mock_ctrl *)td.builder_ctrl)->use_count = 1; // job destroyed without finishing
    stock_start_waveform_builder(&td, 1);
    assert(stub_swb_jobs_launched == 1); // stuck permanently!

    // Stock Bug 2A: populateNode called when QOpenGLContext::currentContext() == NULL
    // poisons node->renderer (shader == NULL) and returns 1 (cached blank forever).
    reset_stubs();
    stub_gl_ctx = NULL;
    struct mock_rms_node rms = {0};
    struct mock_tile_entry entry = {.rms = &rms};
    struct mock_tile_entry *entry_p = &entry;
    int fake_item = 1;
    struct mock_rms_cache cache = {.item = &fake_item};
    int tile = 0;
    assert(stock_populate_node(&cache, &tile, &entry_p) == 1);
    assert(rms.renderer != NULL);
    assert(((struct mock_renderer *)rms.renderer)->shader_program == NULL);

    // Stock Bug 2B: populateNode called while !item->isVisible() (layer switch)
    // records is_visible = 0 and returns 1 (beforeRendering skips forever).
    reset_stubs();
    stub_item_vis = 0;
    memset(&rms, 0, sizeof rms);
    assert(stock_populate_node(&cache, &tile, &entry_p) == 1);
    assert(rms.is_visible == 0);
}

// 2. Passing-after proof for waveform_builder_start_guard.
static void test_builder_guard(void) {
    reset_stubs();
    struct mock_builder init_b = {0};
    struct mock_ctrl init_c = {.use_count = 1};
    struct mock_track_data td = {0};
    td.builder_ptr = &init_b;
    td.builder_ctrl = &init_c;

    // Early exit (decoder == NULL) unlatches started back to 0.
    waveform_builder_start_guard(&td, 0);
    assert(td.started == 0);
    assert(stub_swb_jobs_launched == 0);

    // Subsequent call when decoder IS ready launches job and keeps started == 1.
    stub_decoder_ready = 1;
    waveform_builder_start_guard(&td, 1);
    assert(td.started == 1);
    assert(stub_swb_jobs_launched == 1);
    assert(td.builder_ptr != &init_b);

    // While job is running (use_count == 2), another call does NOT relaunch.
    waveform_builder_start_guard(&td, 1);
    assert(stub_swb_jobs_launched == 1);
    assert(td.started == 1);

    // If job completes normally (use_count == 1, state == 2), another call does NOT relaunch.
    ((struct mock_ctrl *)td.builder_ctrl)->use_count = 1;
    ((struct mock_builder *)td.builder_ptr)->state = 2;
    waveform_builder_start_guard(&td, 1);
    assert(stub_swb_jobs_launched == 1);

    // If job wrote blocks (tree_root != NULL) even with state == 0, does NOT relaunch.
    int fake_node = 1;
    ((struct mock_builder *)td.builder_ptr)->state = 0;
    ((struct mock_builder *)td.builder_ptr)->tree_root = &fake_node;
    waveform_builder_start_guard(&td, 1);
    assert(stub_swb_jobs_launched == 1);

    // If job exited silently on open failure (use_count == 1, state == 0, tree_root == NULL),
    // guard unlatches and retries cleanly up to WF_MAX_DEAD_JOB_RETRIES times, then stops
    // so a permanently unreadable track cannot spin forever.
    ((struct mock_builder *)td.builder_ptr)->tree_root = NULL;
    for (unsigned r = 0; r < WF_MAX_DEAD_JOB_RETRIES; r++) {
        waveform_builder_start_guard(&td, 1);
        assert(stub_swb_jobs_launched == (int)(2 + r));
        assert(td.started == 1);
        ((struct mock_ctrl *)td.builder_ctrl)->use_count = 1;
    }
    // Budget exhausted: next call does not relaunch and keeps started == 1.
    waveform_builder_start_guard(&td, 1);
    assert(stub_swb_jobs_launched == (int)(1 + WF_MAX_DEAD_JOB_RETRIES));
    assert(td.started == 1);
    waveform_builder_start_guard(&td, 1);
    assert(stub_swb_jobs_launched == (int)(1 + WF_MAX_DEAD_JOB_RETRIES));
    // Reused address for a fresh track (started == 0) resets the retry budget.
    td.started = 0;
    waveform_builder_start_guard(&td, 1);
    assert(stub_swb_jobs_launched == (int)(2 + WF_MAX_DEAD_JOB_RETRIES));
}

// 3. Passing-after proof for rms_populate_node_guard.
static void test_populate_guard(void) {
    struct mock_rms_node rms = {0};
    struct mock_tile_entry entry = {.rms = &rms};
    struct mock_tile_entry *entry_p = &entry;
    int fake_item = 1;
    struct mock_rms_cache cache = {.item = &fake_item};
    int tile = 0;

    // Null GL context: returns 0 BEFORE calling stock (no poisoned renderer or FBO leak).
    reset_stubs();
    stub_gl_ctx = NULL;
    assert(rms_populate_node_guard(&cache, &tile, &entry_p) == 0);
    assert(stub_pop_calls == 0);
    assert(rms.renderer == NULL);
    assert(rms.fbo == NULL);

    // Hidden layer (!isVisible()): returns 0 BEFORE calling stock (no +0xf5=0 tile cached).
    reset_stubs();
    stub_item_vis = 0;
    assert(rms_populate_node_guard(&cache, &tile, &entry_p) == 0);
    assert(stub_pop_calls == 0);
    assert(rms.renderer == NULL);

    // Post-check: shader init failed (shaderProgram == NULL) -> returns 0 (never cached).
    reset_stubs();
    stub_force_null_shader = 1;
    memset(&rms, 0, sizeof rms);
    assert(rms_populate_node_guard(&cache, &tile, &entry_p) == 0);
    assert(stub_pop_calls == 1);

    // Post-check: invalid FBO (texture() == 0) -> returns 0 (never cached).
    reset_stubs();
    stub_fbo_tex = 0;
    memset(&rms, 0, sizeof rms);
    assert(rms_populate_node_guard(&cache, &tile, &entry_p) == 0);
    assert(stub_pop_calls == 1);

    // Post-check: null FBO -> returns 0 (never cached).
    reset_stubs();
    stub_force_null_fbo = 1;
    memset(&rms, 0, sizeof rms);
    assert(rms_populate_node_guard(&cache, &tile, &entry_p) == 0);
    assert(stub_pop_calls == 1);

    // Belt-and-suspenders post-check: even if isVisible symbol was unresolved,
    // a node with rms.is_visible == 0 is never cached.
    reset_stubs();
    wf_item_is_visible = NULL;
    stub_item_vis = 0;
    memset(&rms, 0, sizeof rms);
    assert(rms_populate_node_guard(&cache, &tile, &entry_p) == 0);
    assert(stub_pop_calls == 1);

    // Healthy path: GL context active, item visible, valid FBO & shader -> returns 1!
    reset_stubs();
    memset(&rms, 0, sizeof rms);
    assert(rms_populate_node_guard(&cache, &tile, &entry_p) == 1);
    assert(stub_pop_calls == 1);
    assert(rms.fbo != NULL && rms.renderer != NULL && rms.is_visible == 1 && rms.dirty == 1);
    assert(((struct mock_renderer *)rms.renderer)->shader_program != NULL);
}

// 4. Vtable installation, prefix fail-closed check, and HF_DSP_GUARDS feature gate.
static void test_install_and_feature_gate(void) {
    long ps = sysconf(_SC_PAGESIZE);
    assert(ps > 0);
    size_t page = (size_t)ps;
    // Allocate 2 pages: page 0 holds fake functions + prefixes, page 1 holds RO vtable slots.
    uint8_t *img = mmap(NULL, page * 2, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(img != MAP_FAILED);

    uint32_t swb_fn_off = 0x100;
    uint32_t pop_fn_off = 0x200;
    uint32_t swb_slot_off = (uint32_t)page + 0x10;
    uint32_t pop_slot_off = (uint32_t)page + 0x20;
    static const uchar p_swb[8] = {0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88};
    static const uchar p_pop[8] = {0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x12, 0x34};
    memcpy(img + swb_fn_off, p_swb, 8);
    memcpy(img + pop_fn_off, p_pop, 8);
    uintptr_t *swb_slot = (uintptr_t *)(img + swb_slot_off);
    uintptr_t *pop_slot = (uintptr_t *)(img + pop_slot_off);
    *swb_slot = (uintptr_t)(img + swb_fn_off);
    *pop_slot = (uintptr_t)(img + pop_fn_off);
    assert(mprotect(img + page, page, PROT_READ) == 0);

    struct engine_build mock_build = {0};
    mock_build.name = "mock-s5";
    mock_build.wf_start_slot = swb_slot_off;
    mock_build.wf_start_fn = swb_fn_off;
    memcpy(mock_build.wf_start_prefix, p_swb, 8);
    mock_build.wf_pop_slot = pop_slot_off;
    mock_build.wf_pop_fn = pop_fn_off;
    memcpy(mock_build.wf_pop_prefix, p_pop, 8);
    build = &mock_build;

    // 4a. When dsp-guards is disabled via hook-disable.conf, both slots stay untouched.
    assert(mkdtemp(tmp_dir) != NULL);
    snprintf(tmp_conf, sizeof tmp_conf, "%s/hook-disable.conf", tmp_dir);
    FILE *f = fopen(tmp_conf, "w");
    assert(f);
    fputs("dsp-guards\n", f);
    fclose(f);
    hf_path = tmp_conf;
    hf_reload();
    assert(!hf_on(HF_DSP_GUARDS));
    install_waveform_guards((uintptr_t)img, page);
    assert(*swb_slot == (uintptr_t)(img + swb_fn_off));
    assert(*pop_slot == (uintptr_t)(img + pop_fn_off));

    // 4b. Prefix mismatch fails closed (leaves slots untouched).
    unlink(tmp_conf);
    hf_reload();
    assert(hf_on(HF_DSP_GUARDS));
    img[swb_fn_off] ^= 0xff;
    img[pop_fn_off] ^= 0xff;
    install_waveform_guards((uintptr_t)img, page);
    assert(*swb_slot == (uintptr_t)(img + swb_fn_off));
    assert(*pop_slot == (uintptr_t)(img + pop_fn_off));

    // 4c. Matching prefixes install both vtable wrappers and keep page RO afterward.
    img[swb_fn_off] ^= 0xff;
    img[pop_fn_off] ^= 0xff;
    install_waveform_guards((uintptr_t)img, page);
    assert(*swb_slot == (uintptr_t)waveform_builder_start_guard);
    assert(*pop_slot == (uintptr_t)rms_populate_node_guard);
    assert((uintptr_t)wf_start_orig == (uintptr_t)(img + swb_fn_off));
    assert((uintptr_t)wf_pop_orig == (uintptr_t)(img + pop_fn_off));

    // 4d. Every build in engine_builds[] has non-zero verified S5 waveform guard sites.
    for (size_t i = 0; i < sizeof engine_builds / sizeof *engine_builds; i++) {
        assert(engine_builds[i].wf_start_slot != 0 && engine_builds[i].wf_start_fn != 0);
        assert(engine_builds[i].wf_pop_slot != 0 && engine_builds[i].wf_pop_fn != 0);
    }

    munmap(img, page * 2);
    rmdir(tmp_dir);
}

int main(void) {
    test_stock_bugs_reproduce();
    test_builder_guard();
    test_populate_guard();
    test_install_and_feature_gate();
    puts("test_waveform_guard: ok");
    return 0;
}
