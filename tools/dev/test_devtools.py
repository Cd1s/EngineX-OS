"""Self-test of tools/dev: path map, changed-test detection, verdicts, runners, stop cleanup."""
import io, json, os, shutil, subprocess, sys, tempfile, textwrap, time, unittest
from contextlib import redirect_stdout

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import devtest, testmap  # noqa: E402

GO = shutil.which('go')


def write(root, rel, text):
    path = os.path.join(root, rel)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, 'w') as f:
        f.write(textwrap.dedent(text))


def jtext(path):
    with open(path) as f:
        return f.read()


def jload(path):
    with open(path) as f:
        return json.load(f)


def jdump(obj, path):
    with open(path, 'w') as f:
        json.dump(obj, f)


REPO = os.path.normpath(os.path.join(HERE, '..', '..'))


def git_repo(root):
    def g(*a):
        subprocess.run(['git', '-C', root, '-c', 'user.name=t', '-c', 'user.email=t@example.invalid', *a], check=True, capture_output=True)
    g('init', '-q')
    return g


class MapTest(unittest.TestCase):
    """The path-to-tests map on this repository."""
    def plan(self, *paths):
        return testmap.map_paths(list(paths), REPO)

    def test_go_files_map_to_their_package_and_only_code_brings_importers(self):
        p = self.plan('pkg/webui/shadow.go')
        self.assertEqual(p.go_code, {'pkg/webui'})
        self.assertIn('pkg/webui', p.go)
        t = self.plan('pkg/webui/server_test.go', 'pkg/bridge/testdata/issue328_engine_rows.json')
        self.assertEqual(t.go_code, set(), 'a _test.go file or testdata changes no other package')
        self.assertEqual(t.go, {'pkg/webui', 'pkg/bridge'})
        self.assertTrue(self.plan('go.mod').go_all)

    def test_go_importers_come_from_the_static_closure(self):
        import gotree
        closure = gotree.static_closure(REPO, ['pkg/webui'])
        self.assertIn('pkg/webui', closure)
        self.assertIn('cmd/enginex-suite', closure)
        self.assertNotIn('pkg/bridge', closure, 'pkg/bridge does not import pkg/webui')

    def test_pack_generator_and_its_test_run_the_whole_test_ui_mod(self):
        for path in ('tools/ui-mod/build_packs.py', 'tools/az01-packer/test_ui_mod.py', 'tools/ui-mod/i18n.tsv'):
            self.assertIn(('tools/az01-packer', 'test_ui_mod.py'), self.plan(path).py, path)
        # A file spec, never a class or a test id: pyrun runs every test of the file.
        self.assertTrue(all(f.endswith('.py') and '.' not in f[:-3] for _, f in self.plan('tools/ui-mod/build_packs.py').py))

    def test_python_modules_their_tests_and_catch_alls(self):
        self.assertIn(('tools/az01-packer', 'test_az01_packer.py'), self.plan('tools/az01-packer/az01_packer.py').py)
        self.assertEqual(self.plan('tools/az01-packer/test_docs_audit.py').py, {('tools/az01-packer', 'test_docs_audit.py')})
        self.assertEqual(self.plan('tools/stems/score.py').py, {('tools/stems', 'test_score.py'), ('tools/stems', 'test_worker.py')})

    def test_c_changes_run_every_including_test_with_the_checklist_variants(self):
        dsp = self.plan('tools/ui-mod/dsp_smooth.h')
        self.assertTrue({'tools/ui-mod/test_dsp_smooth.c', 'tools/ui-mod/test_rt_budget.c'} <= dsp.c)
        self.assertIn('tools/ui-mod/test_fx_selector.sh', dsp.sh, 'its loop names test_dsp_smooth')
        hook = self.plan('tools/ui-mod/engine_ui_hook.c')
        self.assertTrue({'tools/ui-mod/test_stems_feature.c', 'tools/ui-mod/test_az04_hook.c', 'tools/ui-mod/test_rt_callback.c'} <= hook.c)
        self.assertIn(testmap.OVERLAY, hook.sh, 'the overlay test rebuilds the shipped hook')
        self.assertIn(('tools/az01-packer', 'test_ui_mod.py'), hook.py)
        self.assertIn('tools/edisksd-hook/test_edisksd_hook.c', self.plan('tools/edisksd-hook/edisksd_hook.c').c)
        self.assertIn(testmap.OVERLAY, self.plan('tools/edisksd-hook/edisksd_hook.c').sh)

    def test_a_header_reached_only_through_the_hook_selects_the_hook_tests(self):
        with tempfile.TemporaryDirectory() as root:
            g = git_repo(root)
            write(root, 'tools/ui-mod/stage.h', 'int stage(void);\n')
            write(root, 'tools/ui-mod/engine_ui_hook.c', '#include "stage.h"\n')
            write(root, 'tools/ui-mod/test_stems_feature.c', '#include "engine_ui_hook.c"\n')
            write(root, 'tools/ui-mod/test_other.c', 'int main(void) { return 0; }\n')
            write(root, 'tools/ui-mod/test_built.c', '#include "stage.h"\n')
            write(root, 'tools/az01-packer/test_built.py', 'import subprocess\nsubprocess.run(["cc", "tools/ui-mod/test_built.c", "-lsqlite3"])\n')
            write(root, 'tests/test_rootfs_overlay.sh', 'sh "$ROOT/tools/ui-mod/build.sh"\n')
            g('add', '-A'); g('commit', '-qm', 'base')
            plan = testmap.map_paths(['tools/ui-mod/stage.h'], root)
            self.assertEqual(plan.c, {'tools/ui-mod/test_stems_feature.c', 'tools/ui-mod/test_other.c'}, 'every C test the runner builds')
            self.assertIn(('tools/az01-packer', 'test_built.py'), plan.py, 'test_built.c runs through its own builder')
            self.assertEqual(plan.sh, {testmap.OVERLAY})

    def test_sandbox_helpers_name_their_sandbox_as_manual(self):
        plan = self.plan('tools/az01-packer/console_link_stub.py')
        self.assertIn('tools/az01-packer/test_console_link_sandbox.sh', plan.manual)
        self.assertIn('heavy lock', plan.manual['tools/az01-packer/test_console_link_sandbox.sh']['how'])

    def test_overlay_and_dev_tools(self):
        self.assertIn(testmap.OVERLAY, self.plan('rootfs-overlay/usr/local/bin/x').sh)
        self.assertIn(('tools/dev', 'test_devtools.py'), self.plan('tools/dev/pyrun.py').py)
        self.assertIn('tools/dev/test_integrate.sh', self.plan('tools/dev/integrate.sh').sh)

    def test_a_file_no_test_names_is_listed_as_unmapped(self):
        plan = self.plan('no-such-dir/nothing-names-this.bin')
        self.assertTrue(plan.empty())
        self.assertEqual(plan.unmapped, ['no-such-dir/nothing-names-this.bin'])

    def test_every_bash_test_is_classified_once(self):
        listed = list(testmap.SH_TESTS['auto']) + list(testmap.SH_TESTS['manual'])
        self.assertEqual(len(listed), len(set(listed)))
        idx = testmap.index(REPO)
        scripts = {f for f in idx.files if f.endswith('.sh') and testmap.is_test_file(f)}
        self.assertEqual(sorted(scripts - set(listed)), [], 'add them to tools/dev/sh-tests.json (auto or manual)')
        self.assertEqual(sorted(f for f in listed if f not in idx.fileset), [], 'listed files must exist')

    def test_c_recipes_cover_every_c_test_with_the_checklist_variants(self):
        import crun
        idx = testmap.index(REPO)
        srcs = sorted(f for f in idx.files if os.path.dirname(f) in testmap.C_TEST_DIRS and os.path.basename(f).startswith('test_') and f.endswith('.c')
                      and not (testmap.C_TESTS['tests'].get(f) or {}).get('via'))
        self.assertEqual(testmap.c_drivers(idx, 'tools/ui-mod/test_rekordbox_rows.c'), ['tools/az01-packer/test_rekordbox_rows.py'])
        self.assertEqual(testmap.c_drivers(idx, 'tools/ui-mod/test_pioneer_selector.c'), ['tools/ui-mod/test_fx_selector.sh'])
        self.assertEqual(testmap.c_drivers(idx, 'tools/ui-mod/test_dsp_smooth.c'), ['tools/ui-mod/test_fx_selector.sh'])
        self.assertIn('tools/ui-mod/test_dsp_smooth.c', testmap.full_plan(REPO).c, 'an explicit recipe wins over a driver')
        self.assertNotIn('tools/ui-mod/test_pioneer_selector.c', testmap.full_plan(REPO).c, 'built by test_fx_selector.sh')
        jobs, bad = crun.jobs_for(srcs)
        self.assertEqual(bad, [])
        variants = {}
        for j in jobs:
            variants.setdefault(j['src'], {})[j['variant']] = j
        self.assertEqual(set(variants['tools/ui-mod/test_dsp_smooth.c']), {'plain', 'asan-ubsan', 'tsan'})
        self.assertEqual(set(variants['tools/ui-mod/test_rt_budget.c']), {'native', 'armhf'})
        for v in variants['tools/ui-mod/test_rt_callback.c'].values():
            self.assertIn('{out} selftest', v['runs'])
        self.assertIn('{out} {e504} {e530} {esl4}', variants['tools/ui-mod/test_stems_feature.c']['armhf']['runs'])
        self.assertIn('{eaz04}', variants['tools/ui-mod/test_az04_hook.c']['native']['runs'][0])
        for v in variants['tools/edisksd-hook/test_edisksd_hook.c'].values():
            self.assertTrue(all(r.startswith('root: unshare -m --propagation private ') for r in v['runs'] if '--mounts' in r),
                            '--mounts only inside a private mount namespace')
        self.assertEqual({k for k, e in testmap.C_TESTS['tests'].items() if e.get('checklist')},
                         {'tools/ui-mod/test_dsp_smooth.c', 'tools/ui-mod/test_stems_feature.c', 'tools/edisksd-hook/test_edisksd_hook.c',
                          'tools/ui-mod/test_az04_hook.c', 'tools/ui-mod/test_rt_budget.c', 'tools/ui-mod/test_rt_callback.c'})


