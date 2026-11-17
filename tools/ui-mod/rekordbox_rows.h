#ifndef SC_REKORDBOX_ROWS_H
#define SC_REKORDBOX_ROWS_H

#include <stdlib.h>
#include <string.h>

// TIDAL ignores BPM/key; recover only our carrier into Streaming::Track.
// dbserver BPM is an integer in hundredths; parse it without LC_NUMERIC.
static int sc_rekordbox_row_values(const char *label, double *bpm, unsigned *wheel, char *mode) {
    if (strncmp(label, "rekordbox [", 11)) return 0;
    const char *p = label + 11;
    char *end;
    if (*p < '0' || *p > '9') return 0;
    unsigned long whole = strtoul(p, &end, 10), fraction = 0;
    if (whole >= 1000) return 0;
    if (*end == '.') {
        const char *start = end + 1;
        fraction = strtoul(start, &end, 10);
        if (*start < '0' || *start > '9' || end - start < 1 || end - start > 2) return 0;
        if (end - start == 1) fraction *= 10;
    }
    if (strncmp(end, " BPM", 4)) return 0;
    p = end + 4;
    unsigned long key = 0;
    char letter = 0;
    if (*p == ' ') {
        if (p[1] < '0' || p[1] > '9') return 0;
        key = strtoul(p + 1, &end, 10);
        if (key < 1 || key > 12 || (*end != 'A' && *end != 'B')) return 0;
        letter = *end;
        p = end + 1;
    }
    if (p[0] != ']' || p[1] || (!whole && !fraction)) return 0;
    *bpm = (whole * 100 + fraction) / 100.0;
    *wheel = (unsigned)key;
    *mode = letter;
    return 1;
}

#endif
