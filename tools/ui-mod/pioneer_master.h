/* Keep dfdef97 master Color FX on every architecture. Namespace the frozen
 * source so armhf selector instances can use their parameterized DSP separately.
 * Include before pioneer_fx.h; the frozen source itself stays byte-identical. */
#ifndef SC_PIONEER_MASTER_H
#define SC_PIONEER_MASTER_H
#define sc_pioneer_fx_mode sc_master_pioneer_fx_mode
#define sc_pioneer_fx sc_master_pioneer_fx
#define sc_pioneer_fx_parse sc_master_pioneer_fx_parse
#define sc_pioneer_fx_planar sc_master_pioneer_fx_planar
#define sc_pfx_sanitize sc_master_pfx_sanitize
#define sc_pfx_state sc_master_pfx_state
#define sc_pfx_clamp_out sc_master_pfx_clamp_out
#define sc_pfx_soft_sat sc_master_pfx_soft_sat
#define sc_pfx_read_ring sc_master_pfx_read_ring
#define sc_pfx_allpass sc_master_pfx_allpass
#define SC_PFX_OFF SC_MASTER_PFX_OFF
#define SC_PFX_SPIRAL SC_MASTER_PFX_SPIRAL
#define SC_PFX_PITCH_ECHO SC_MASTER_PFX_PITCH_ECHO
#define SC_PFX_SLIP_ROLL SC_MASTER_PFX_SLIP_ROLL
#define SC_PFX_DUB_ECHO SC_MASTER_PFX_DUB_ECHO
#define SC_PFX_SPACE SC_MASTER_PFX_SPACE
#include "pioneer_fx_legacy.h"
#undef SC_PIONEER_FX_H
#undef SC_PFX_SPACE
#undef SC_PFX_DUB_ECHO
#undef SC_PFX_SLIP_ROLL
#undef SC_PFX_PITCH_ECHO
#undef SC_PFX_SPIRAL
#undef SC_PFX_OFF
#undef sc_pfx_allpass
#undef sc_pfx_read_ring
#undef sc_pfx_soft_sat
#undef sc_pfx_clamp_out
#undef sc_pfx_state
#undef sc_pfx_sanitize
#undef sc_pioneer_fx_planar
#undef sc_pioneer_fx_parse
#undef sc_pioneer_fx
#undef sc_pioneer_fx_mode
#endif
