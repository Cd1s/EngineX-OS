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

    def test_az04_exempt_strict_per_installer_binding(self):
        source = (ROOT / 'tools/ui-mod/engine_ui_hook.c').read_text()
        documented_def = source.replace(
            '#endif\n\n// The sender\'s prologue',
            '#endif\n\n#if !defined(__aarch64__)\nAZ04_EXEMPT("Batch 2: planned AZ04 port")\nstatic void install_future_a32(uintptr_t base, size_t page) { (void)base; (void)page; }\n#endif\n\n// The sender\'s prologue',
            1,
        )
        self.assertNotEqual(documented_def, source, 'anchor missing for documented_def')

        # An unannotated call immediately after an inline-exempt call (engine_ui_hook.c:2854) must fail.
        line_borrow = documented_def.replace(
            '    AZ04_EXEMPT("Batch 3: peer shadow browse deferral awaiting AZ04 RE") if (hf_on(HF_FOUR_DECK)) install_shadow_browse(base, page);\n',
            '    AZ04_EXEMPT("Batch 3: peer shadow browse deferral awaiting AZ04 RE") if (hf_on(HF_FOUR_DECK)) install_shadow_browse(base, page);\n    install_future_a32(base, page);\n',
            1,
        )
        self.assertNotEqual(line_borrow, documented_def, 'anchor missing for line_borrow')
        self.assertTrue(any('install_future_a32' in e for e in coverage.check(line_borrow)))

        # A second unannotated call on the SAME line after an inline-exempt call must fail.
        same_line_two_calls = documented_def.replace(
            '    AZ04_EXEMPT("Batch 3: peer shadow browse deferral awaiting AZ04 RE") if (hf_on(HF_FOUR_DECK)) install_shadow_browse(base, page);\n',
            '    AZ04_EXEMPT("Batch 3: peer shadow browse deferral awaiting AZ04 RE") if (hf_on(HF_FOUR_DECK)) install_shadow_browse(base, page); install_future_a32(base, page);\n',
            1,
        )
        self.assertNotEqual(same_line_two_calls, documented_def, 'anchor missing for same_line_two_calls')
        self.assertTrue(any('install_future_a32' in e for e in coverage.check(same_line_two_calls)))

        # Two consecutive unannotated calls cannot share one standalone AZ04_EXEMPT line.
        standalone_shared = documented_def.replace(
            '    AZ04_EXEMPT("Batch 2: rekordbox USB browse on-demand awaiting AZ04 RE")\n    install_rekordbox_browse(base, page);\n',
            '    AZ04_EXEMPT("Batch 2: rekordbox USB browse on-demand awaiting AZ04 RE")\n    install_rekordbox_browse(base, page);\n    install_future_a32(base, page);\n',
            1,
        )
        self.assertNotEqual(standalone_shared, documented_def, 'anchor missing for standalone_shared')
        self.assertTrue(any('install_future_a32' in e for e in coverage.check(standalone_shared)))

        # Multiple HF_* entries on one line in az04_exempt_features[] cannot share a single AZ04_EXEMPT.
        shared_feature_exempt = source.replace(
            '    HF_REVERB_TAIL, AZ04_EXEMPT("Batch 2: Reverb tail hook awaiting AZ04 RE")\n',
            '    HF_REVERB_TAIL, HF_AUX_INPUT, AZ04_EXEMPT("shared")\n',
            1,
        )
        self.assertNotEqual(shared_feature_exempt, source, 'anchor missing for shared_feature_exempt')
        self.assertTrue(coverage.check(shared_feature_exempt))

        # An unannotated A32-only installer definition on the line after an inline-exempt definition must fail.
        def_borrow = source.replace(
            '#endif\n\n// The sender\'s prologue',
            '#endif\n\n#if !defined(__aarch64__)\nAZ04_EXEMPT("Batch 2: first") static void install_first_a32(uintptr_t b, size_t p) { (void)b; (void)p; }\nstatic void install_second_a32(uintptr_t b, size_t p) { (void)b; (void)p; }\n#endif\n\n// The sender\'s prologue',
            1,
        )
        self.assertNotEqual(def_borrow, source, 'anchor missing for def_borrow')
        self.assertTrue(any('install_second_a32' in e for e in coverage.check(def_borrow)))

    def test_az04_build_descriptor_completeness(self):
        source = (ROOT / 'tools/ui-mod/engine_ui_hook.c').read_text()
        self.assertEqual(coverage.check(source), [])

        cases = [
            ('remove .bt_send', '     .bt_send = 0x102eb20,\n', '', 'bt_send'),
            ('zero .bt_send', '     .bt_send = 0x102eb20,\n', '     .bt_send = 0,\n', 'bt_send'),
            ('remove .tidal_sso', '     .tidal_sso_slot = 0x2d52870, .tidal_sso_getter = 0x713b80,\n', '', ('tidal_sso_slot', 'tidal_sso_getter')),
            ('remove .shadow_owner_slot', '.shadow_owner_slot = 0x2d27a10, ', '', 'shadow_owner_slot'),
            ('remove .sqlite_opendb', '.sqlite_opendb = 0x1b8b060,\n', '', 'sqlite_opendb'),
            ('empty .sqlite_prefix[1]',
             '.sqlite_prefix = {"", "\\x3f\\x23\\x03\\xd5\\xff\\x43\\x02\\xd1\\xfd\\x7b\\x03\\xa9\\xfd\\xc3\\x00\\x91", ""},',
             '.sqlite_prefix = {"", "", ""},',
             'sqlite_prefix'),
            ('zero .dsp slot', '             0xff08c0, 0xfda420, 0xd30e68},', '             0xff08c0, 0xfda420, 0},', 'dsp'),
            ('short .dsp array', '             0xff08c0, 0xfda420, 0xd30e68},', '             0xff08c0, 0xfda420},', 'dsp'),
        ]
        for label, old, new, expected_fields in cases:
            expected = (expected_fields,) if isinstance(expected_fields, str) else expected_fields
            with self.subTest(label=label):
                mutated = source.replace(old, new, 1)
                self.assertNotEqual(mutated, source, f'anchor missing for {label}')
                errs = coverage.check(mutated)
                for field in expected:
                    self.assertTrue(any(field in e for e in errs), (label, field, errs))

        # A new build->field read in a shared/A64 installer must also be required on 5.3.0-az04.
        future_field = source.replace(
            '    uint32_t bt_send;\n',
            '    uint32_t bt_send;\n    uint32_t future_a64_hook;\n',
            1,
        ).replace(
            '    uintptr_t entry = base + build->bt_send;\n',
            '    uintptr_t entry = base + build->bt_send;\n    if (!build->future_a64_hook) return;\n',
            1,
        )
        self.assertNotEqual(future_field, source, 'anchor missing for future_field')
        errs = coverage.check(future_field)
        self.assertTrue(any('future_a64_hook' in e for e in errs), errs)

    def test_az04_exempt_stays_within_preprocessor_branch(self):
        source = (ROOT / 'tools/ui-mod/engine_ui_hook.c').read_text()

        # Reviewer case: AZ04_EXEMPT alone in #if defined(__aarch64__) must not cover #else installer.
        opposite_arm = source.replace(
            '#endif\n\n// The sender\'s prologue',
            '#endif\n\n#if defined(__aarch64__)\nAZ04_EXEMPT("Batch 2: A64 annotation")\n#else\nstatic void install_review_opposite(void) {}\n#endif\n\n// The sender\'s prologue',
            1,
        )
        self.assertNotEqual(opposite_arm, source, 'anchor missing for opposite_arm')
        self.assertTrue(any('install_review_opposite' in e for e in coverage.check(opposite_arm)))

        # Annotation inside a closed #if 0 ... #endif arm must not cover the installer after #endif.
        after_endif = source.replace(
            '#endif\n\n// The sender\'s prologue',
            '#endif\n\n#if !defined(__aarch64__)\n#if 0\nAZ04_EXEMPT("Batch 2: disabled arm")\n#endif\nstatic void install_after_endif(uintptr_t b, size_t p) { (void)b; (void)p; }\n#endif\n\n// The sender\'s prologue',
            1,
        )
        self.assertNotEqual(after_endif, source, 'anchor missing for after_endif')
        self.assertTrue(any('install_after_endif' in e for e in coverage.check(after_endif)))

        # Annotation outside #if !defined(__aarch64__) immediately above #if must not cross into the block.
        outside_guard = source.replace(
            '#endif\n\n// The sender\'s prologue',
            '#endif\n\nAZ04_EXEMPT("Batch 2: outside guard")\n#if !defined(__aarch64__)\nstatic void install_inside_guard(uintptr_t b, size_t p) { (void)b; (void)p; }\n#endif\n\n// The sender\'s prologue',
            1,
        )
        self.assertNotEqual(outside_guard, source, 'anchor missing for outside_guard')
        self.assertTrue(any('install_inside_guard' in e for e in coverage.check(outside_guard)))

        # Inside edit_engine(), an annotation in another #if/#else branch must not cover a following A32-only call.
        documented_def = source.replace(
            '#endif\n\n// The sender\'s prologue',
            '#endif\n\n#if !defined(__aarch64__)\nAZ04_EXEMPT("Batch 2: planned AZ04 port")\nstatic void install_future_a32(uintptr_t base, size_t page) { (void)base; (void)page; }\n#endif\n\n// The sender\'s prologue',
            1,
        )
        call_opposite = documented_def.replace(
            '#if !defined(__aarch64__)\n    AZ04_EXEMPT',
            '#if !defined(__aarch64__)\n#if defined(OTHER_BRANCH)\n    AZ04_EXEMPT("Batch 2: other branch")\n#else\n    install_future_a32(base, page);\n#endif\n    AZ04_EXEMPT',
            1,
        )
        self.assertNotEqual(call_opposite, documented_def, 'anchor missing for call_opposite')
        self.assertTrue(any('install_future_a32' in e for e in coverage.check(call_opposite)))

        # Nonconditional directives (#pragma, #define, multi-line #define, macro-body comments, %: digraph) are also barriers for AZ04_EXEMPT.
        for label, directive, fn_name in (
            ('pragma barrier', '#pragma GCC diagnostic push', 'install_after_pragma'),
            ('define barrier', '#define REVIEW_TAG 1', 'install_after_define'),
            ('multiline define barrier', '#define REVIEW_MULTI \\\n    1', 'install_after_multiline_define'),
            ('digraph define barrier', '%:define REVIEW_MACRO 1', 'install_review_digraph'),
        ):
            with self.subTest(label=label):
                mutated = source.replace(
                    '#endif\n\n// The sender\'s prologue',
                    f'#endif\n\n#if !defined(__aarch64__)\nAZ04_EXEMPT("Batch 2: before directive")\n{directive}\nstatic void {fn_name}(uintptr_t b, size_t p) {{ (void)b; (void)p; }}\n#endif\n\n// The sender\'s prologue',
                    1,
                )
                self.assertNotEqual(mutated, source, f'anchor missing for {label}')
                self.assertTrue(any(fn_name in e for e in coverage.check(mutated)), label)

        for label, snippet, fn_name in (
            ('define block-comment backslash barrier', '#define REVIEW_MACRO /* comment \\\n*/ AZ04_EXEMPT("Review marker")\nstatic void install_review_pp(void) {}', 'install_review_pp'),
            ('define multiline block-comment barrier', '#define REVIEW_MACRO /* comment\n*/ AZ04_EXEMPT("Review marker")\nstatic void install_review_pp_nobackslash(void) {}', 'install_review_pp_nobackslash'),
        ):
            with self.subTest(label=label):
                mutated = source.replace(
                    '#endif\n\n// The sender\'s prologue',
                    f'#endif\n\n#if !defined(__aarch64__)\n{snippet}\n#endif\n\n// The sender\'s prologue',
                    1,
                )
                self.assertNotEqual(mutated, source, f'anchor missing for {label}')
                self.assertTrue(any(fn_name in e for e in coverage.check(mutated)), label)

        for label, body_prefix in (
            ('call macro continuation barrier', '#define REVIEW_CALL_TAG \\\n    AZ04_EXEMPT("Batch 2: inside macro body")'),
            ('call block-comment macro barrier', '#define REVIEW_CALL_TAG /* comment \\\n*/ AZ04_EXEMPT("Batch 2: inside macro body")'),
        ):
            with self.subTest(label=label):
                call_barrier = documented_def.replace(
                    '#if !defined(__aarch64__)\n    AZ04_EXEMPT',
                    f'#if !defined(__aarch64__)\n{body_prefix}\n    install_future_a32(base, page);\n    AZ04_EXEMPT',
                    1,
                )
                self.assertNotEqual(call_barrier, documented_def, f'anchor missing for {label}')
                self.assertTrue(any('install_future_a32' in e for e in coverage.check(call_barrier)), label)