class ChangedTestsTest(unittest.TestCase):
    def test_added_and_edited_tests_are_found(self):
        with tempfile.TemporaryDirectory() as root:
            g = git_repo(root)
            write(root, 'p/a_test.go', """\
                package p

                func TestOld(t *testing.T) {
                	x := 1
                }

                func TestEdited(t *testing.T) {
                	y := 1
                }
                """)
            write(root, 'tools/test_a.py', """\
                import unittest
                class A(unittest.TestCase):
                    def test_old(self):
                        pass
                    def test_edited(self):
                        pass
                """)
            g('add', '-A'); g('commit', '-qm', 'base')
            write(root, 'p/a_test.go', jtext(root + '/p/a_test.go').replace('y := 1', 'y := 2') + '\nfunc TestNew(t *testing.T) {\n}\n')
            write(root, 'tools/test_a.py', jtext(root + '/tools/test_a.py').replace('    def test_edited(self):\n        pass', '    def test_edited(self):\n        pass  # changed') )
            write(root, 'tools/test_b.py', "import unittest\nclass B(unittest.TestCase):\n    def test_b(self):\n        pass\n")
            write(root, 'p/oneline_test.go', 'package p\n\nfunc TestOneLine(t *testing.T) { t.Skip("lab") }\n')
            write(root, 'tools/ui-mod/test_new.c', 'int main(void) { return 0; }\n')
            write(root, 'tests/test_new.sh', 'true\n')
            found = testmap.changed_tests(root, 'HEAD', ['p/a_test.go', 'tools/test_a.py', 'tools/test_b.py', 'p/oneline_test.go',
                                                         'tools/ui-mod/test_new.c', 'tests/test_new.sh'])
            self.assertEqual(found, {('go', 'p', 'TestEdited'), ('go', 'p', 'TestNew'), ('py', 'tools', 'test_a.A.test_edited'),
                                     ('py', 'tools', 'test_b.B.test_b'), ('go', 'p', 'TestOneLine'),
                                     ('c', 'tools/ui-mod/test_new.c', '*'), ('sh', 'tests/test_new.sh', '*')})


