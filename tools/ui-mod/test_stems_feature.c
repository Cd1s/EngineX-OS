// Compile and run with ARMv7 gcc / qemu-arm on IN.
#include "engine_ui_hook.c"
#include <assert.h>

__attribute__((naked)) static void invoke_stub(void *feature __attribute__((unused)),
    void *frame __attribute__((unused)), void *stub __attribute__((unused)),
    uint32_t *out __attribute__((unused))) {
    __asm__("push {r4, r5, r7, r8, fp, lr}\n"
            "mov r7, r0\nmov fp, r1\nmov r8, r2\nmov r5, r3\n"
            "mov r0, #17\nmov r1, #23\nblx r8\nstm r5, {r0, r1, r2, r3}\n"
            "pop {r4, r5, r7, r8, fp, pc}");
}

int main(void) {
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    char *image = mmap(NULL, 0x2a00000, PROT_READ | PROT_WRITE,
                       MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(image != MAP_FAILED);
    uint32_t resume = 0xe12fff1e; // bx lr
    memcpy(image + 0x8429b8, &resume, 4);
    char *code_page = image + 0x8429b8 / page * page;
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
    void *stub = (void *)(uintptr_t)stems_feature_jump[1];
    invoke_stub(feature, fp, stub, out);
    assert(feature[1] == (uint32_t)(uintptr_t)(image + 0x258e185));
    assert(*(char *)(uintptr_t)feature[1] == 1);
    assert(out[0] == 17 && out[1] == 23 && out[2] == 1234 && out[3] == 5678);
    invoke_stub(other, fp, stub, out);
    assert(other[1] == false_ptr);
    assert(out[0] == 17 && out[1] == 23 && out[2] == 1234 && out[3] == 5678);
    puts("ARM Stems binding, other-feature isolation and register replay: PASS");
    return 0;
}
