#!/usr/bin/env python3
"""Implementation of tools/dev/test.sh: one ARM build host runs every selected test (Go, Python, C, bash).

  tools/dev/test.sh [--full] [--list [--json]] [--base REF] [--into BRANCH] [--tree DIR] [--host HOST] [--exclusive]
                    [--require-samba] [--no-cache] [--accept-incomplete] [--report DIR]

Targeted (default): files changed against the merge base with origin/<BRANCH> (default agent/5306-batch2), plus
uncommitted and untracked files, are mapped to tests by tools/dev/testmap.py: gofmt of the changed Go files, go vet
and go test -race of the touched packages and every package that imports them, the whole test file of a Python change
(the whole tools/az01-packer/test_ui_mod.py for the pack generator), every C test of the release checklist with all
its variants, and the automatic bash tests. Engine sandboxes the change selects are named with their commands.
It fails closed: a Git error or an unknown base stops the run (exit 2); no changed file, a file or deletion the map
does not know, or a selection with nothing in it runs everything instead, keeping every target the narrow plan had
(its Engine sandboxes included).
--full: every Go package, Python directory, C test and automatic bash test, on a host that can run all of them
(the Samba fixture included). --list: print the selection and why; run nothing (needs only git and Python).

The tree is copied with remote-build to one host of ENGINEX_BUILD_HOSTS (environment or ~/.config/enginex/dev.env),
the least busy one that has what the plan needs, and runs inside `enginex-run sandbox` (`--exclusive`: `enginex-run
exclusive`); a host without enginex-run is refused, never used unlocked. There `go list` re-checks the Go selection
(it can only add packages). Go tests that start the Samba fixture (internal/testsmb) run on a host with smbd and
smbpasswd, one at a time, in a private UTS namespace with hostname localhost.

Result and exit status: PASS (0) only when every selected test produced a pass. FAIL (1): a failure or an error.
INCOMPLETE (3): nothing failed, but a test of a changed test file (each one counts), a test run by name, or a selected
target (a Python file or spec, a Go package with tests, a C test, a bash test) skipped, did not run or has no result,
or a sandbox the lane runs is listed, or a package built only under a constraint other than plain tags. A package
built only under a tag is vetted by file name, as its wrapper builds it, in narrow and full runs alike (it cannot be
built as a package). --accept-incomplete turns INCOMPLETE into
PASS once the lane has run them; the list stays in the output. Other skips (a test the change did not touch, in a
target that passed) are listed with their reason. Tests are never rerun. Only what tools/dev/split.json lists runs
in parallel, and only in a tree that meets the entry's prerequisite.
"""
import argparse, fcntl, filecmp, hashlib, json, os, pathlib, re, shutil, signal, socket, subprocess, sys, time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import gotree  # noqa: E402
import pyrun  # noqa: E402
import results  # noqa: E402
import testmap  # noqa: E402

MARK = 'ENGINEX_RESULT_JSON '
PLAN_PREFIX = '.enginex-test-plan'  # .enginex-test-plan-<pid>.json in the tree for one run (synced with it)
TOOLS_PREFIX = '.enginex-devtools'  # a copy of this runner in a tree that has none (synced with it, removed after)
DEFAULT_INTO = 'agent/5306-batch2'
SANDBOX_ENGINES = ('e504', 'e530', 'esl4', 'eaz04')
EXIT_INCOMPLETE = 3


class PlanError(Exception):
    """The change set cannot be known (a Git error, an unknown base): the run stops instead of guessing."""


def load_env():
    path = os.environ.get('ENGINEX_DEV_ENV', os.path.expanduser('~/.config/enginex/dev.env'))
    if os.path.isfile(path):
        for line in pathlib.Path(path).read_text().splitlines():
            m = re.match(r'\s*(?:export\s+)?(\w+)=(.*?)\s*$', line)
            if m and m.group(1) not in os.environ:
                os.environ[m.group(1)] = m.group(2).strip('\'"')


def run(cmd, **kw):
    return subprocess.run(cmd, capture_output=True, text=True, **kw)


def git(root, *args):
    """git's output; a Git error stops the plan (PlanError), never an empty change set."""
    p = run(['git', '-C', root, *args])
    if p.returncode:
        raise PlanError(f"git {' '.join(args)} failed in {root}: {(p.stderr or p.stdout).strip()[-500:]}")
    return p.stdout


# ---------------------------------------------------------------- the plan (made where test.sh is called)

