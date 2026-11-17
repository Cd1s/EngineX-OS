#define _GNU_SOURCE
#include <assert.h>
#include <dlfcn.h>
#include <link.h>
#include <setjmp.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <unwind.h>

typedef void (*decode_fn)(void *, void *, uintptr_t, uintptr_t);
static jmp_buf resume;
static int reached_caller;
extern const char __start_rbx_unwind_caller[], __stop_rbx_unwind_caller[];

static _Unwind_Reason_Code visit(struct _Unwind_Context *context, void *unused) {
    (void)unused;
    uintptr_t ip = _Unwind_GetIP(context) & ~(uintptr_t)1;
    if (ip >= (uintptr_t)__start_rbx_unwind_caller && ip < (uintptr_t)__stop_rbx_unwind_caller)
        reached_caller = 1;
    return _URC_NO_REASON;
}

static void inspect_stack(void *json, void *track, uintptr_t r2, uintptr_t r3) {
    (void)json; (void)track; (void)r2; (void)r3;
    _Unwind_Backtrace(visit, NULL);
    longjmp(resume, 1); // original decoder's non-local exit, before metadata access
}

__attribute__((noinline, section("rbx_unwind_caller"))) static void invoke(decode_fn wrapper) {
    wrapper(NULL, NULL, 0, 0);
    __asm__ volatile ("" ::: "memory"); // retain the caller frame
}

int main(int argc, char **argv) {
    assert(argc == 4);
    void *library = dlopen(argv[1], RTLD_NOW | RTLD_LOCAL);
    if (!library) { fprintf(stderr, "%s\n", dlerror()); return 1; }
    struct link_map *mapping = NULL;
    assert(dlinfo(library, RTLD_DI_LINKMAP, &mapping) == 0);
    decode_fn *slot = (decode_fn *)(mapping->l_addr + strtoul(argv[2], NULL, 0));
    decode_fn wrapper = (decode_fn)(mapping->l_addr + strtoul(argv[3], NULL, 0));
    *slot = inspect_stack;
    if (!setjmp(resume)) invoke(wrapper);
    assert(reached_caller); // fails with the original cantunwind C wrapper
    dlclose(library);
    puts("rekordbox native decoder frame unwound to its caller");
}
