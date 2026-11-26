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
        t = self.plan('pkg/webui/shadow_share_test.go', 'pkg/bridge/testdata/issue328_engine_rows.json')
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
            self.assertEqual(plan.c, {'tools/ui-mod/test_stems_feature.c'}, 'test_built.c runs through its own builder')
            self.assertIn(('tools/az01-packer', 'test_built.py'), plan.py)
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


class ReportTest(unittest.TestCase):
    def report(self, plan_extra, res):
        a = type('A', (), {'top': 0, 'report': None})()
        plan = {'mode': 'targeted', 'base': 'abc', 'changed': ['x'], 'changed_tests': [], 'manual': {}, 'unmapped': [],
                'c': [], 'sh': []}
        plan.update(plan_extra)
        out = io.StringIO()
        with redirect_stdout(out):
            rc = devtest.report(a, plan, res, 'host', 0.0)
        return rc, out.getvalue()

    def res(self, skipped=()):
        return {'steps': [{'name': 'go test', 'secs': 1, 'ok': True, 'note': '', 'tail': ''}],
                'go': {'skipped': [{'pkg': 'p', 'test': t, 'reason': 'lab'} for t in skipped], 'tests': {}, 'run': 3},
                'py': {}, 'c': None}

    def test_skipped_test_that_the_lane_changed_is_not_proven(self):
        rc, text = self.report({'changed_tests': [['go', 'p', 'TestNew']]}, self.res(['TestNew/sub']))
        self.assertEqual(rc, 1)
        self.assertIn('NOT PROVEN', text)

    def test_skipped_unrelated_test_is_listed_not_fatal(self):
        rc, text = self.report({'changed_tests': [['go', 'p', 'TestNew']]}, self.res(['TestOther']))
        self.assertEqual(rc, 0)
        self.assertIn('skipped: 1', text)
        self.assertIn('TestOther', text)

    def test_flaky_pass_is_listed_loudly_and_counts_as_proof(self):
        res = self.res(); res['go']['flaky'] = [{'pkg': 'p', 'test': 'TestNew', 'first_failure': 'took 900ms'}]
        rc, text = self.report({'changed_tests': [['go', 'p', 'TestNew']]}, res)
        self.assertEqual(rc, 0)
        self.assertIn('FLAKY: 1 test(s)', text)
        self.assertIn('took 900ms', text)

    def test_a_changed_c_test_with_a_skipped_run_is_not_proven(self):
        res = self.res(); res['c'] = {'results': [{'name': 'tools/ui-mod/test_x.c native', 'ok': True, 'secs': 1, 'runs': [],
                                                   'skipped': ['unshare ... --mounts (needs root)'], 'tail': ''}], 'skipped': ['x']}
        rc, text = self.report({'changed_tests': [['c', 'tools/ui-mod/test_x.c', '*']], 'c': ['tools/ui-mod/test_x.c']}, res)
        self.assertEqual(rc, 1)
        self.assertIn('NOT PROVEN: c tools/ui-mod/test_x.c', text)

    def test_manual_sandbox_tests_are_named_with_their_command_not_failed(self):
        rc, text = self.report({'manual': {'tools/az01-packer/test_x_sandbox.sh': {'how': 'enginex-run sandbox -- bash x', 'why': 'Engine'}}}, self.res())
        self.assertEqual(rc, 0)
        self.assertIn('still to run by the lane', text)
        self.assertIn('enginex-run sandbox -- bash x', text)
        self.assertIn('1 manual sandbox tests still to run', text)

    def test_samba_tests_on_a_host_without_samba_are_not_run_and_said_so(self):
        res = self.res(); res['smb_not_run'] = [['pkg/webui', 'TestSMBNASAPIAndRebootReload', 'no smbpasswd on h']]
        rc, text = self.report({}, res)
        self.assertEqual(rc, 0)
        self.assertIn('NOT RUN: 1 Samba fixture tests', text)
        self.assertIn('TestSMBNASAPIAndRebootReload', text)
        self.assertIn('1 Samba fixture tests NOT RUN', text)
        rc, text = self.report({'changed_tests': [['go', 'pkg/webui', 'TestSMBNASAPIAndRebootReload']]}, res)
        self.assertEqual(rc, 1, 'a Samba test the lane changed must run')
        self.assertIn('NOT PROVEN: go pkg/webui TestSMBNASAPIAndRebootReload (Samba fixture test NOT RUN', text)

    def test_a_remote_run_without_a_result_fails(self):
        res = {'steps': [{'name': 'remote run', 'secs': 1, 'ok': False, 'note': 'exit 1', 'tail': 'sync failed'}], 'go': None, 'py': {}, 'c': None}
        rc, text = self.report({}, res)
        self.assertEqual(rc, 1)
        self.assertIn('sync failed', text)


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
    def test_every_test_in_exactly_one_shard_and_only_heavy_classes_split(self):
        import pyrun
        tests = [(f'm.A.t{i}', 'm.A') for i in range(4)] + [(f'm.B.t{i}', 'm.B') for i in range(30)] + [('m.C.t0', 'm.C')]
        w = {t: (1.0 if t.startswith('m.A') else 10.0 if t.startswith('m.B') else 2.0) for t, _ in tests}
        shards, load = pyrun.make_shards(tests, w, 4)
        flat = [t for s in shards for t in s]
        self.assertEqual(sorted(flat), sorted(t for t, _ in tests))
        self.assertEqual(len(flat), len(set(flat)))
        self.assertEqual(sum(1 for s in shards if any(t.startswith('m.A') for t in s)), 1, 'a light class stays whole')
        self.assertGreater(sum(1 for s in shards if any(t.startswith('m.B') for t in s)), 1, 'a class heavier than a share is split')
        self.assertLess(max(load) / (sum(load) / len(load)), 1.2)

    def test_weights_prefer_this_hosts_timings_then_the_seed(self):
        import pyrun
        w = pyrun.weights_for([('a', 'X'), ('b', 'X'), ('c', 'X')], {'a': 5.0, 'b': 3.0}, {'a': 1.0})
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
                    import tempfile
                    import os
                    self.assertTrue(os.environ['TMPDIR'].startswith(os.environ['PYRUN_BASE']), os.environ['TMPDIR'])
                    self.assertLess(len(os.environ['TMPDIR']) - len(os.environ['PYRUN_BASE']), 20, 'short: unix sockets live there')
            """)
        write(root, 'test_two.py', """\
            import unittest
            class Third(unittest.TestCase):
                def test_c(self): pass
            """)

    def pyrun(self, root, *args, tmpdir=None):
        env = dict(os.environ, TMPDIR=tmpdir or tempfile.gettempdir(), PYRUN_BASE=tmpdir or tempfile.gettempdir())
        return subprocess.run([sys.executable, os.path.join(HERE, 'pyrun.py'), root, *args], capture_output=True, text=True, env=env)

    def test_class_count_equals_serial_discover(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as tmp:
            self.make(root)
            serial = subprocess.run([sys.executable, '-m', 'unittest', 'discover', '-s', root], capture_output=True, text=True)
            n = int(serial.stderr.split('Ran ')[1].split()[0])
            out = os.path.join(tmp, 'r.json')
            p = self.pyrun(root, '--json', out, tmpdir=tmp)
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
            res = jload(out)
            self.assertEqual((res['run'], res['discovered']), (n, n))
            self.assertEqual([s[0] for s in res['skipped']], ['test_one.First.test_skipped'])
            self.assertEqual(os.listdir(tmp), ['r.json'], 'worker TMPDIRs must be removed')

    def test_defer_and_specs_account_for_every_test(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as tmp:
            self.make(root)
            defer = os.path.join(tmp, 'defer.txt'); write(tmp, 'defer.txt', 'test_one.First.test_a  # ARM\ntest_two\n')
            out = os.path.join(tmp, 'r.json')
            p = self.pyrun(root, '--defer', defer, '--json', out, tmpdir=tmp)
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
            res = jload(out)
            self.assertEqual(res['run'] + len(res['deferred']), res['discovered'])
            self.assertEqual(sorted(res['deferred']), ['test_one.First.test_a', 'test_two.Third.test_c'])
            only = os.path.join(tmp, 'o.json')
            self.assertEqual(self.pyrun(root, 'test_two.py', '--json', only, tmpdir=tmp).returncode, 0)
            self.assertEqual(jload(only)['run'], 1)

    def test_failure_exits_nonzero_and_prints_output(self):
        with tempfile.TemporaryDirectory() as root:
            write(root, 'test_bad.py', "import unittest\nclass Bad(unittest.TestCase):\n    def test_x(self): self.fail('boom')\n")
            p = self.pyrun(root)
            self.assertEqual(p.returncode, 1)
            self.assertIn('boom', p.stderr)

    def test_rerun_turns_a_load_failure_into_a_flaky_pass(self):
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
            env = dict(os.environ, TMPDIR=tmp, FLAKY_MARK=os.path.join(tmp, 'mark'))
            run = lambda *a: subprocess.run([sys.executable, os.path.join(HERE, 'pyrun.py'), root, *a], capture_output=True, text=True, env=env)
            p = run('test_flaky.Flaky', '--rerun', '--json', out)
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
            res = jload(out)
            self.assertEqual([f['id'] for f in res['flaky']], ['test_flaky.Flaky.test_timing'])
            os.remove(os.path.join(tmp, 'mark'))
            self.assertEqual(run('test_flaky.Flaky').returncode, 1, 'without --rerun the failure stands')
            os.remove(os.path.join(tmp, 'mark'))
            self.assertEqual(run('test_flaky.Broken', '--rerun').returncode, 1, 'a deterministic failure is never excused')

    def test_import_error_is_a_failed_class(self):
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

    def gorun(self, root, *args):
        env = dict(os.environ, TMPDIR=tempfile.gettempdir())
        return subprocess.run([sys.executable, os.path.join(HERE, 'gorun.py'), '--no-race', '--no-cache', *args], cwd=root,
                              capture_output=True, text=True, env=env)

    def test_sharded_run_equals_unsharded_run(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as tmp:
            self.module(root)
            plain, sharded = os.path.join(tmp, 'p.json'), os.path.join(tmp, 's.json')
            seed = os.path.join(tmp, 'seed.json'); jdump({'a': {'shards': 3, 'tests': {'TestOne': 5.0}}}, seed)
            p1 = self.gorun(root, '--json', plain, './...')
            p2 = self.gorun(root, '--json', sharded, '--seed', seed, './...')
            self.assertEqual((p1.returncode, p2.returncode), (0, 0), p1.stdout + p1.stderr + p2.stdout + p2.stderr)
            a, b = jload(plain), jload(sharded)
            self.assertEqual(a['run'], b['run'])
            self.assertEqual(a['run'], 8)  # TestOne..TestTwoAndAHalf (6) + one subtest in a, TestB
            self.assertEqual([s['test'] for s in b['skipped']], ['TestThree'])
            self.assertIn('shard processes', p2.stdout)
            self.assertNotIn(' 1 shard processes', p2.stdout)

    def test_defer_and_only_account_for_every_test(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as tmp:
            self.module(root)
            defer = os.path.join(tmp, 'defer.txt'); write(tmp, 'defer.txt', 'a TestThree  # lab\nb *  # whole package\n')
            res = os.path.join(tmp, 'd.json')
            p = self.gorun(root, '--defer', defer, '--json', res, './...')
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
            d = jload(res)
            self.assertEqual(sorted(d['deferred']), ['a TestThree', 'b TestB'])
            self.assertEqual(d['skipped'], [])
            self.assertEqual(d['run'], 6)  # One, Two, Two/sub, Four, Five, TwoAndAHalf in a; b is deferred
            only = os.path.join(tmp, 'only.txt'); write(tmp, 'only.txt', 'a TestThree\n')
            o = os.path.join(tmp, 'o.json')
            p = self.gorun(root, '--only', only, '--json', o, './a')
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
            self.assertEqual([s['test'] for s in jload(o)['skipped']], ['TestThree'])

    def test_rerun_turns_a_load_failure_into_a_flaky_pass(self):
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
            env = dict(os.environ, TMPDIR=tmp, FLAKY_MARK=os.path.join(tmp, 'mark'))
            run = lambda *a: subprocess.run([sys.executable, os.path.join(HERE, 'gorun.py'), '--no-race', '--no-cache', *a, './...'], cwd=root,
                                            capture_output=True, text=True, env=env)
            res = os.path.join(tmp, 'f.json')
            p = run('--rerun', '--json', res)
            self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
            self.assertEqual([f['test'] for f in jload(res)['flaky']], ['b TestB'])
            os.remove(os.path.join(tmp, 'mark'))
            self.assertEqual(run().returncode, 1, 'without --rerun the failure stands')

    def test_every_go_test_process_gets_the_explicit_timeout(self):
        with tempfile.TemporaryDirectory() as root:
            self.module(root)
            write(root, 'b/b_test.go', 'package b\n\nimport (\n\t"testing"\n\t"time"\n)\n\nfunc TestB(t *testing.T) { time.Sleep(4 * time.Second) }\n')
            seed = os.path.join(root, 'seed.json'); jdump({'b': {'shards': 1, 'tests': {}}}, seed)
            for extra in ([], ['--seed', seed]):  # plain job and sharded job
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
