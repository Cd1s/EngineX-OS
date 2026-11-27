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
            source.replace('    const unsigned at[]', '    code[6] |= 1;\n    const unsigned at[]', 1),
            source.replace('code[6] = HF_CONFIG(HF_STEMS_EQ, 0xe5c02054)', 'code[6] = 0xe5c02054', 1),
            source.replace('patch_bytes(HF_DSP_GUARDS, rate,', 'patch_bytes(feature, rate,', 1),
            source + '\nint untagged_wrapper(void) { return 0; }\n',
            source.replace('HF_INTERPOSE(HF_BLUETOOTH)\nint sbc_reinit_a2dp', 'int sbc_reinit_a2dp', 1),
            source + '\nstatic void new_hook(void *p) { mprotect(p, 4096, 3); }\n',
        ]
        for mutated in mutations:
            self.assertNotEqual(mutated, source)
            self.assertTrue(coverage.check(mutated))

    def test_az04_parity_guard_rejects_unexempt_a32_installers_and_features(self):
        source = (ROOT / 'tools/ui-mod/engine_ui_hook.c').read_text()
        self.assertEqual(coverage.check(source), [])

        # New installer behind #if !defined(__aarch64__) without AZ04_EXEMPT fails.
        unexempt_def = source.replace(
            '#endif\n\n// The sender\'s prologue',
            '#endif\n\n#if !defined(__aarch64__)\nstatic void install_future_a32(uintptr_t base, size_t page) { (void)base; (void)page; }\n#endif\n\n// The sender\'s prologue',
            1,
        )
        self.assertNotEqual(unexempt_def, source, 'anchor missing for unexempt_def')
        self.assertTrue(any('install_future_a32' in e for e in coverage.check(unexempt_def)))

        # Empty reason in AZ04_EXEMPT("") or AZ04_EXEMPT("  ") also fails.
        empty_reason = unexempt_def.replace(
            'static void install_future_a32(',
            'AZ04_EXEMPT("")\nstatic void install_future_a32(',
            1,
        )
        self.assertTrue(any('non-empty string' in e for e in coverage.check(empty_reason)))

        # With a documented AZ04_EXEMPT("Batch 2: ...") on the definition, parity passes.
        documented_def = unexempt_def.replace(
            'static void install_future_a32(',
            'AZ04_EXEMPT("Batch 2: planned AZ04 port")\nstatic void install_future_a32(',
            1,
        )
        self.assertEqual(coverage.check(documented_def), [])

        # A32_ONLY installer without AZ04_EXEMPT fails.
        a32_only_unexempt = documented_def.replace(
            '#endif\n\n// The sender\'s prologue',
            '#endif\n\nA32_ONLY static void install_a32_only_future(uintptr_t b, size_t p) { (void)b; (void)p; }\n\n// The sender\'s prologue',
            1,
        )
        self.assertNotEqual(a32_only_unexempt, documented_def, 'anchor missing for a32_only_unexempt')
        self.assertTrue(any('install_a32_only_future' in e for e in coverage.check(a32_only_unexempt)))

        # A32-only installer call in edit_engine() without AZ04_EXEMPT fails.
        unexempt_call = documented_def.replace(
            '#if !defined(__aarch64__)\n    AZ04_EXEMPT',
            '#if !defined(__aarch64__)\n    install_future_a32(base, page);\n    AZ04_EXEMPT',
            1,
        )
        self.assertNotEqual(unexempt_call, documented_def, 'anchor missing for unexempt_call')
        self.assertTrue(any('A32-only call to install_future_a32' in e for e in coverage.check(unexempt_call)))
        exempt_call = documented_def.replace(
            '#if !defined(__aarch64__)\n    AZ04_EXEMPT',
            '#if !defined(__aarch64__)\n    AZ04_EXEMPT("Batch 2: planned AZ04 port")\n    install_future_a32(base, page);\n    AZ04_EXEMPT',
            1,
        )
        self.assertEqual(coverage.check(exempt_call), [])

        # Removing an existing A32-only installer exemption fails.
        removed_cfx = source.replace(
            'AZ04_EXEMPT("Batch 2: Color FX channel knob hooks awaiting AZ04 RE")\nstatic int install_cfx_scope',
            'static int install_cfx_scope',
            1,
        )
        self.assertNotEqual(removed_cfx, source, 'anchor missing for removed_cfx')
        self.assertTrue(any('install_cfx_scope' in e for e in coverage.check(removed_cfx)))

        # A feature added to armhf tables with no AZ04 site and no AZ04 exemption fails.
        unexempt_feat = source.replace(
            'HF_REVERB_TAIL}',
            'HF_FUTURE_GAP}',
            1,
        )
        self.assertNotEqual(unexempt_feat, source, 'anchor missing for unexempt_feat')
        self.assertTrue(any('HF_FUTURE_GAP' in e for e in coverage.check(unexempt_feat)))

        # Removing an existing AZ04 feature exemption fails.
        removed_reverb = source.replace(
            '    HF_REVERB_TAIL, AZ04_EXEMPT("Batch 2: Reverb tail hook awaiting AZ04 RE")\n',
            '',
            1,
        )
        self.assertNotEqual(removed_reverb, source, 'anchor missing for removed_reverb')
        self.assertTrue(any('HF_REVERB_TAIL' in e for e in coverage.check(removed_reverb)))