def args(**kw):
    """make_plan's arguments, as test.sh passes them."""
    d = dict(full=False, no_cache=False, rerun=False, base=None, into='x', require_samba=False)
    d.update(kw)
    return type('A', (), d)()


class FailClosedPlanTest(unittest.TestCase):
    """A change set the runner cannot know, or cannot map, never returns PASS (review 1)."""
    def repo(self, root):
        g = git_repo(root)
        write(root, 'tools/az01-packer/test_known.py', 'import unittest\nclass K(unittest.TestCase):\n    def test_k(self): pass\n')
        write(root, 'notes/known.txt', 'named by nothing\n')
        g('add', '-A'); g('commit', '-qm', 'base')
        return g

    def test_a_git_error_or_an_unknown_base_stops_the_plan(self):
        with tempfile.TemporaryDirectory() as root:
            self.repo(root)
            with self.assertRaises(devtest.PlanError):
                devtest.make_plan(args(base='no-such-ref'), root)
            with self.assertRaises(devtest.PlanError):
                devtest.make_plan(args(into='no-such-branch'), root)  # no origin/<into>: no merge base
        with tempfile.TemporaryDirectory() as plain:  # not a repository at all
            with self.assertRaises(devtest.PlanError):
                devtest.make_plan(args(base='HEAD'), plain)

    def test_no_changed_file_runs_everything(self):
        with tempfile.TemporaryDirectory() as root:
            self.repo(root)
            plan = devtest.make_plan(args(base='HEAD'), root)
            self.assertEqual((plan['mode'], plan['fallback']), ('full', 'no changed file'))
            self.assertTrue(plan['go_all'])

    def test_an_unknown_file_deletion_or_rename_runs_everything(self):
        with tempfile.TemporaryDirectory() as root:
            g = self.repo(root)
            write(root, 'misc/unknown.bin', 'x')  # an unknown new file
            plan = devtest.make_plan(args(base='HEAD'), root)
            self.assertEqual(plan['mode'], 'full'); self.assertIn('misc/unknown.bin', plan['unmapped'])
            os.remove(os.path.join(root, 'misc/unknown.bin'))
            os.remove(os.path.join(root, 'notes/known.txt'))  # an unknown deletion
            plan = devtest.make_plan(args(base='HEAD'), root)
            self.assertEqual(plan['mode'], 'full'); self.assertEqual(plan['deleted'], ['notes/known.txt'])
            g('add', '-A'); g('commit', '-qm', 'deleted')
            g('mv', 'tools/az01-packer/test_known.py', 'misc/renamed.py')  # a rename with unknown paths
            plan = devtest.make_plan(args(base='HEAD'), root)
            self.assertEqual(plan['mode'], 'full')
            self.assertEqual(set(plan['unmapped']), {'tools/az01-packer/test_known.py', 'misc/renamed.py'})

    def test_the_full_fallback_keeps_every_narrow_target_sandboxes_included(self):
        # Review round 2: a sandbox helper selected its sandbox; adding an unmapped file made the plan full and lost it.
        with tempfile.TemporaryDirectory() as root:
            g = git_repo(root)
            write(root, 'tools/az01-packer/test_known.py', 'import unittest\nclass K(unittest.TestCase):\n    def test_k(self): pass\n')
            g('add', '-A'); g('commit', '-qm', 'base')
            os.makedirs(os.path.join(root, 'tools/dev'), exist_ok=True)
            write(root, 'tools/az01-packer/console_link_stub.py', 'x = 1\n')
            write(root, 'tools/az01-packer/test_console_link_sandbox.sh', 'python3 tools/az01-packer/console_link_stub.py\n')
            narrow = devtest.make_plan(args(base='HEAD'), root)
            self.assertIn('tools/az01-packer/test_console_link_sandbox.sh', narrow['manual'])
            write(root, 'misc/unknown.bin', 'x')
            full = devtest.make_plan(args(base='HEAD'), root)
            self.assertEqual(full['mode'], 'full')
            self.assertIn('tools/az01-packer/test_console_link_sandbox.sh', full['manual'], 'the fallback is a superset')

    def test_a_selection_with_nothing_in_it_runs_everything(self):
        plan = testmap.map_paths(['no-such-dir/nothing-names-this.bin'], REPO)
        self.assertTrue(plan.empty())  # devtest turns this into a full run (test_an_unknown_file...)

    def test_a_c_change_runs_every_c_test(self):
        own, drivers = testmap.all_c_tests(testmap.index(REPO))
        plan = testmap.map_paths(['tools/ui-mod/master_limiter.h'], REPO)
        self.assertEqual(plan.c, own, 'includes found only through -I or macros would escape an include map')
        self.assertTrue(drivers <= plan.sh | {f'{d}/{f}' for d, f in plan.py})

