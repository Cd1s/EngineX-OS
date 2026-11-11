/* Prove default OFF does not fault in the optional rings on the worker. */
#define _GNU_SOURCE
#include "pioneer_fx.h"
#include <assert.h>
#include <stdio.h>
#include <sys/mman.h>
#include <unistd.h>
int main(void) {
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    size_t bytes = (sizeof(struct sc_pfx_instance) + page - 1) & ~(page - 1);
    struct sc_pfx_instance *s = mmap(NULL, bytes, PROT_READ | PROT_WRITE,
                                    MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(s != MAP_FAILED && !madvise(s, bytes, MADV_NOHUGEPAGE));
    void *middle = (void *)((uintptr_t)(s->ms.l + SC_MS_RING / 2) & ~(uintptr_t)(page - 1));
    unsigned char resident = 0;
    sc_pfx_instance_prepare(s);
    assert(s->ms.ready == 0);
    assert(!mincore(middle, page, &resident) && !(resident & 1));
    sc_ms_prepare(&s->ms);
    assert(s->ms.ready == 2);
    assert(!mincore(middle, page, &resident) && (resident & 1));
    assert(!munmap(s, bytes));
    puts("test_ms_lazy PASS (OFF ring pages untouched; ON prepared before publication)");
    return 0;
}