def make_plan(a, root):
    plan = {'mode': 'full' if a.full else 'targeted', 'no_cache': a.no_cache,
            'base': None, 'changed': [], 'deleted': [], 'why': {}, 'manual': {}, 'unmapped': [], 'fallback': ''}
    changed, deleted = [], []
    if not a.full:
        if a.base:
            base = a.base
        else:
            p = run(['git', '-C', root, 'merge-base', 'HEAD', f'origin/{a.into}'])
            if p.returncode or not p.stdout.strip():
                raise PlanError(f'no merge base of HEAD with origin/{a.into} (git fetch origin {a.into}, or pass --base REF): '
                                f'{(p.stderr or "").strip()[-300:]}')
            base = p.stdout.strip()
        p = run(['git', '-C', root, 'rev-parse', '--verify', '--quiet', base + '^{commit}'])
        if p.returncode or not p.stdout.strip():
            raise PlanError(f'base {base!r} is not a commit in {root}')
        plan['base'] = p.stdout.strip()
        tracked = git(root, 'diff', '--name-only', '--no-renames', plan['base']).splitlines()
        untracked = git(root, 'ls-files', '-o', '--exclude-standard').splitlines()
        deleted = git(root, 'diff', '--name-only', '--no-renames', '--diff-filter=D', plan['base']).splitlines()
        changed = sorted({x for x in tracked + untracked if x and not x.startswith((PLAN_PREFIX, TOOLS_PREFIX))})
        deleted = sorted({x for x in deleted if x})
    plan['changed'], plan['deleted'] = changed, deleted
    if a.full:
        sel = testmap.full_plan(root)
    else:
        sel = testmap.map_paths(changed, root, deleted=set(deleted))
        reason = ('no changed file' if not changed else
                  f"{len(sel.unmapped)} changed files no rule or test knows: {' '.join(sel.unmapped[:8])}" if sel.unmapped else
                  'the change selects no test' if sel.empty() else '')
        if reason:  # fail closed: everything, plus every target the narrow plan had (its sandboxes and C drivers)
            plan['fallback'] = reason
            plan['unmapped'] = sorted(sel.unmapped)
            plan['mode'] = 'full'
            sel = testmap.full_plan(root, keep=sel)
    plan['go_all'] = sel.go_all
    code = sorted(d for d in sel.go_code if os.path.isdir(os.path.join(root, d)))
    plan['go_code'] = code
    plan['go'] = [] if sel.go_all else sorted(set(gotree.static_closure(root, code)) | {d for d in sel.go if os.path.isdir(os.path.join(root, d))})
    plan['gofmt'] = None if plan['mode'] == 'full' else [p for p in changed if p.endswith('.go') and os.path.isfile(os.path.join(root, p))]
    by_dir = {}
    for d, f in sel.py:
        by_dir.setdefault(d, set()).add(f)
    plan['py'] = [{'dir': d, 'specs': [] if None in fs else sorted(fs)} for d, fs in sorted(by_dir.items())]  # no specs: whole dir
    plan['sh'] = sorted(sel.sh)
    plan['c'] = sorted(sel.c)
    plan['why'] = {k: v for k, v in sorted(sel.why.items())}
    plan['manual'] = dict(sorted(sel.manual.items()))
    if not plan['fallback']:
        plan['unmapped'] = sorted(sel.unmapped)
    plan['changed_tests'] = [] if a.full else sorted(map(list, testmap.changed_tests(root, changed)))
    plan['smb'] = gotree.smb_tests(root, sorted(gotree.packages(root)) if sel.go_all else plan['go'])
    # The full run must run everything, the Samba fixture included; a lane that changed a Samba test needs Samba too.
    plan['smb_required'] = bool(plan['smb']) and (plan['mode'] == 'full' or a.require_samba or
                                                  any(['go', d, n] in plan['changed_tests'] for d, n in plan['smb']))
    plan['split'] = json.loads(pathlib.Path(HERE, 'split.json').read_text())
    plan['needs'] = needs_of(plan)
    return plan


def needs_of(plan):
    """What the build host must have: samba, gcc14 (the hook rebuild, the C tests' -Werror builds), lab files."""
    needs = {'samba'} if plan.get('smb_required') else set()
    sh_auto = testmap.SH_TESTS['auto']
    for s in plan['sh']:
        needs |= set(sh_auto.get(s, {}).get('needs', []))
    if plan['c']:
        needs.add('gcc14')
        for src in plan['c']:
            entry = testmap.C_TESTS['tests'].get(src) or testmap.C_TESTS['default']
            text = json.dumps(entry) + testmap.C_TESTS['default_args'].get(src, '')
            needs |= {'lab:' + e for e in SANDBOX_ENGINES if '{' + e + '}' in text}
            if '{lab}' in text or 'lab' in entry.get('needs', []):
                needs.add('lab')
    return sorted(needs)