class ReportTest(unittest.TestCase):
    """The verdict comes from the runners' results (one format); every changed test must have passed (review 1, 2)."""
    def report(self, plan_extra, res, accept=False):
        a = type('A', (), {'report': None, 'accept_incomplete': accept})()
        plan = {'mode': 'targeted', 'base': 'abc', 'changed': ['x'], 'changed_tests': [], 'manual': {}, 'unmapped': [],
                'c': [], 'sh': [], 'fallback': ''}
        plan.update(plan_extra)
        out = io.StringIO()
        with redirect_stdout(out):
            rc = devtest.report(a, plan, res, 'host', 0.0)
        return rc, out.getvalue()

    def res(self, *items, errors=()):
        r = {'results': [dict(zip(('kind', 'where', 'test', 'status', 'secs', 'reason'), i)) for i in items],
             'errors': list(errors), 'notes': []}
        return {'steps': [{'name': 'go test', 'secs': 1, 'ok': not errors and all(i[3] != 'fail' for i in items), 'note': '', 'tail': ''}],
                'runs': [r]}

    def test_a_changed_test_without_a_pass_is_incomplete(self):
        rc, text = self.report({'changed_tests': [['go', 'p', 'TestNew']]}, self.res(('go', 'p', 'TestOld', 'pass', 0, '')))
        self.assertEqual(rc, devtest.EXIT_INCOMPLETE)
        self.assertIn('go p TestNew: no result', text)
        rc, _ = self.report({'changed_tests': [['py', 'd', 'test_a.A.test_x']]}, self.res(('py', 'd', 'test_a.A.test_y', 'pass', 0, '')))
        self.assertEqual(rc, devtest.EXIT_INCOMPLETE)

    def test_a_changed_test_that_skipped_is_not_proven_an_unrelated_skip_is_listed(self):
        rc, text = self.report({'changed_tests': [['go', 'p', 'TestNew']]}, self.res(('go', 'p', 'TestNew', 'skip', 0, 'lab')))
        self.assertEqual(rc, devtest.EXIT_INCOMPLETE)
        self.assertIn('go p TestNew: skip', text)
        rc, text = self.report({'changed_tests': [['go', 'p', 'TestNew']]},
                               self.res(('go', 'p', 'TestNew', 'pass', 0, ''), ('go', 'p', 'TestOther', 'skip', 0, 'lab')))
        self.assertEqual(rc, 0)
        self.assertIn('skipped: 1', text)

    def test_a_runner_error_fails_the_run(self):
        rc, text = self.report({}, self.res(errors=['pkg/x: go test -list failed (exit 1): undefined: y']))
        self.assertEqual(rc, 1)
        self.assertIn('ERROR: pkg/x: go test -list failed', text)

    def test_manual_and_not_run_tests_are_incomplete_unless_accepted(self):
        plan = {'manual': {'tools/az01-packer/test_x_sandbox.sh': {'how': 'enginex-run sandbox -- bash x', 'why': 'Engine'}}}
        rc, text = self.report(plan, self.res(('go', 'p', 'T', 'pass', 0, '')))
        self.assertEqual(rc, devtest.EXIT_INCOMPLETE)
        self.assertIn('RESULT: INCOMPLETE', text)
        self.assertIn('enginex-run sandbox -- bash x', text)
        rc, text = self.report(plan, self.res(('go', 'p', 'T', 'pass', 0, '')), accept=True)
        self.assertEqual(rc, 0)
        self.assertIn('accepted as run by the lane', text)
        rc, text = self.report({}, self.res(('go', 'pkg/webui', 'TestSMBNASAPIAndRebootReload', 'notrun', 0, 'no smbpasswd')))
        self.assertEqual(rc, devtest.EXIT_INCOMPLETE)
        self.assertIn('TestSMBNASAPIAndRebootReload: did not run (no smbpasswd)', text)
        rc, _ = self.report({'changed_tests': [['go', 'pkg/webui', 'TestSMBNASAPIAndRebootReload']]},
                            self.res(('go', 'pkg/webui', 'TestSMBNASAPIAndRebootReload', 'notrun', 0, 'no smbpasswd')))
        self.assertEqual(rc, devtest.EXIT_INCOMPLETE, 'a changed Samba test must run before PASS')

    def test_a_test_requested_by_name_that_skipped_is_incomplete(self):
        # Review round 2: a Python test selected by name (a spec) that reported skip, outside changed_tests, gave PASS.
        one = {'py': [{'dir': 'd', 'specs': ['test_a.A.test_x']}]}
        rc, text = self.report(one, self.res(('py', 'd', 'test_a.A.test_x', 'skip', 0, 'no lab')))
        self.assertEqual(rc, devtest.EXIT_INCOMPLETE, 'a test requested by name that skipped is not a pass')
        self.assertIn('py d test_a.A.test_x: skip (no lab)', text)
        both = self.res(('py', 'd', 'test_a.A.test_x', 'skip', 0, 'no lab'), ('py', 'd', 'test_a.A.test_y', 'pass', 0, ''))
        self.assertEqual(self.report(one, both)[0], devtest.EXIT_INCOMPLETE, 'another test passing does not count for it')
        rc, text = self.report({'py': [{'dir': 'd', 'specs': ['test_a.py']}]}, self.res(('py', 'd', 'test_a.A.test_x', 'skip', 0, 'no lab')))
        self.assertEqual(rc, devtest.EXIT_INCOMPLETE, 'a selected file whose every test skipped proves nothing')
        self.assertIn('py d test_a.py: skip (no lab)', text)
        rc, text = self.report({'py': [{'dir': 'd', 'specs': ['test_a.py']}]}, both)
        self.assertEqual(rc, 0, 'one skip in a selected file that passed is listed with its reason')
        self.assertIn('no lab', text)
        go = self.res(('go', 'p', 'TestA', 'skip', 0, 'needs the lab'))
        go['runs'][0]['packages'] = {'p': 'tested', 'q': 'no-tests'}
        rc, text = self.report({}, go)
        self.assertEqual(rc, devtest.EXIT_INCOMPLETE, 'a selected Go package whose every test skipped proves nothing')
        self.assertIn('go p: skip (needs the lab)', text)
        self.assertNotIn('go q', text)
        smb = self.res(('go', 'p', 'TestSMB', 'skip', 0, 'no smbd'), ('go', 'p', 'TestOther', 'pass', 0, ''))
        smb['runs'][0]['packages'], smb['by_name'] = {'p': 'tested'}, [['go', 'p', 'TestSMB']]
        rc, text = self.report({}, smb)
        self.assertEqual(rc, devtest.EXIT_INCOMPLETE, 'a Samba test run by name (gorun --only) that skipped')
        self.assertIn('go p TestSMB: skip (no smbd)', text)
        rc, text = self.report({}, self.res(('py', 'd', 'test_missing', 'notrun', 0, 'no test matches it')))
        self.assertEqual(rc, devtest.EXIT_INCOMPLETE, 'a test requested by name that does not exist is never a pass')
        rc, text = self.report({'sh': ['tests/test_x.sh']}, self.res(('go', 'p', 'T', 'pass', 0, '')))
        self.assertEqual(rc, devtest.EXIT_INCOMPLETE, 'a selected bash test with no result')
        self.assertIn('sh tests/test_x.sh: no result', text)

    def test_a_remote_run_without_a_result_fails(self):
        res = {'steps': [{'name': 'remote run', 'secs': 1, 'ok': False, 'note': 'exit 1', 'tail': 'sync failed'}], 'runs': []}
        rc, text = self.report({}, res)
        self.assertEqual(rc, 1)
        self.assertIn('sync failed', text)


