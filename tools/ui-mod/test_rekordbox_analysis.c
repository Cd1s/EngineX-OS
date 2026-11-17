#include <assert.h>
#include <string.h>
#include "rekordbox_analysis.h"

int main(void) {
    assert(sc_rbx_analysis_track(SC_RBX_ANALYSIS_SELECT "16449", -1) == 16449);
    assert(sc_rbx_analysis_track(SC_RBX_ANALYSIS_SELECT "16449;", -1) == 16449);
    const char *with_tail = SC_RBX_ANALYSIS_SELECT "77 junk";
    assert(sc_rbx_analysis_track(with_tail, (int)strlen(with_tail) - 5) == 77);
    const char *bad[] = {"", SC_RBX_ANALYSIS_SELECT, SC_RBX_ANALYSIS_SELECT "x", SC_RBX_ANALYSIS_SELECT "12 OR 1",
                         "SELECT trackId FROM PerformanceData WHERE trackId = 3", SC_RBX_ANALYSIS_SELECT "123456789012345"};
    for (unsigned i = 0; i < sizeof bad / sizeof *bad; i++) assert(!sc_rbx_analysis_track(bad[i], -1));

    assert(sc_rbx_uri_id("streaming://TIDAL/Track/855") == 855);
    assert(sc_rbx_uri_id("tidal:855") == 855);
    assert(!sc_rbx_uri_id("") && !sc_rbx_uri_id("855") && !sc_rbx_uri_id("streaming://TIDAL/Track/") && !sc_rbx_uri_id("x/a855"));

    const unsigned char file[] = "RBXA1" "\2\0\0\0" "\x01\xab" "\0\0\0\0" "\1\0\0\0" "\xff" "\0\0\0\0" "\0\0\0\0";
    char *sql = sc_rbx_analysis_sql(file, sizeof file - 1, 9);
    assert(sql && !strcmp(sql, "UPDATE PerformanceData SET trackData = X'01ab', overviewWaveFormData = NULL, beatData = X'ff', quickCues = NULL, loops = NULL WHERE trackId = 9 AND trackData IS NULL"));
    free(sql);
    assert(!sc_rbx_analysis_sql(file, sizeof file - 2, 9));                      // cut short
    assert(!sc_rbx_analysis_sql((const unsigned char *)"RBXA0", 5, 9));           // not ours
    const unsigned char huge[] = "RBXA1" "\xff\xff\xff\x7f";
    assert(!sc_rbx_analysis_sql(huge, sizeof huge - 1, 9));                       // length past the end
    const unsigned char no_grid[] = "RBXA1" "\1\0\0\0" "\x01" "\0\0\0\0" "\0\0\0\0" "\0\0\0\0" "\0\0\0\0";
    assert(!sc_rbx_analysis_sql(no_grid, sizeof no_grid - 1, 9));                 // nothing to skip analysis for
    return 0;
}
