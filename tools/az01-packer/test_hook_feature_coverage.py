"""A new untagged site must fail the same guard run by build.sh."""
import importlib.util
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('coverage', ROOT / 'tools/ui-mod/check_hook_features.py')
coverage = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(coverage)


class HookFeatureCoverageTest(unittest.TestCase):
    def test_current_hook_and_untagged_mutations(self):
        source = (ROOT / 'tools/ui-mod/engine_ui_hook.c').read_text()
        self.assertEqual(coverage.check(source), [])
        mutations = [
            source.replace('EDIT_GATEWAY, HF_GATEWAY}', 'EDIT_GATEWAY}', 1),
            source.replace('patch_bytes(HF_DSP_GUARDS, rate,', 'patch_bytes(rate,', 1),
            source.replace('patch_pointer(HF_GATEWAY, slot,', 'patch_pointer(slot,', 1),
            source.replace('HF_CONFIG(HF_STEMS, 0xe5c020db)', '0xe5c020db', 1),
            source.replace('code[6] = HF_CONFIG(HF_STEMS_EQ, 0xe5c02054)', 'code[6] = 0xe5c02054', 1),
            source.replace('patch_bytes(HF_DSP_GUARDS, rate,', 'patch_bytes(feature, rate,', 1),
            source + '\nint untagged_wrapper(void) { return 0; }\n',
            source.replace('HF_INTERPOSE(HF_BLUETOOTH)\nint sbc_reinit_a2dp', 'int sbc_reinit_a2dp', 1),
            source + '\nstatic void new_hook(void *p) { mprotect(p, 4096, 3); }\n',
        ]
        for mutated in mutations:
            self.assertNotEqual(mutated, source)
            self.assertTrue(coverage.check(mutated))