def print_plan(plan):
    go = ['./...'] if plan['go_all'] else plan['go']
    print(f"== tools/dev/test.sh {plan['mode']} plan{' vs ' + plan['base'][:12] if plan['base'] else ''}: "
          f"{len(plan['changed'])} changed files ==")
    if plan['fallback']:
        print(f"running everything: {plan['fallback']}")
    print(f"go test -race ({len(go)} packages): {' '.join(go) if go else '-'}")
    if plan['go_code']:
        print(f"  changed Go code in: {' '.join(plan['go_code'])} (their importers are included)")
    for item in plan['py']:
        print(f"python {item['dir']}: {' '.join(item['specs']) if item['specs'] else 'every test file'}")
    print(f"C tests ({len(plan['c'])}, every variant of tools/dev/c-tests.json): {' '.join(plan['c']) or '-'}")
    print(f"bash tests: {' '.join(plan['sh']) or '-'}")
    if plan.get('smb'):
        print(f"Samba fixture tests ({len(plan['smb'])}): on a host with smbd and smbpasswd, inside a private UTS namespace "
              f"with hostname localhost; elsewhere NOT RUN{' (Samba host required)' if plan['smb_required'] else ''}")
    print(f"host needs: {', '.join(plan['needs']) or 'any build host'}")
    if plan['manual']:
        print('manual (Engine sandboxes this change selects; test.sh does not run them):')
        for path, info in plan['manual'].items():
            print(f"  {path}: {info['how']}  [{info['why']}]")
    if plan['why'] and not plan['fallback']:
        print('why:')
        for key, reasons in plan['why'].items():
            print(f"  {key} <- {', '.join(reasons[:3])}{' ...' if len(reasons) > 3 else ''}")


# ---------------------------------------------------------------- execution (on the build host)

class Step:
    def __init__(self, name):
        self.name, self.secs, self.ok, self.note, self.tail, self.out = name, 0.0, None, '', '', ''


STOP = [0]       # the signal that stopped this run (exec on the build host), 0 while it runs
CURRENT = [None]  # the process group of the step running now


def _stop(signum, frame):
    """A stop never raises at an arbitrary point (it could land between creating a file and recording it): it ends the
    step that runs now, and the run leaves at the next step boundary through its `finally`."""
    STOP[0] = STOP[0] or signum
    if CURRENT[0]:
        try:
            os.killpg(CURRENT[0], signal.SIGKILL)
        except OSError:
            pass


def stopped():
    if STOP[0]:
        raise SystemExit(128 + STOP[0])


def spawn(step, cmd, env, cwd=None, timeout=None):
    """Run one step in its own process group; on a timeout or a stop the whole group (the runner and every worker it
    started) is killed, so nothing keeps running on a shared host."""
    stopped()
    t0 = time.monotonic()
    p = subprocess.Popen(cmd, env=env, cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
                         errors='replace', start_new_session=True)
    CURRENT[0] = p.pid
    try:
        if STOP[0]:  # the stop came while the step was being started
            os.killpg(p.pid, signal.SIGKILL)
        out, _ = p.communicate(timeout=timeout)
        rc = p.returncode
    except subprocess.TimeoutExpired:
        os.killpg(p.pid, signal.SIGKILL)
        out, _ = p.communicate()
        rc, out = 124, (out or '') + f'\n(timed out after {timeout}s)'
    finally:
        CURRENT[0] = None
    stopped()
    step.secs = time.monotonic() - t0
    step.ok = rc == 0
    step.out = out
    step.tail = '\n'.join(out.splitlines()[-150:]) if not step.ok else ''
    print(f'[{step.name}] {"ok" if step.ok else "FAILED"} {step.secs:.1f}s', file=sys.stderr, flush=True)
    return rc


def samba_missing():
    """Why this host cannot run the Samba fixture (internal/testsmb), or '' when it can."""
    missing = [t for t in ('smbd', 'smbpasswd', 'unshare', 'hostname') if not shutil.which(t)]
    if missing:
        return f"no {' and '.join(missing)} on {socket.gethostname()}"
    if os.geteuid() != 0:
        return 'not root (unshare --uts needs root)'
    return ''


def uts_localhost(cmd):
    """cmd in a private UTS namespace whose hostname is localhost (the build host's own name is untouched)."""
    return ['unshare', '--uts', 'sh', '-c', 'hostname localhost && exec "$@"', 'sh'] + cmd


def gcc_major():
    out = run(['gcc', '-dumpversion']).stdout.strip()
    return out.split('.')[0] if out else ''


def split_args(split, d):
    """pyrun's --split arguments for Python directory d: the split.json entries of d whose prerequisite this tree
    meets (each entry's equality run holds only with it); the others run in one process, with a note saying why."""
    args, why = [], []
    for path, v in sorted(split['python'].items()):
        if os.path.dirname(path) != d:
            continue
        need = v.get('requires')
        text = pathlib.Path(path).read_text(errors='replace') if os.path.isfile(path) else ''
        if need and text.count(need['text']) < need['count']:
            why.append(f"{path} runs in one process: {need['why']}")
        else:
            args.append(f"--split={os.path.basename(path)[:-3]}={v['processes']}")
    return args, why


