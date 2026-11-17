#ifndef SC_REKORDBOX_ANALYSIS_H
#define SC_REKORDBOX_ANALYSIS_H

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// Engine 5.3.0 reads a streamed track's analysis with this statement, the id inline.
#define SC_RBX_ANALYSIS_SELECT "SELECT trackId, trackData, overviewWaveFormData, beatData, quickCues, loops, thirdPartySourceId, activeOnLoadLoops FROM PerformanceData WHERE trackId = "

// The track whose analysis sql reads (n bytes, or NUL-terminated if n < 0), or 0.
static long long sc_rbx_analysis_track(const char *sql, int n) {
    size_t len = sizeof SC_RBX_ANALYSIS_SELECT - 1;
    if (!sql || (n >= 0 && (size_t)n <= len) || strncmp(sql, SC_RBX_ANALYSIS_SELECT, len)) return 0;
    const char *p = sql + len, *end = n >= 0 ? sql + n : NULL;
    long long id = 0;
    while ((!end || p < end) && *p >= '0' && *p <= '9') {
        if (id > 100000000000LL) return 0;
        id = id * 10 + (*p++ - '0');
    }
    if (p == sql + len) return 0;
    while ((!end || p < end) && (*p == ' ' || *p == ';')) p++;
    return (end ? p == end : !*p) ? id : 0;
}

// The streaming service's track id that ends Engine's uri ("…/855"), or 0.
static long long sc_rbx_uri_id(const char *uri) {
    size_t n = uri ? strlen(uri) : 0, i = n;
    while (i > 0 && uri[i - 1] >= '0' && uri[i - 1] <= '9') i--;
    if (i == n || n - i > 12 || i == 0 || (uri[i - 1] != '/' && uri[i - 1] != ':')) return 0;
    return strtoll(uri + i, NULL, 10);
}

// The UPDATE that gives the empty PerformanceData row of track the analysis in b, the file
// stream-sync writes: "RBXA1", then trackData, overviewWaveFormData, beatData, quickCues
// and loops, each a little-endian uint32 length and its bytes. NULL if b is not one.
static char *sc_rbx_analysis_sql(const unsigned char *b, size_t n, long long track) {
    static const char *const columns[] = {"trackData", "overviewWaveFormData", "beatData", "quickCues", "loops"};
    size_t at = 5, len[5], start[5], total = 160;
    if (n < 5 || memcmp(b, "RBXA1", 5)) return NULL;
    for (int i = 0; i < 5; i++) {
        if (n - at < 4) return NULL;
        len[i] = (size_t)b[at] | (size_t)b[at + 1] << 8 | (size_t)b[at + 2] << 16 | (size_t)b[at + 3] << 24;
        start[i] = at + 4;
        if (len[i] > n - start[i]) return NULL;
        at = start[i] + len[i];
        total += strlen(columns[i]) + 8 + 2 * len[i];
    }
    if (at != n || !len[0] || !len[2]) return NULL;
    char *sql = malloc(total), *p = sql;
    if (!sql) return NULL;
    p += sprintf(p, "UPDATE PerformanceData SET ");
    for (int i = 0; i < 5; i++) {
        p += sprintf(p, "%s%s = ", i ? ", " : "", columns[i]);
        if (!len[i]) { p += sprintf(p, "NULL"); continue; }
        *p++ = 'X'; *p++ = '\'';
        for (size_t k = 0; k < len[i]; k++) p += sprintf(p, "%02x", b[start[i] + k]);
        *p++ = '\'';
    }
    sprintf(p, " WHERE trackId = %lld AND trackData IS NULL", track);
    return sql;
}

#endif
