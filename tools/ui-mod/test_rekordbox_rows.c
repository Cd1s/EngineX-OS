#include <assert.h>
#include "rekordbox_rows.h"

int main(void) {
    double bpm = 0;
    unsigned wheel = 0;
    char mode = 0;
    assert(sc_rekordbox_row_values("rekordbox [123.45 BPM 8A]", &bpm, &wheel, &mode));
    assert(bpm == 123.45 && wheel == 8 && mode == 'A');
    assert(sc_rekordbox_row_values("rekordbox [90 BPM 12B]", &bpm, &wheel, &mode));
    assert(bpm == 90 && wheel == 12 && mode == 'B');
    assert(sc_rekordbox_row_values("rekordbox [128 BPM]", &bpm, &wheel, &mode));
    assert(bpm == 128 && wheel == 0);
    const char *bad[] = {
        "", "TIDAL [123 BPM 8A]", "Beatport [123 BPM 8A]", "rekordbox",
        "rekordbox [NaN BPM 8A]", "rekordbox [inf BPM 8A]", "rekordbox [-1 BPM 8A]",
        "rekordbox [0 BPM 8A]", "rekordbox [1000 BPM 8A]", "rekordbox [123 BPM 0A]",
        "rekordbox [123 BPM 13A]", "rekordbox [123 BPM 8C]", "rekordbox [123 BPM 8A]junk",
        "rekordbox [123 BPM 8A", "rekordbox [123.456 BPM 8A]",
        "rekordbox [1e2 BPM 8A]", "rekordbox [+123 BPM 8A]", "rekordbox [123BPM8A]", "rekordbox [123 BPM 999999999999999999999999A]",
    };
    for (unsigned i = 0; i < sizeof bad / sizeof *bad; i++) {
        bpm = 99; wheel = 3; mode = 'B';
        assert(!sc_rekordbox_row_values(bad[i], &bpm, &wheel, &mode));
        assert(bpm == 99 && wheel == 3 && mode == 'B');
    }
    return 0;
}