def exec_plan(plan_path):
    """Run a plan on this build host; print one MARK line with the steps and every runner's results."""
    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):  # stopped: clean up and leave
        signal.signal(sig, _stop)
    out = {'steps': [], 'runs': [], 'notes': []}

    def add(step, runner=None):
        out['steps'].append({'name': step.name, 'secs': round(step.secs, 1), 'ok': bool(step.ok), 'note': step.note, 'tail': step.tail})
        if runner is not None:
            out['runs'].append(runner)

    # Everything this run creates is named before it exists and belongs to this process alone (no lock file, no
    # shared directory), so `finally` removes it without asking who made it, whatever moment a stop comes at.
    tmp = f'/var/tmp/x{os.getpid()}'  # short (unix sockets: 108 bytes; fixed C buffers), on disk (not a tmpfs /tmp)
    made_git = not os.path.exists('.git')  # the staging repository this run makes (tests call git ls-files)
    try:
        plan = json.loads(pathlib.Path(plan_path).read_text())
        if os.uname().machine not in ('aarch64', 'arm64'):
            s = Step('host'); s.ok, s.tail = False, f'tools/dev/test.sh runs on an ARM build host (this is {os.uname().machine})'
            add(s)
            return 0
        os.nice(5)
        shutil.rmtree(tmp, ignore_errors=True)  # a dead process's leftover with this pid
        os.mkdir(tmp); os.chmod(tmp, 0o1777)
        env = dict(os.environ, TMPDIR=tmp, GOFLAGS=(os.environ.get('GOFLAGS', '') + ' -buildvcs=false').strip())
        if made_git:
            env['GIT_INDEX_FILE'] = os.path.join(tmp, 'index')
            for cmd in (['git', 'init', '-q'], ['git', 'add', '-A']):
                if spawn(Step(' '.join(cmd)), cmd, env):
                    raise RuntimeError(f"{' '.join(cmd)} failed")
        out.update(host_gcc=gcc_major(), is_root=os.geteuid() == 0, host=socket.gethostname())
        print(f'[exec] {out["host"]}: gcc {out["host_gcc"]}, root {out["is_root"]}', file=sys.stderr, flush=True)

        def runner_step(name, cmd, json_path, step_env=None):
            s = Step(name)
            spawn(s, cmd, step_env or env)
            r = results.load(json_path)
            if r is None:  # the runner died before it wrote anything: that is a failure
                r = {'ok': False, 'results': [], 'errors': [f'{name}: no result ({s.tail[-1500:]})'], 'notes': []}
            n = {st: sum(1 for x in r['results'] if x['status'] == st) for st in results.STATUSES}
            s.note = f"{len(r['results'])} tests: {n['pass']} passed, {n['fail']} failed, {n['skip']} skipped"
            if not r.get('ok') and s.ok:
                s.ok = False
            add(s, r)
            return r

        # Go selection: the plan's static closure, plus whatever `go list` finds that it missed.
        go_dirs = list(plan['go'])
        if plan['go_code'] and not plan['go_all']:
            gl = gotree.go_list_closure('.', set(plan['go_code']), env=env)
            if gl is not None:
                extra = sorted(set(gl) - set(go_dirs))
                out['go_added_by_go_list'] = extra
                go_dirs += extra
        # A package built only under a tag (tools/az01-packer's fxselector_sandbox server, tools/shadow-twin's `ignore`
        # generator) cannot be built as a package, not even with the tag (C fixtures share its directory; `-tags ignore`
        # pulls in GOROOT's own generators). Its files are vetted by name, the way its wrapper builds them (`go build
        # FILE.go`, `go run FILE.go`), in a narrow and a full run alike. One with test files, or with a constraint other
        # than plain tag names (an OS, a negation), is NOT RUN.
        check = sorted(gotree.packages('.')) if plan['go_all'] else go_dirs
        tagged = gotree.tag_only('.', check, env=env) if check else {}
        if tagged is None:
            s = Step('go list'); s.ok, s.tail = False, 'go list ./... failed: which packages build only under a tag is unknown'
            add(s)
            tagged = {}
        by_file, r = [], results.Results()
        for d, exprs in sorted(tagged.items()):
            files = sorted(os.path.join(d, f) for f in os.listdir(d) if f.endswith('.go'))
            if exprs and all(re.fullmatch(r'[\w.]+', e.strip()) for e in exprs) and not any(f.endswith('_test.go') for f in files):
                by_file.append((d, exprs, files))
            else:
                r.add('go', d, '*', 'notrun', 0, f"built only under {' or '.join(exprs) or 'a build constraint'}: not run here")
        if r.items:
            s = Step(f'go: {len(r.items)} constrained packages'); s.ok, s.note = True, 'NOT RUN: ' + ' '.join(x['where'] for x in r.items)
            add(s, r.as_dict())
        go_dirs = [d for d in go_dirs if d not in tagged]
        pk = ['./...'] if plan['go_all'] else [f'./{d}' if d != '.' else '.' for d in sorted(set(go_dirs))]
        smb = gotree.smb_tests('.', sorted(gotree.packages('.')) if plan['go_all'] else sorted(set(go_dirs))) if pk else []
        out['by_name'] = [['go', d, n] for d, n in smb]  # run by name (gorun --only): each must pass itself
        smb_file = os.path.join(tmp, 'smb.txt')
        pathlib.Path(smb_file).write_text(''.join(f'{d} {n}\n' for d, n in smb))
        if plan['gofmt'] is None or plan['gofmt']:
            s = Step('gofmt'); spawn(s, ['gofmt', '-l'] + (['.'] if plan['gofmt'] is None else plan['gofmt']), env)
            if s.ok and s.out.strip():
                s.ok, s.tail = False, 'not gofmt-clean:\n' + s.out
            add(s)
        if pk:
            s = Step(f'go vet ({len(pk)} pkg args)'); spawn(s, ['go', 'vet'] + pk, env); add(s)
        for item in plan['py']:
            d = item['dir']; out_json = os.path.join(tmp, 'py-' + d.replace('/', '_') + '.json')
            split, why = split_args(plan['split'], d)
            r = runner_step(f'py {d}', [sys.executable, os.path.join(HERE, 'pyrun.py'), d] + item['specs'] + split + [
                '--json', out_json, '--seed', os.path.join(HERE, 'py-slow.json')], out_json)
            r['notes'] += why
        if pk:
            out_json = os.path.join(tmp, 'go.json')
            runner_step('go test -race', [sys.executable, os.path.join(HERE, 'gorun.py'), '--json', out_json]
                        + (['--no-cache'] if plan['no_cache'] else []) + (['--skip', smb_file] if smb else []) + pk, out_json)
        for d, exprs, files in by_file:
            s = Step(f'go vet {d} (by file name)'); spawn(s, ['go', 'vet'] + files, env)
            s.note = s.note or f"built only under {' or '.join(exprs)}: vetted as its wrapper builds it"
            add(s)
        if smb:
            why = samba_missing()
            if why:
                r = results.Results()
                for d, n in smb:
                    r.add('go', d, n, 'notrun', 0, f'Samba fixture test: {why}')
                s = Step(f'go test -race (Samba, {len(smb)} tests)'); s.ok, s.note = True, f'NOT RUN: {why}'
                add(s, r.as_dict())
            else:
                # The fixture's server is "localhost": with the build host's own name every SMB login times out.
                out_json = os.path.join(tmp, 'go-smb.json')
                pkgs = sorted({f'./{d}' if d != '.' else '.' for d, _ in smb})
                runner_step(f'go test -race (Samba, private UTS ns, {len(smb)} tests)', uts_localhost(
                    [sys.executable, os.path.join(HERE, 'gorun.py'), '--only', smb_file, '--json', out_json, '-p', '1']
                    + (['--no-cache'] if plan['no_cache'] else []) + pkgs), out_json)
        if plan['c']:
            out_json = os.path.join(tmp, 'c.json')
            runner_step(f"C tests ({len(plan['c'])})", [sys.executable, os.path.join(HERE, 'crun.py'), '--json', out_json,
                                                         '-j', str(plan['split']['c']['processes'])] + plan['c'], out_json)
        for script in sorted(plan['sh'], key=lambda x: x == testmap.OVERLAY):  # the overlay test last, without .git
            info = testmap.SH_TESTS['auto'].get(script, {})
            s = Step(f'sh {script}')
            r = results.Results()
            why_not = []
            if 'root' in info.get('needs', []) and os.geteuid() != 0:
                why_not.append('needs root')
            if 'gcc14' in info.get('needs', []) and out['host_gcc'] != '14':
                why_not.append(f'needs GCC 14 (host has {out["host_gcc"]})')
            if why_not:
                r.add('sh', script, '*', 'notrun', 0, ', '.join(why_not))
                s.ok, s.note = True, 'NOT RUN: ' + ', '.join(why_not)
                add(s, r.as_dict())
                continue
            sh_env = dict(env)
            if script == testmap.OVERLAY and made_git:  # the dev line runs it without the throwaway repository
                sh_env.pop('GIT_INDEX_FILE', None)
                shutil.rmtree('.git', ignore_errors=True)
                made_git = False
            rc = spawn(s, ['bash', script], sh_env, timeout=int(os.environ.get('ENGINEX_SH_TIMEOUT', '3600')))
            r.add('sh', script, '*', 'pass' if rc == 0 else 'fail', s.secs, s.tail if rc else '')
            add(s, r.as_dict())
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
        if made_git:
            shutil.rmtree('.git', ignore_errors=True)
        print(MARK + json.dumps(out), flush=True)
    stopped()
    return 0


