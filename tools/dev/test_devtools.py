"""Self-test of tools/dev: path map, changed-test detection, skip accounting, shard and class counts."""
import io, json, os, shutil, subprocess, sys, tempfile, textwrap, unittest
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


class MapTest(unittest.TestCase):
    def plan(self, *paths):
        return testmap.map_paths(list(paths), HERE + '/../..')

    def test_go_files_map_to_their_package(self):
        self.assertEqual(self.plan('pkg/webui/shadow.go', 'pkg/webui/x_test.go').go, {'pkg/webui'})
        self.assertTrue(self.plan('go.mod').go_all)

    def test_build_packs_maps_to_ui_mod_tests(self):
        self.assertEqual(self.plan('tools/ui-mod/build_packs.py').py,
                         {('tools/az01-packer', 'test_ui_mod.py'), ('tools/az01-packer', 'test_ui_perf.py')})

    def test_packer_and_stems_modules(self):
        self.assertIn(('tools/az01-packer', 'test_az01_packer.py'), self.plan('tools/az01-packer/az01_packer.py').py)
        self.assertEqual(self.plan('tools/az01-packer/test_docs_audit.py').py, {('tools/az01-packer', 'test_docs_audit.py')})
        self.assertEqual(self.plan('tools/stems/worker.py').py, {('tools/stems', 'test_worker.py')})

    def test_overlay_and_dev_tools(self):
        self.assertEqual(self.plan('rootfs-overlay/usr/local/bin/x').sh, {'tests/test_rootfs_overlay.sh'})
        self.assertIn(('tools/dev', 'test_devtools.py'), self.plan('tools/dev/pyrun.py').py)

    def test_unmapped_file_runs_nothing(self):
        self.assertTrue(self.plan('LICENSE').empty())

    def test_arm_list_needs_a_reason(self):
        with tempfile.NamedTemporaryFile('w', suffix='.txt') as f:
            f.write('py tools/x test_a.B.c\n'); f.flush()
            with self.assertRaises(SystemExit):
                devtest.load_arm(f.name)
        entries = devtest.load_arm(os.path.join(HERE, 'arm-tests.txt'))
        self.assertTrue(all(e['reason'] for e in entries))


class ChangedTestsTest(unittest.TestCase):
    def test_added_and_edited_tests_are_found(self):
        with tempfile.TemporaryDirectory() as root:
            def g(*a):
                subprocess.run(['git', '-C', root, '-c', 'user.name=t', '-c', 'user.email=t@example.invalid', *a], check=True, capture_output=True)
            g('init', '-q')
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
            found = testmap.changed_tests(root, 'HEAD', ['p/a_test.go', 'tools/test_a.py', 'tools/test_b.py'])
            self.assertEqual(found, {('go', 'p', 'TestEdited'), ('go', 'p', 'TestNew'), ('py', 'tools', 'test_a.A.test_edited'),
                                     ('py', 'tools', 'test_b.B.test_b')})


class ReportTest(unittest.TestCase):
    def report(self, plan_extra, fast):
        a = type('A', (), {'top': 0, 'report': None})()
        plan = {'mode': 'targeted', 'base': 'abc', 'changed': ['x'], 'arm': [], 'changed_tests': []}
        plan.update(plan_extra)
        out = io.StringIO()
        with redirect_stdout(out):
            rc = devtest.report(a, plan, fast, None, {}, 'host', None, 0.0, [], False)
        return rc, out.getvalue()

    def fast(self, skipped=()):
        return {'steps': [{'name': 'go test', 'secs': 1, 'ok': True, 'note': '', 'tail': ''}],
                'go': {'skipped': [{'pkg': 'p', 'test': t, 'reason': 'lab'} for t in skipped], 'deferred': [], 'tests': {}, 'run': 3},
                'py': {}}

    def test_skipped_test_that_the_lane_changed_is_not_proven(self):
        rc, text = self.report({'changed_tests': [['go', 'p', 'TestNew']]}, self.fast(['TestNew/sub']))
        self.assertEqual(rc, 1)
        self.assertIn('NOT PROVEN', text)

    def test_skipped_unrelated_test_is_listed_not_fatal(self):
        rc, text = self.report({'changed_tests': [['go', 'p', 'TestNew']]}, self.fast(['TestOther']))
        self.assertEqual(rc, 0)
        self.assertIn('skipped: 1', text)
        self.assertIn('TestOther', text)

    def test_flaky_pass_is_listed_loudly_and_counts_as_proof(self):
        fast = self.fast(); fast['go']['flaky'] = [{'test': 'p TestNew', 'first_failure': 'took 900ms'}]
        rc, text = self.report({'changed_tests': [['go', 'p', 'TestNew']]}, fast)
        self.assertEqual(rc, 0)
        self.assertIn('FLAKY: 1 test(s)', text)
        self.assertIn('took 900ms', text)

    def test_deferred_changed_test_without_arm_run_is_not_proven(self):
        fast = self.fast(); fast['go']['deferred'] = ['p TestNew']
        rc, text = self.report({'changed_tests': [['go', 'p', 'TestNew']]}, fast)
        self.assertEqual(rc, 1)
        self.assertIn('deferred to the ARM tier', text)


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
                    self.assertIn('pyrun-', tempfile.gettempdir())
            """)
        write(root, 'test_two.py', """\
            import unittest
            class Third(unittest.TestCase):
                def test_c(self): pass
            """)

    def pyrun(self, root, *args, tmpdir=None):
        env = dict(os.environ, TMPDIR=tmpdir or tempfile.gettempdir())
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

    def test_failing_test_fails_the_run(self):
        with tempfile.TemporaryDirectory() as root:
            self.module(root)
            write(root, 'b/b_test.go', 'package b\n\nimport "testing"\n\nfunc TestB(t *testing.T) { t.Fatal("broken") }\n')
            p = self.gorun(root, './...')
            self.assertEqual(p.returncode, 1)
            self.assertIn('broken', p.stdout)


if __name__ == '__main__':
    unittest.main()
