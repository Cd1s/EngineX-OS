// LD_PRELOAD for qemu-arm on the build host: when the official Engine registers
// its embedded resources, write each (tree, names, data) triple verbatim to
// $QRC_DUMP_DIR/res-<n>.bin for extract_qrc.py. Never installed on a deck.
// Build: arm-linux-gnueabihf-gcc -O2 -shared -fPIC -o qrc_dump.so qrc_dump.c -ldl
#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>

typedef unsigned char uchar;
static uint32_t be32(const uchar *p) { return (uint32_t)p[0] << 24 | p[1] << 16 | p[2] << 8 | p[3]; }
static uint16_t be16(const uchar *p) { return (uint16_t)(p[0] << 8 | p[1]); }

struct span { const uchar *tree, *names, *data; size_t node, tree_end, names_end, data_end; };

static void walk(struct span *s, uint32_t n, int depth) {
    const uchar *e = s->tree + n * s->node;
    if ((n + 1) * s->node > s->tree_end) s->tree_end = (n + 1) * s->node;
    uint32_t name = be32(e);
    size_t name_end = name + 6 + 2 * (size_t)be16(s->names + name);
    if (n && name_end > s->names_end) s->names_end = name_end;
    if (be16(e + 4) & 2) {
        if (depth > 64) return;
        uint32_t count = be32(e + 6), first = be32(e + 10);
        for (uint32_t i = 0; i < count; i++) walk(s, first + i, depth + 1);
    } else {
        uint32_t off = be32(e + 10);
        size_t end = off + 4 + (size_t)be32(s->data + off);
        if (end > s->data_end) s->data_end = end;
    }
}

int _Z21qRegisterResourceDataiPKhS0_S0_(int version, const uchar *tree, const uchar *names, const uchar *data) {
    static int count;
    int (*real)(int, const uchar *, const uchar *, const uchar *) =
        (int (*)(int, const uchar *, const uchar *, const uchar *))dlsym(RTLD_NEXT, "_Z21qRegisterResourceDataiPKhS0_S0_");
    const char *dir = getenv("QRC_DUMP_DIR");
    if (dir && version >= 1 && version <= 3) {
        struct span s = {tree, names, data, version >= 2 ? 22 : 14, 0, 0, 0};
        walk(&s, 0, 0);
        char path[4096];
        snprintf(path, sizeof path, "%s/res-%03d.bin", dir, count++);
        FILE *f = fopen(path, "wb");
        if (f) {
            uint32_t head[4] = {(uint32_t)version, (uint32_t)s.tree_end, (uint32_t)s.names_end, (uint32_t)s.data_end};
            fwrite(head, sizeof head, 1, f);
            fwrite(tree, 1, s.tree_end, f);
            fwrite(names, 1, s.names_end, f);
            fwrite(data, 1, s.data_end, f);
            fclose(f);
        }
    }
    return real ? real(version, tree, names, data) : 0;
}