# ---------------------------------------------------------------- host choice and the remote run

def lab_path(root):
    if os.environ.get('ENGINEX_LAB'):
        return os.environ['ENGINEX_LAB']
    try:
        m = re.search(r"^legacy_lab='([^']+)'", pathlib.Path(root, 'rootfs-overlay/usr/libexec/enginex-migration').read_text(), re.M)
        return m.group(1) if m else ''
    except OSError:
        return ''


def probe(host, needs, lab, prefer_samba=False):
    """(score, reason): lower is better; None when the host lacks something the plan needs."""
    sshctl = shutil.which('sshctl')
    if not sshctl:
        return 0, 'no sshctl: not probed'
    checks = ' '.join(f"[ -e '{lab}/{testmap.C_TESTS['engines'][n.split(':')[1]]}' ] && echo has:{n} ;" for n in needs if n.startswith('lab:'))
    script = (". /etc/profile.d/buildtools.sh 2>/dev/null; echo gcc:$(gcc -dumpversion); echo arch:$(uname -m); "
              f"[ -d '{lab}' ] && echo has:lab; {checks} "
              "echo load:$(cut -d' ' -f1 /proc/loadavg); echo cpus:$(nproc); "
              "echo heavy:$(ps -eo args= | grep -c '[f]lock.*enginex-heavy'); "
              "command -v enginex-run >/dev/null 2>&1 && echo has:enginex-run; "
              "command -v smbd >/dev/null 2>&1 && command -v smbpasswd >/dev/null 2>&1 && echo has:samba")
    out = run([sshctl, 'run', host, '--no-stdin', '--timeout', '20s', '--argv', 'sh', '-c', script]).stdout
    kv = dict(line.split(':', 1) for line in out.splitlines() if ':' in line and not line.startswith('ssm'))
    if not kv:
        return None, 'unreachable'
    if kv.get('arch') not in ('aarch64', 'arm64'):
        return None, f"not ARM ({kv.get('arch')})"
    have = {line[4:] for line in out.splitlines() if line.startswith('has:')}
    if 'enginex-run' not in have:
        return None, 'no enginex-run (never run unlocked)'
    if 'gcc14' in needs and kv.get('gcc', '').split('.')[0] != '14':
        return None, f"gcc {kv.get('gcc')}"
    for n in needs:
        if (n == 'lab' or n.startswith('lab:') or n == 'samba') and n not in have:
            return None, f'no {n}'
    try:
        load, cpus, heavy = float(kv.get('load', 9)), int(kv.get('cpus', 1)), int(kv.get('heavy', 0))
    except ValueError:
        return None, 'unparsable probe'
    samba = 'samba' in have
    # Samba fixture tests in the plan: prefer a host that can run them (a busy one still loses: 10 per heavy job).
    return heavy * 10 + load / cpus + (5 if prefer_samba and not samba else 0), \
        f"load {load}/{cpus} cpus, {heavy} heavy-lock jobs{', Samba' if samba else ''}"