class LockAndCleanupTest(unittest.TestCase):
    """Never unlocked; nothing left after a stop (review 3)."""
    def test_a_host_without_enginex_run_runs_nothing(self):
        with tempfile.TemporaryDirectory() as tmp:
            mark = os.path.join(tmp, 'ran')
            cmd = devtest.remote_command('sandbox', f'touch {mark}')
            p = subprocess.run(['sh', '-c', cmd], env={'PATH': '/usr/bin:/bin'}, capture_output=True, text=True)
            self.assertEqual(p.returncode, 2)
            self.assertIn('enginex-run is missing', p.stderr)
            self.assertFalse(os.path.exists(mark))
            os.mkdir(os.path.join(tmp, 'bin'))
            write(tmp, 'bin/enginex-run', '#!/bin/sh\n[ "$1" = sandbox ] && [ "$2" = -- ] && shift 2 && exec "$@"\n')
            os.chmod(os.path.join(tmp, 'bin/enginex-run'), 0o755)
            p = subprocess.run(['sh', '-c', cmd], env={'PATH': os.path.join(tmp, 'bin') + ':/usr/bin:/bin'}, capture_output=True, text=True)
            self.assertEqual(p.returncode, 0, p.stderr)
            self.assertTrue(os.path.exists(mark))

    @unittest.skipUnless(os.uname().machine in ('aarch64', 'arm64'), 'exec runs only on the ARM build hosts')
    def test_a_stop_at_any_moment_leaves_no_scratch_lock_or_git(self):
        # Review round 2: a stop after the TMPDIR was made but before it was recorded left the TMPDIR and its lock.
        # Stop the run at the moment each piece appears (the TMPDIR, its lock, the staging .git) and after.
        import signal
        for when in ('start', 'tmpdir', 'git', 'index'):
            with tempfile.TemporaryDirectory() as tree:
                plan = {'mode': 'targeted', 'go': [], 'go_all': False, 'go_code': [], 'gofmt': [], 'py': [], 'c': [], 'sh': [],
                        'no_cache': False, 'split': {'python': {}, 'c': {'processes': 1}}}
                jdump(plan, os.path.join(tree, 'plan.json'))
                write(tree, 'file.txt', 'x\n')
                p = subprocess.Popen([sys.executable, os.path.join(HERE, 'devtest.py'), '--exec', 'plan.json'], cwd=tree,
                                     stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                scratch = f'/var/tmp/x{p.pid}'
                seen = {'start': lambda: True, 'tmpdir': lambda: os.path.isdir(scratch),
                        'git': lambda: os.path.isdir(os.path.join(tree, '.git')), 'index': lambda: os.path.exists(os.path.join(scratch, 'index'))}[when]
                for _ in range(5000):
                    if seen() or p.poll() is not None:
                        break
                    time.sleep(0.0005)
                p.send_signal(signal.SIGINT)
                p.communicate(timeout=30)
                left = [x for x in (scratch, os.path.join(tree, '.git')) if os.path.exists(x)]
                self.assertEqual(left, [], f'stopped at {when}: left {left}')

    INJECT = textwrap.dedent('''\
        import os, subprocess, sys
        sys.path.insert(0, sys.argv[1])
        import devtest
        real_mkdir = os.mkdir
        def mkdir(path, *a, **k):  # the interrupt lands the instant the scratch directory exists
            real_mkdir(path, *a, **k)
            if sys.argv[2] == 'scratch' and str(path).startswith('/var/tmp/'):
                open('made', 'w').write(str(path))
                raise KeyboardInterrupt
        os.mkdir = mkdir
        class Popen(subprocess.Popen):  # ... or the instant `git init` has made the staging repository
            def __init__(self, args, *a, **k):
                super().__init__(args, *a, **k)
                if sys.argv[2] == 'git' and list(args[:2]) == ['git', 'init']:
                    self.wait()
                    raise KeyboardInterrupt
        subprocess.Popen = Popen
        sys.exit(devtest.main(['--exec', 'plan.json']))
        ''')

    @unittest.skipUnless(os.uname().machine in ('aarch64', 'arm64'), 'exec runs only on the ARM build hosts')
    def test_an_interrupt_the_instant_a_directory_exists_leaves_nothing(self):
        # Review round 2: a Ctrl-C after the TMPDIR was made but before it was recorded left the TMPDIR and its lock.
        for when in ('scratch', 'git'):
            with tempfile.TemporaryDirectory() as tree:
                jdump({'mode': 'targeted', 'go': [], 'go_all': False, 'go_code': [], 'gofmt': [], 'py': [], 'c': [], 'sh': [],
                       'no_cache': False, 'split': {'python': {}, 'c': {'processes': 1}}}, os.path.join(tree, 'plan.json'))
                p = subprocess.run([sys.executable, '-c', self.INJECT, HERE, when], cwd=tree, capture_output=True, text=True, timeout=60)
                self.assertIn('KeyboardInterrupt', p.stderr, f'{when}: the interrupt never came')
                made = jtext(os.path.join(tree, 'made')) if os.path.exists(os.path.join(tree, 'made')) else None
                left = [x for x in ([made, made + '.lock'] if made else []) + [os.path.join(tree, '.git')] if os.path.exists(x)]
                for x in left:  # the run's residue goes, whatever the verdict
                    self.addCleanup(lambda x=x: shutil.rmtree(x) if os.path.isdir(x) else os.unlink(x))
                self.assertEqual(left, [], f'interrupted at {when}: left {left}')


class SambaTest(unittest.TestCase):
    def test_tests_that_start_the_fixture_directly_or_through_a_helper_are_found(self):
        import gotree
        with tempfile.TemporaryDirectory() as root:
            write(root, 'go.mod', 'module example.test/m\n\ngo 1.21\n')
            write(root, 'internal/testsmb/server.go', 'package testsmb\n')
            write(root, 'p/p_test.go', """\
                package p

                import (
                	"testing"

                	fix "example.test/m/internal/testsmb"
                )

                func startShare(t *testing.T) { fix.Start(t) }

                func TestDirect(t *testing.T) { s := fix.Start(t); _ = s }
                func TestThroughHelper(t *testing.T) { startShare(t) }
                func TestPlain(t *testing.T) {}
                """)
            write(root, 'q/q_test.go', 'package q\n\nimport "testing"\n\nfunc TestQ(t *testing.T) {}\n')
            self.assertEqual(gotree.smb_tests(root, ['p', 'q']), [['p', 'TestDirect'], ['p', 'TestThroughHelper']])

    def test_the_repository_fixture_tests_are_found(self):
        import gotree
        found = gotree.smb_tests(REPO, ['internal/smbclient', 'pkg/providers', 'pkg/webui'])
        self.assertIn(['internal/smbclient', 'TestSMBIdleConnectionSurvivesShortCachePause'], found)
        self.assertIn(['pkg/webui', 'TestSMBRekordboxWrongPasswordNotice'], found)
        self.assertEqual({d for d, _ in found}, {'internal/smbclient', 'pkg/providers', 'pkg/webui'})

    def test_they_run_in_a_private_uts_namespace_named_localhost(self):
        cmd = devtest.uts_localhost(['python3', 'gorun.py', './p'])
        self.assertEqual(cmd[:3], ['unshare', '--uts', 'sh'])
        self.assertIn('hostname localhost', cmd[4])
        self.assertEqual(cmd[-3:], ['python3', 'gorun.py', './p'])


class SpawnTest(unittest.TestCase):
    def test_a_timed_out_step_takes_its_workers_with_it(self):
        with tempfile.TemporaryDirectory() as tmp:
            mark = os.path.join(tmp, 'pid')
            step = devtest.Step('x')
            rc = devtest.spawn(step, ['bash', '-c', f'sleep 300 & echo $! > {mark}; wait'], dict(os.environ), timeout=1)
            self.assertEqual(rc, 124)
            self.assertIn('timed out after 1s', step.out)
            pid = int(open(mark).read())
            time.sleep(0.3)
            with self.assertRaises(ProcessLookupError):
                os.kill(pid, 0)


class NoRerunTest(unittest.TestCase):
    def test_a_failed_test_is_never_rerun(self):
        """A failure stands: no runner retries a test, and no result counts a later pass as "flaky"."""
        import results
        with tempfile.TemporaryDirectory() as tmp:
            for tool, args in (('devtest.py', []), ('pyrun.py', ['.']), ('gorun.py', ['./...']), ('crun.py', [])):
                p = subprocess.run([sys.executable, '-I', os.path.join(HERE, tool), *args, '--rerun', '1'], cwd=tmp,
                                   capture_output=True, text=True, timeout=60)
                self.assertEqual((tool, p.returncode), (tool, 2), p.stdout[-500:])
                self.assertIn('unrecognized arguments: --rerun', p.stderr, tool)
        self.assertNotIn('flaky', results.Results().as_dict())


def ids(res, status=None):
    return sorted(r['where'] + ' ' + r['test'] if r['kind'] == 'go' else r['test'] for r in res['results'] if status in (None, r['status']))


class PyrunTest(unittest.TestCase):
    def make(self, root):
        write(root, 'test_one.py', """\
            import unittest
            class First(unittest.TestCase):
                def test_a(self): pass
                def test_b(self): pass
                @unittest.skip('no lab')
                def test_skipped(self): pass
            class Second(unittest.TestCase):
                def test_tmp(self):
                    import os
                    self.assertTrue(os.environ['TMPDIR'].startswith(os.environ['PYRUN_BASE']), os.environ['TMPDIR'])
                    self.assertLess(len(os.environ['TMPDIR']) - len(os.environ['PYRUN_BASE']), 20, 'short: unix sockets live there')
            """)
        write(root, 'test_two.py', """\
            import unittest
            class Third(unittest.TestCase):
                def test_c(self): pass
            """)

    def pyrun(self, root, *args, tmpdir=None, env=None):
        e = dict(os.environ, TMPDIR=tmpdir or tempfile.gettempdir(), PYRUN_BASE=tmpdir or tempfile.gettempdir(), **(env or {}))
        return subprocess.run([sys.executable, os.path.join(HERE, 'pyrun.py'), root, *args], capture_output=True, text=True, env=e)

    def test_every_discovered_test_reports_one_result(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as tmp:
            self.make(root)
            serial = subprocess.run([sys.executable, '-m', 'unittest', 'discover', '-s', root], capture_output=True, text=True)
            n = int(serial.stderr.split('Ran ')[1].split()[0])
            out = os.path.join(tmp, 'r.json')
            p = self.pyrun(root, '--json', out, tmpdir=tmp)
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
            res = jload(out)
            self.assertEqual(len(res['results']), n)
            self.assertEqual(ids(res, 'skip'), ['test_one.First.test_skipped'])
            self.assertEqual(os.listdir(tmp), ['r.json'], 'worker TMPDIRs must be removed')

    def test_defer_specs_and_a_spec_that_matches_nothing(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as tmp:
            self.make(root)
            defer = os.path.join(tmp, 'defer.txt'); write(tmp, 'defer.txt', 'test_one.First.test_a  # elsewhere\ntest_two\n')
            out = os.path.join(tmp, 'r.json')
            self.assertEqual(self.pyrun(root, '--defer', defer, '--json', out, tmpdir=tmp).returncode, 0)
            self.assertNotIn('test_one.First.test_a', ids(jload(out)))
            self.assertNotIn('test_two.Third.test_c', ids(jload(out)))
            self.assertEqual(self.pyrun(root, 'test_two.py', '--json', out, tmpdir=tmp).returncode, 0)
            self.assertEqual(ids(jload(out)), ['test_two.Third.test_c'])
            p = self.pyrun(root, 'test_three.py', '--json', out, tmpdir=tmp)
            self.assertEqual(p.returncode, 2, 'a selected file with no test must not pass as zero tests')
            self.assertEqual(ids(jload(out), 'notrun'), ['test_three'])

    def test_only_modules_split_json_lists_are_split(self):
        # Review round 2: a heuristic split a module whose test leaked state despite an unrelated addCleanup. Now only
        # an explicit --split (from split.json) runs a module in more than one process; everything else runs serially.
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as tmp:
            write(root, 'test_leak.py', textwrap.dedent("""\
                import unittest
                STATE = {}
                class A(unittest.TestCase):
                    def test_a(self):
                        self.addCleanup(lambda: None)
                        STATE['seen'] = True
                class B(unittest.TestCase):
                    def test_b(self):
                        self.assertTrue(STATE.get('seen'))
                """))
            out = os.path.join(tmp, 'r.json')
            p = self.pyrun(root, '--json', out, tmpdir=tmp)
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
            self.assertEqual(jload(out)['processes'], 1)
            p = self.pyrun(root, '--split', 'test_leak=2', '--json', out, tmpdir=tmp)
            self.assertEqual(jload(out)['processes'], 2, 'split.json is the only way to split')

    def test_split_json_entries_carry_their_equality_evidence(self):
        cfg = jload(os.path.join(HERE, 'split.json'))
        self.assertEqual(sorted(cfg['python']), ['tools/az01-packer/test_ui_mod.py'])
        for name, entry in list(cfg['python'].items()) + [('c', cfg['c'])]:
            self.assertGreater(entry['processes'], 1, name)
            self.assertRegex(entry['evidence'], r'20\d\d-\d\d-\d\d, .*results\.py --same: SAME, sha256 ([0-9a-f]{16}) and \1$',
                             f'{name}: an entry needs its recorded serial-vs-split equality run (date, host, tree, both hashes)')

    def test_failure_exits_nonzero_and_prints_output(self):
        with tempfile.TemporaryDirectory() as root:
            write(root, 'test_bad.py', "import unittest\nclass Bad(unittest.TestCase):\n    def test_x(self): self.fail('boom')\n")
            p = self.pyrun(root)
            self.assertEqual(p.returncode, 1)
            self.assertIn('boom', p.stderr)

    def test_import_error_fails(self):
        with tempfile.TemporaryDirectory() as root:
            write(root, 'test_broken.py', "import no_such_module_anywhere\n")
            self.assertEqual(self.pyrun(root).returncode, 1)


@unittest.skipUnless(GO, 'go toolchain needed')
class GorunTest(unittest.TestCase):
    def module(self, root):
        write(root, 'go.mod', 'module example.test/m\n\ngo 1.21\n')
        write(root, 'a/a.go', 'package a\n')
        write(root, 'a/a_test.go', """\
            package a

            import (
            	"testing"
            	"time"
            )

            func TestOne(t *testing.T)   { time.Sleep(50 * time.Millisecond) }
            func TestTwo(t *testing.T)   { t.Run("sub", func(t *testing.T) {}) }
            func TestThree(t *testing.T) { t.Skip("needs the lab") }
            func TestFour(t *testing.T)  {}
            func TestFive(t *testing.T)  {}
            func TestTwoAndAHalf(t *testing.T) {}
            """)
        write(root, 'b/b.go', 'package b\n')
        write(root, 'b/b_test.go', 'package b\n\nimport "testing"\n\nfunc TestB(t *testing.T) {}\n')

    def gorun(self, root, *args, env=None):
        e = dict(os.environ, TMPDIR=tempfile.gettempdir(), **(env or {}))
        return subprocess.run([sys.executable, os.path.join(HERE, 'gorun.py'), '--no-race', '--no-cache', *args], cwd=root,
                              capture_output=True, text=True, env=e)

    def test_skip_and_only_account_for_every_test(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as tmp:
            self.module(root)
            skip = os.path.join(tmp, 'skip.txt'); write(tmp, 'skip.txt', 'a TestThree\nb TestB\n')
            out = os.path.join(tmp, 'd.json')
            p = self.gorun(root, '--skip', skip, '--json', out, './...')
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
            self.assertEqual(ids(jload(out)), ['a TestFive', 'a TestFour', 'a TestOne', 'a TestTwo', 'a TestTwoAndAHalf'])
            only = os.path.join(tmp, 'only.txt'); write(tmp, 'only.txt', 'a TestThree\n')
            p = self.gorun(root, '--only', only, '--json', out, './a')
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
            self.assertEqual(ids(jload(out), 'skip'), ['a TestThree'])

    def test_only_with_a_name_that_is_not_in_the_listing_exits_2(self):
        # Review round 2: TestPresent and a tag-excluded TestExcluded were requested; only TestPresent ran, exit 0.
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as tmp:
            self.module(root)
            write(root, 'a/excluded_test.go', '//go:build some_tag\n\npackage a\n\nimport "testing"\n\nfunc TestExcluded(t *testing.T) {}\n')
            only = os.path.join(tmp, 'only.txt'); write(tmp, 'only.txt', 'a TestOne\na TestExcluded\n')
            out = os.path.join(tmp, 'o.json')
            p = self.gorun(root, '--only', only, '--json', out, './a')
            self.assertEqual(p.returncode, 2, p.stdout + p.stderr)
            self.assertEqual(ids(jload(out), 'notrun'), ['a TestExcluded'])
            self.assertEqual(ids(jload(out), 'pass'), ['a TestOne'])

    def test_a_build_error_fails_the_run(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as tmp:
            self.module(root)
            write(root, 'a/broken_test.go', 'package a\n\nimport "testing"\n\nfunc TestBroken(t *testing.T) { undefinedName() }\n')
            out = os.path.join(tmp, 'r.json')
            p = self.gorun(root, '--json', out, './a')
            self.assertEqual(p.returncode, 1, p.stdout + p.stderr)
            self.assertTrue(jload(out)['errors'], 'a package that fails without a failed test is an error')

    def test_a_selected_package_built_only_under_a_tag_is_reported_not_dropped(self):
        # Review round 2: a selected tag-only importer was dropped from the run without a word.
        import gotree
        with tempfile.TemporaryDirectory() as root:
            self.module(root)
            write(root, 'tool/server.go', '//go:build fxselector_sandbox\n\npackage main\n\nimport _ "example.test/m/a"\n')
            self.assertIn('tool', gotree.static_closure(root, ['a']), 'the static map ignores build tags')
            self.assertEqual(gotree.tag_only(root, ['a', 'tool'], env=dict(os.environ, GOFLAGS='-buildvcs=false')),
                             {'tool': ['fxselector_sandbox']})

    def test_failing_test_fails_the_run(self):
        with tempfile.TemporaryDirectory() as root:
            self.module(root)
            write(root, 'b/b_test.go', 'package b\n\nimport "testing"\n\nfunc TestB(t *testing.T) { t.Fatal("broken") }\n')
            p = self.gorun(root, './...')
            self.assertEqual(p.returncode, 1)
            self.assertIn('broken', p.stdout)


if __name__ == '__main__':
    unittest.main()
