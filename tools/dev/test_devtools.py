"""Self-test of tools/dev: path map, changed-test detection, skip accounting, shard and class counts."""
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

    def test_a_selection_with_nothing_in_it_runs_everything(self):
        plan = testmap.map_paths(['no-such-dir/nothing-names-this.bin'], REPO)
        self.assertTrue(plan.empty())  # devtest turns this into a full run (test_an_unknown_file...)

    def test_a_c_change_runs_every_c_test(self):
        own, drivers = testmap.all_c_tests(testmap.index(REPO))
        plan = testmap.map_paths(['tools/ui-mod/master_limiter.h'], REPO)
        self.assertEqual(plan.c, own, 'includes found only through -I or macros would escape an include map')
        self.assertTrue(drivers <= plan.sh | {f'{d}/{f}' for d, f in plan.py})

    def test_reruns_are_off_unless_asked_and_the_full_run_needs_samba(self):
        with tempfile.TemporaryDirectory() as root:
            self.repo(root)
            self.assertFalse(devtest.make_plan(args(base='HEAD'), root)['rerun'])
        full = devtest.make_plan(args(full=True), REPO)
        self.assertIn('samba', full['needs'])


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

    def res(self, *items, errors=(), flaky=()):
        r = {'results': [dict(zip(('kind', 'where', 'test', 'status', 'secs', 'reason'), i)) for i in items],
             'errors': list(errors), 'flaky': list(flaky), 'notes': []}
        return {'steps': [{'name': 'go test', 'secs': 1, 'ok': not errors and all(i[3] != 'fail' for i in items), 'note': '', 'tail': ''}],
                'runs': [r]}

    def test_a_changed_test_that_did_not_run_is_not_proven(self):
        rc, text = self.report({'changed_tests': [['go', 'p', 'TestNew']]}, self.res(('go', 'p', 'TestOld', 'pass', 0, '')))
        self.assertEqual(rc, 1)
        self.assertIn('NOT PROVEN: go p TestNew: did not run', text)
        rc, _ = self.report({'changed_tests': [['py', 'd', 'test_a.A.test_x']]}, self.res(('py', 'd', 'test_a.A.test_y', 'pass', 0, '')))
        self.assertEqual(rc, 1)

    def test_a_changed_test_that_skipped_is_not_proven_an_unrelated_skip_is_listed(self):
        rc, text = self.report({'changed_tests': [['go', 'p', 'TestNew']]}, self.res(('go', 'p', 'TestNew', 'skip', 0, 'lab')))
        self.assertEqual(rc, 1)
        self.assertIn('NOT PROVEN: go p TestNew: skip', text)
        rc, text = self.report({'changed_tests': [['go', 'p', 'TestNew']]},
                               self.res(('go', 'p', 'TestNew', 'pass', 0, ''), ('go', 'p', 'TestOther', 'skip', 0, 'lab')))
        self.assertEqual(rc, 0)
        self.assertIn('skipped: 1', text)

    def test_a_runner_error_fails_the_run(self):
        rc, text = self.report({}, self.res(errors=['pkg/x: go test -list failed (exit 1): undefined: y']))
        self.assertEqual(rc, 1)
        self.assertIn('ERROR: pkg/x: go test -list failed', text)

    def test_flaky_is_only_printed_and_comes_in_one_format(self):
        res = self.res(('go', 'p', 'TestNew', 'pass', 0, ''), flaky=[{'kind': 'go', 'where': 'p', 'test': 'TestNew', 'first_failure': 'took 900ms'}])
        rc, text = self.report({'changed_tests': [['go', 'p', 'TestNew']]}, res)
        self.assertEqual(rc, 0)
        self.assertIn('FLAKY: 1 test(s)', text)
        self.assertIn('took 900ms', text)

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
        self.assertIn('TestSMBNASAPIAndRebootReload: NOT RUN', text)
        rc, _ = self.report({'changed_tests': [['go', 'pkg/webui', 'TestSMBNASAPIAndRebootReload']]},
                            self.res(('go', 'pkg/webui', 'TestSMBNASAPIAndRebootReload', 'notrun', 0, 'no smbpasswd')))
        self.assertEqual(rc, 1, 'a changed Samba test must run')

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
    def test_a_stop_during_start_up_leaves_no_scratch_and_no_git(self):
        import hashlib, signal
        with tempfile.TemporaryDirectory() as tree:
            plan = {'mode': 'targeted', 'go': [], 'go_all': False, 'go_code': [], 'gofmt': [], 'py': [], 'c': [], 'sh': [],
                    'no_cache': False, 'rerun': False}
            jdump(plan, os.path.join(tree, 'plan.json'))
            write(tree, 'file.txt', 'x\n')
            env = dict(os.environ); env.pop('ENGINEX_CPU_BUDGET', None)  # cpu_budget() sleeps a second: stop it there
            p = subprocess.Popen([sys.executable, os.path.join(HERE, 'devtest.py'), '--exec', 'plan.json'], cwd=tree, env=env,
                                 stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            scratch = os.path.join('/var/tmp', 'x' + hashlib.sha1(os.path.realpath(tree).encode()).hexdigest()[:6])
            for _ in range(200):  # until start-up has made its scratch and its .git
                if os.path.isdir(os.path.join(tree, '.git')) and os.path.isdir(scratch):
                    break
                time.sleep(0.01)
            p.send_signal(signal.SIGTERM)
            p.communicate(timeout=30)
            self.assertFalse(os.path.exists(os.path.join(tree, '.git')), 'the staging .git is removed')
            self.assertFalse(os.path.exists(scratch), 'TMPDIR is removed')
            self.assertFalse(os.path.exists(scratch + '.lock'), 'its lock file is removed')


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


class ShardTest(unittest.TestCase):
    """Python processes never split what shares state (review 2)."""
    def test_classes_stay_whole_and_a_module_with_shared_state_is_one_process(self):
        import pyrun
        tests = [(f'm.A.t{i}', 'm', 'm.A') for i in range(4)] + [(f'm.B.t{i}', 'm', 'm.B') for i in range(30)] + \
                [('m.C.t0', 'm', 'm.C'), ('n.D.t0', 'n', 'n.D'), ('n.E.t0', 'n', 'n.E')]
        w = {t: (1.0 if '.A.' in t else 10.0 if '.B.' in t else 2.0) for t, *_ in tests}
        units = pyrun.make_units(tests, w, 4, keep_whole={'n': ['shares state']}, net=set())
        flat = [t for u in units for t in u[2]]
        self.assertEqual(sorted(flat), sorted(t for t, *_ in tests))
        self.assertEqual(len(flat), len(set(flat)))
        for cls in ('m.A', 'm.B', 'm.C'):
            self.assertEqual(sum(1 for u in units if any(t.startswith(cls + '.') for t in u[2])), 1, f'{cls} stays whole')
        self.assertEqual([u[1] for u in units].count('n'), 1, 'module n runs in one process')
        self.assertTrue(all(len({t.split('.')[0] for t in u[2]}) == 1 for u in units), 'one module per process')

    def test_module_state_hazards_are_found_and_undone_changes_are_not(self):
        import pyrun
        with tempfile.TemporaryDirectory() as root:
            write(root, 'test_leak.py', """\
                import unittest
                import helper
                STATE = {}
                class A(unittest.TestCase):
                    def test_sets(self):
                        helper.MODE = 'x'
                class B(unittest.TestCase):
                    def test_fills(self):
                        STATE['k'] = 1
                """)
            write(root, 'test_clean.py', """\
                import os, unittest
                import helper
                class A(unittest.TestCase):
                    def test_restores(self):
                        old = helper.MODE
                        try:
                            helper.MODE = 'x'
                        finally:
                            helper.MODE = old
                    def test_cleanup(self):
                        self.addCleanup(setattr, helper, 'MODE', helper.MODE)
                        helper.MODE = 'y'
                    def test_files(self):
                        os.remove('/nonexistent') if False else None
                """)
            leak = pyrun.module_hazards(os.path.join(root, 'test_leak.py'))
            self.assertEqual(len(leak), 2, leak)
            self.assertEqual(pyrun.module_hazards(os.path.join(root, 'test_clean.py')), [])

    def test_weights_prefer_this_hosts_timings_then_the_seed(self):
        import pyrun
        w = pyrun.weights_for([('a', 'm', 'm.X'), ('b', 'm', 'm.X'), ('c', 'm', 'm.X')], {'a': 5.0, 'b': 3.0}, {'a': 1.0})
        self.assertEqual((w['a'], w['b']), (1.0, 3.0))
        self.assertEqual(w['c'], 3.0, 'unknown tests weigh the median')


class StableDirTest(unittest.TestCase):
    def test_same_path_every_run_and_no_sharing_while_held(self):
        import gorun
        with tempfile.TemporaryDirectory() as base:
            first, lock = gorun.stable_dir(base, 'scratch')
            self.assertEqual(first, os.path.join(base, 'scratch'))
            second, second_lock = gorun.stable_dir(base, 'scratch')  # a concurrent run gets its own directory
            self.assertNotEqual(second, first)
            self.assertIsNone(second_lock)
            shutil.rmtree(second)
            gorun.release_dir(first, lock)
            self.assertEqual(os.listdir(base), [], 'the directory and its lock file are removed')
            again, lock = gorun.stable_dir(base, 'scratch')
            self.assertEqual(again, first)  # Go's test cache keys on the TMPDIR value
            gorun.release_dir(again, lock)


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
            p = self.pyrun(root, '-j', '3', '--json', out, tmpdir=tmp)
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
            self.assertEqual(p.returncode, 1, 'a selected file with no test must not pass as zero tests')

    def test_failure_exits_nonzero_and_prints_output(self):
        with tempfile.TemporaryDirectory() as root:
            write(root, 'test_bad.py', "import unittest\nclass Bad(unittest.TestCase):\n    def test_x(self): self.fail('boom')\n")
            p = self.pyrun(root)
            self.assertEqual(p.returncode, 1)
            self.assertIn('boom', p.stderr)

    def test_a_module_whose_classes_share_state_runs_in_one_process(self):
        # The review's probe: class A sets module state that class B reads. Serially it passes; split it must too.
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as tmp:
            write(root, 'test_shared.py', textwrap.dedent("""\
                import unittest
                SEEN = None
                class A(unittest.TestCase):
                    def test_a(self):
                        global SEEN
                        SEEN = 'A ran first'
                class B(unittest.TestCase):
                    def test_b(self):
                        self.assertEqual(SEEN, 'A ran first')
                """) + ''.join(f"class C{i}(unittest.TestCase):\n    def test_c(self): pass\n" for i in range(6)))
            out = os.path.join(tmp, 'r.json')
            p = self.pyrun(root, '-j', '2', '--json', out, tmpdir=tmp)
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
            self.assertEqual(len(jload(out)['units']), 1)

    def test_modules_that_bind_a_fixed_port_do_not_run_at_the_same_time(self):
        import random
        port = random.randint(41000, 48000)
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as tmp:
            for name in ('test_port_a.py', 'test_port_b.py'):
                write(root, name, f"""\
                    import socket, time, unittest
                    class Port(unittest.TestCase):
                        def test_holds_the_port(self):
                            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
                            s.bind(('127.0.0.1', {port}))
                            time.sleep(1.0)
                            s.close()
                    """)
            p = self.pyrun(root, '-j', '2', tmpdir=tmp)
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)

    def test_rerun_turns_a_load_failure_into_a_flaky_pass_and_only_with_rerun(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as tmp:
            write(root, 'test_flaky.py', """\
                import os, unittest
                class Flaky(unittest.TestCase):
                    def test_timing(self):
                        mark = os.environ['FLAKY_MARK']
                        if not os.path.exists(mark):
                            open(mark, 'w').close()
                            self.fail('too slow under load')
                    def test_steady(self): pass
                class Broken(unittest.TestCase):
                    def test_always(self): self.fail('real bug')
                """)
            out = os.path.join(tmp, 'r.json')
            env = {'FLAKY_MARK': os.path.join(tmp, 'mark')}
            p = self.pyrun(root, 'test_flaky.Flaky', '--rerun', '--json', out, tmpdir=tmp, env=env)
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
            self.assertEqual([f['test'] for f in jload(out)['flaky']], ['test_flaky.Flaky.test_timing'])
            os.remove(os.path.join(tmp, 'mark'))
            self.assertEqual(self.pyrun(root, 'test_flaky.Flaky', tmpdir=tmp, env=env).returncode, 1, 'without --rerun the failure stands')
            os.remove(os.path.join(tmp, 'mark'))
            self.assertEqual(self.pyrun(root, 'test_flaky.Broken', '--rerun', tmpdir=tmp, env=env).returncode, 1, 'a real failure is never excused')

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

    def seed(self, root, pkg, shards):
        path = os.path.join(root, 'seed.json'); jdump({pkg: {'shards': shards, 'tests': {}}}, path)
        return path

    def test_a_split_run_equals_one_process(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as tmp:
            self.module(root)
            plain, split = os.path.join(tmp, 'p.json'), os.path.join(tmp, 's.json')
            p1 = self.gorun(root, '--json', plain, './...')
            p2 = self.gorun(root, '--json', split, '--seed', self.seed(root, 'a', 3), './...')
            self.assertEqual((p1.returncode, p2.returncode), (0, 0), p1.stdout + p1.stderr + p2.stdout + p2.stderr)
            a, b = jload(plain), jload(split)
            self.assertEqual(ids(a), ids(b))
            self.assertEqual(len(ids(a)), 7)  # TestOne..TestTwoAndAHalf in a, TestB
            self.assertEqual(ids(b, 'skip'), ['a TestThree'])
            self.assertGreater(sum(1 for k in b['jobs'] if k.startswith('a#')), 1)

    def test_a_build_error_in_a_listed_package_fails_the_run(self):
        # The review's probe: `go test -list` of a seeded package failed (a compile error) and the run passed with 0 tests.
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as tmp:
            self.module(root)
            write(root, 'a/broken_test.go', 'package a\n\nimport "testing"\n\nfunc TestBroken(t *testing.T) { undefinedName() }\n')
            out = os.path.join(tmp, 'r.json')
            p = self.gorun(root, '--json', out, '--seed', self.seed(root, 'a', 2), './a')
            self.assertEqual(p.returncode, 1, p.stdout + p.stderr)
            self.assertTrue(any('go test -list failed' in e for e in jload(out)['errors']), jload(out)['errors'])

    def test_a_package_that_binds_a_fixed_port_is_not_split(self):
        import random
        port = random.randint(41000, 48000)
        with tempfile.TemporaryDirectory() as root:
            write(root, 'go.mod', 'module example.test/m\n\ngo 1.21\n')
            write(root, 'p/p.go', 'package p\n')
            body = ''.join(f"""
                func TestHold{i}(t *testing.T) {{
                	c, err := net.ListenUDP("udp4", &net.UDPAddr{{IP: net.IPv4(127, 0, 0, 1), Port: {port}}})
                	if err != nil {{
                		t.Fatal(err)
                	}}
                	time.Sleep(time.Second)
                	c.Close()
                }}
                """ for i in range(2))
            write(root, 'p/p_test.go', 'package p\n\nimport (\n\t"net"\n\t"testing"\n\t"time"\n)\n' + textwrap.dedent(body))
            p = self.gorun(root, '--seed', self.seed(root, 'p', 2), './p')
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
            self.assertIn('p: not split', p.stdout + p.stderr)

    def test_a_package_whose_tests_change_package_state_is_not_split(self):
        with tempfile.TemporaryDirectory() as root:
            write(root, 'go.mod', 'module example.test/m\n\ngo 1.21\n')
            write(root, 'q/q.go', 'package q\n\nvar mode = "stock"\n')
            write(root, 'q/q_test.go', """\
                package q

                import "testing"

                func TestA(t *testing.T) { mode = "patched" }
                func TestB(t *testing.T) {
                	if mode != "patched" {
                		t.Fatal("TestB runs after TestA in one process")
                	}
                }
                """)
            p = self.gorun(root, '--seed', self.seed(root, 'q', 2), './q')
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)

    def test_defer_and_only_account_for_every_test(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as tmp:
            self.module(root)
            defer = os.path.join(tmp, 'defer.txt'); write(tmp, 'defer.txt', 'a TestThree  # lab\nb *  # whole package\n')
            out = os.path.join(tmp, 'd.json')
            p = self.gorun(root, '--defer', defer, '--json', out, './...')
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
            self.assertEqual(ids(jload(out)), ['a TestFive', 'a TestFour', 'a TestOne', 'a TestTwo', 'a TestTwoAndAHalf'])
            only = os.path.join(tmp, 'only.txt'); write(tmp, 'only.txt', 'a TestThree\n')
            p = self.gorun(root, '--only', only, '--json', out, './a')
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
            self.assertEqual(ids(jload(out), 'skip'), ['a TestThree'])

    def test_rerun_turns_a_load_failure_into_a_flaky_pass_and_only_with_rerun(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as tmp:
            self.module(root)
            write(root, 'b/b_test.go', """\
                package b

                import (
                	"os"
                	"testing"
                )

                func TestB(t *testing.T) {
                	mark := os.Getenv("FLAKY_MARK")
                	if _, err := os.Stat(mark); err != nil {
                		os.WriteFile(mark, nil, 0o644)
                		t.Fatal("too slow under load")
                	}
                }
                """)
            env = {'FLAKY_MARK': os.path.join(tmp, 'mark')}
            out = os.path.join(tmp, 'f.json')
            p = self.gorun(root, '--rerun', '--json', out, './...', env=env)
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
            self.assertEqual([(f['kind'], f['where'], f['test']) for f in jload(out)['flaky']], [('go', 'b', 'TestB')])
            os.remove(os.path.join(tmp, 'mark'))
            self.assertEqual(self.gorun(root, './...', env=env).returncode, 1, 'without --rerun the failure stands')

    def test_every_go_test_process_gets_the_explicit_timeout(self):
        with tempfile.TemporaryDirectory() as root:
            self.module(root)
            write(root, 'b/b_test.go', 'package b\n\nimport (\n\t"testing"\n\t"time"\n)\n\nfunc TestB(t *testing.T) { time.Sleep(4 * time.Second) }\n')
            for extra in ([], ['--seed', self.seed(root, 'b', 1)]):
                p = self.gorun(root, '--timeout', '1s', *extra, './b')
                self.assertEqual(p.returncode, 1, p.stdout + p.stderr)
                self.assertIn('test timed out after 1s', p.stdout + p.stderr)

    def test_a_directory_whose_go_files_are_all_excluded_is_not_a_package_here(self):
        import gotree
        with tempfile.TemporaryDirectory() as root:
            self.module(root)
            write(root, 'tool/server.go', '//go:build ignore\n\npackage main\n')
            self.assertIn('tool', gotree.static_closure(root, ['tool']), 'the static map ignores build tags')
            ok = gotree.buildable(root, env=dict(os.environ, GOFLAGS='-buildvcs=false'))
            self.assertEqual(sorted(ok), ['a', 'b'])

    def test_failing_test_fails_the_run(self):
        with tempfile.TemporaryDirectory() as root:
            self.module(root)
            write(root, 'b/b_test.go', 'package b\n\nimport "testing"\n\nfunc TestB(t *testing.T) { t.Fatal("broken") }\n')
            p = self.gorun(root, './...')
            self.assertEqual(p.returncode, 1)
            self.assertIn('broken', p.stdout)


if __name__ == '__main__':
    unittest.main()