def pick_host(hosts, needs, lab, prefer_samba=False):
    best, best_score, notes = None, None, []
    for h in hosts:
        score, why = probe(h, needs, lab, prefer_samba)
        notes.append(f'{h}: {why}')
        if score is not None and (best is None or score < best_score):
            best, best_score = h, score
    return best, notes


def remote_command(mode, inner):
    """The command the build host runs: inside enginex-run, or not at all (a missing enginex-run is an error)."""
    return (f'command -v enginex-run >/dev/null 2>&1 || {{ echo "test.sh: enginex-run is missing on this host; '
            f'refusing to run without the shared lock" >&2; exit 2; }}; exec enginex-run {mode} -- {inner}')


def remote(host, name, command, root, timeout):
    """remote-build to host; returns (returncode, output)."""
    rb = os.environ.get('REMOTE_BUILD_BIN') or shutil.which('remote-build')
    if not rb:
        return 2, 'remote-build is required\n'
    env = dict(os.environ, REMOTE_BUILD_HOST=host, REMOTE_BUILD_TIMEOUT=timeout)
    p = subprocess.run([rb, '-C', root, '--name', name, '--clean', '--', command], env=env, stdout=subprocess.PIPE,
                       stderr=subprocess.STDOUT, text=True, errors='replace')
    sshctl = shutil.which('sshctl')
    if sshctl and not os.environ.get('ENGINEX_KEEP_REMOTE'):  # the exact directory remote-build created
        run([sshctl, 'run', host, '--no-stdin', '--timeout', '60s', '--argv', 'rm', '-rf', '--', f'/root/build/{name}'])
    return p.returncode, p.stdout


def parse_result(out):
    for line in reversed(out.splitlines()):
        if line.startswith(MARK):
            try:
                return json.loads(line[len(MARK):])
            except ValueError:
                return None
    return None


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--full', action='store_true'); ap.add_argument('--no-cache', action='store_true')
    ap.add_argument('--strict', action='store_true', help='accepted for older callers (tests are never rerun)')
    ap.add_argument('--base'); ap.add_argument('--into', default=os.environ.get('ENGINEX_TEST_TARGET', DEFAULT_INTO))
    ap.add_argument('--tree', help='tree under test (default: the git checkout of the current directory)')
    ap.add_argument('--host', help='this build host (default: the least busy of ENGINEX_BUILD_HOSTS that qualifies)')
    ap.add_argument('--exclusive', action='store_true', help='enginex-run exclusive instead of sandbox (merge train, timing)')
    ap.add_argument('--require-samba', action='store_true', help='only a host with smbd and smbpasswd (the Samba fixture tests run)')
    ap.add_argument('--accept-incomplete', action='store_true', help='PASS although listed tests did not run here (the lane ran them)')
    ap.add_argument('--list', action='store_true'); ap.add_argument('--json', action='store_true')
    ap.add_argument('--report')
    ap.add_argument('--exec', dest='exec_plan')
    a = ap.parse_args(argv)
    load_env()
    if a.exec_plan:
        return exec_plan(a.exec_plan)
    started = time.monotonic()
    try:
        root = os.path.abspath(a.tree) if a.tree else git(os.getcwd(), 'rev-parse', '--show-toplevel').strip()
        git(root, 'rev-parse', '--git-dir')
        plan = make_plan(a, root)
    except PlanError as e:
        print(f'test.sh: {e}', file=sys.stderr)
        print('RESULT: FAIL  (no plan: the change set is unknown)')
        return 2
    if a.list:
        if a.json:
            print(json.dumps(plan, indent=1))
        else:
            print_plan(plan)
        return 0
    print_plan(plan)
    hosts = [a.host] if a.host else os.environ.get('ENGINEX_BUILD_HOSTS', '').replace(',', ' ').split()
    if not hosts:
        print('test.sh: ENGINEX_BUILD_HOSTS is not set (environment or ~/.config/enginex/dev.env)', file=sys.stderr); return 2
    lab = lab_path(root)
    host, notes = pick_host(hosts, plan['needs'], lab, prefer_samba=bool(plan.get('smb')))
    print('hosts: ' + '; '.join(notes))
    if not host:
        print(f"test.sh: no build host has what this plan needs ({', '.join(plan['needs']) or 'enginex-run'})", file=sys.stderr)
        print('RESULT: FAIL  (no build host qualifies)')
        return 1
    plan_name = f'{PLAN_PREFIX}-{os.getpid()}.json'
    plan_path = os.path.join(root, plan_name)
    tools_copy = None
    # One stable remote directory per tree (Go's build cache keys on the path), a private one when it is busy.
    name = 'enginex-test-' + re.sub(r'[^A-Za-z0-9._-]', '_', os.path.basename(root))[:40] + '-' + hashlib.sha1(root.encode()).hexdigest()[:6]
    state = os.path.expanduser(os.environ.get('ENGINEX_DEV_STATE', '~/.cache/enginex-dev')); os.makedirs(state, exist_ok=True)
    guard = open(os.path.join(state, name + '.lock'), 'w')
    try:
        fcntl.flock(guard, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        name += f'-{os.getpid()}'
    # Here a stop may raise anywhere: what this side makes (the plan file, the runner copy) is named before it is made.
    signal.signal(signal.SIGTERM, lambda signum, frame: sys.exit(128 + signum))
    try:
        pathlib.Path(plan_path).write_text(json.dumps(plan))
        # The tree may not carry this runner yet (a lane branch older than it): run a copy of this one, synced with it.
        if not os.path.isfile(os.path.join(root, 'tools/dev/devtest.py')) or not filecmp.cmp(os.path.join(root, 'tools/dev/devtest.py'), __file__, shallow=False):
            tools_copy = os.path.join(root, f'{TOOLS_PREFIX}-{os.getpid()}')
            shutil.copytree(HERE, tools_copy, ignore=shutil.ignore_patterns('__pycache__', 'test_*'))
            print(f'runner: this tree has no (or another) tools/dev runner; using a copy of {HERE}')
        runner = os.path.relpath(tools_copy, root) if tools_copy else 'tools/dev'
        mode = 'exclusive' if a.exclusive else 'sandbox'
        print(f'run: {host} ({mode}), /root/build/{name}', flush=True)
        try:
            rc, out = remote(host, name, remote_command(mode, f'bash {runner}/test.sh --exec {plan_name}'), root,
                             os.environ.get('ENGINEX_TEST_TIMEOUT', '150m'))
        except BaseException:
            # Interrupted here: stop this run on the host too (the plan file name is unique to it), then remove its copy.
            sshctl = shutil.which('sshctl')
            if sshctl:
                run([sshctl, 'run', host, '--no-stdin', '--timeout', '30s', '--argv', 'pkill', '-TERM', '-f', plan_name])
                time.sleep(3)
                run([sshctl, 'run', host, '--no-stdin', '--timeout', '60s', '--argv', 'rm', '-rf', '--', f'/root/build/{name}'])
            print(f'test.sh: interrupted; stopped the run on {host}', file=sys.stderr)
            raise
        res = parse_result(out)
        if res is None:
            res = {'steps': [{'name': 'remote run', 'secs': round(time.monotonic() - started, 1), 'ok': False,
                              'note': f'host {host}, exit {rc}', 'tail': out[-4000:]}], 'runs': []}
        elif rc:
            res['steps'].append({'name': 'remote run', 'secs': 0, 'ok': False, 'note': f'exit {rc}', 'tail': out[-2000:]})
    finally:
        try:
            os.remove(plan_path)
        except OSError:
            pass
        if tools_copy:
            shutil.rmtree(tools_copy, ignore_errors=True)  # only the copy made above
        guard.close()
    res['host'] = host
    return report(a, plan, res, host, started)


def verdict(plan, res, accept_incomplete=False):
    """(status, exit code, lines, results, failed results). PASS only when every selected test produced a pass."""
    lines = []
    items = [x for r in res.get('runs', []) for x in r.get('results', [])]
    errors = [e for r in res.get('runs', []) for e in r.get('errors', [])]
    failed = [x for x in items if x['status'] == 'fail']
    bad_steps = [s for s in res.get('steps', []) if not s['ok']]
    named = {tuple(c) for c in plan.get('changed_tests', []) + res.get('by_name', [])}  # changed, or run by name
    missing = []  # selected by name, but no pass
    for kind, where, test in sorted(named):
        mine = [x for x in items if x['kind'] == kind and x['where'] == where and (
            kind in ('c', 'sh') or x['test'] == test or kind == 'py' and pyrun.matches(x['test'], [test]))]  # py: a module
        if not mine:
            missing.append(f'{kind} {where} {test}: no result')
        elif any(x['status'] == 'skip' for x in mine):  # a not-run is listed below; a failure fails
            missing.append(f"{kind} {where} {test}: skip (" + '; '.join((x['test'] + ': ' if x['test'] != test else '')
                           + x['reason'][:120] for x in mine if x['status'] == 'skip')[:600] + ')')
    missing += [f"{x['kind']} {x['where']} {x['test']}: did not run ({x['reason']})" for x in items if x['status'] == 'notrun']
    missing += [f"manual: {p}: {i['how']}" for p, i in plan.get('manual', {}).items()]

    def needs_a_pass(label, mine):  # a selected target: one of its tests must pass
        if not any(x['status'] in ('pass', 'fail', 'notrun') for x in mine):  # a failure fails; a not-run is listed above
            missing.append(f'{label}: ' + ('; '.join(sorted({f"skip ({x['reason'][:120]})" for x in mine})[:3]) or 'no result'))
    for item in plan.get('py', []):
        for spec in item['specs'] or [None]:
            m = spec and pyrun.norm(spec)
            needs_a_pass(f"py {item['dir']} {spec or '(all)'}", [x for x in items if x['kind'] == 'py' and x['where'] ==
                         item['dir'].rstrip('/') and (not m or pyrun.matches(x['test'], [m]))])
    for d in sorted({d for r in res.get('runs', []) for d, k in r.get('packages', {}).items() if k == 'tested'}):
        needs_a_pass(f'go {d}', [x for x in items if x['kind'] == 'go' and x['where'] == d])
    for kind in ('c', 'sh'):
        for where in plan.get(kind, []):
            needs_a_pass(f'{kind} {where}', [x for x in items if x['kind'] == kind and x['where'] == where])
    if failed or errors or bad_steps:
        status, code = 'FAIL', 1
    elif missing and not accept_incomplete:
        status, code = 'INCOMPLETE', EXIT_INCOMPLETE
    else:
        status, code = 'PASS', 0
    for e in errors:
        lines.append('ERROR: ' + e[:3000])
    if missing:
        lines.append(f"{'accepted as run by the lane' if accept_incomplete else 'without a pass here'}: {len(missing)} selected tests:")
        lines += ['  ' + m for m in missing]
    return status, code, lines, items, failed


def report(a, plan, res, host, started):
    print(f"\n== tools/dev/test.sh {plan['mode']}{' vs ' + plan['base'][:12] if plan['base'] else ''}"
          f" ({len(plan['changed'])} changed files){' on ' + host if host else ''} ==")
    print(f"{'step':40} {'wall':>8}  result")
    for s in res.get('steps', []):
        print(f"{s['name'][:40]:40} {s['secs']:7.1f}s  {'ok' if s['ok'] else 'FAILED'}{'  ' + s['note'] if s['note'] else ''}")
    for key, text in (('go_added_by_go_list', 'go list added packages the static map missed'),):
        if res.get(key):
            print(f"{text}: {' '.join(res[key])}")
    for r in res.get('runs', []):
        for n in r.get('notes', []):
            print('note: ' + n)
    status, code, lines, items, failed = verdict(plan, res, a.accept_incomplete)
    skipped = [x for x in items if x['status'] == 'skip']
    print(f'\nskipped: {len(skipped)}' + ''.join(f"\n  {x['kind']} {x['where']} {x['test']}  [{x['reason'][:80]}]" for x in skipped[:40]))
    for line in lines:
        print(line)
    for x in failed[:30]:
        print(f"\n----- FAILED {x['kind']} {x['where']} {x['test']} -----\n{x['reason'][-3000:]}")
    for s in res.get('steps', []):
        if not s['ok'] and s['tail']:
            print(f"\n----- {s['name']} (last lines) -----\n{s['tail']}")
    if a.report:
        os.makedirs(a.report, exist_ok=True)
        pathlib.Path(a.report, 'test-run.json').write_text(json.dumps({'plan': plan, 'result': res}, indent=1))
    print(f"\nRESULT: {status}  ({len(items)} tests, wall {time.monotonic() - started:.1f}s)")
    return code


if __name__ == '__main__':
    sys.exit(main())
