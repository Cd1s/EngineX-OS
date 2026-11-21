"""The one path-to-tests map used by tools/dev/test.sh (and through it by tools/dev/integrate.sh).

map_paths(paths) -> Plan: which Go package directories, Python test files and shell tests a set of changed files
touches. Only the direct mapping lives here; the reverse-dependency closure of Go packages is added by devtest.py.
changed_tests(...) lists the tests a diff added or changed, so a skipped one can fail the run as "not proven".
"""
import pathlib, ast, os, re, subprocess

FULL_PY_DIRS = ['tools/az01-packer', 'tools/splash', 'tools/stems', 'tools/dev']  # what --full runs through pyrun
FULL_SH = ['tests/test_rootfs_overlay.sh', 'tools/dev/test_integrate.sh']  # pure bash tests, run by --full


class Plan:
    def __init__(self):
        self.go_all = False
        self.go = set()      # package dirs relative to the module root ('.' for the root package)
        self.py = set()      # (dir, test file name)
        self.sh = set()      # bash tests, repo relative

    def empty(self):
        return not (self.go_all or self.go or self.py or self.sh)


def _py(plan, path):
    plan.py.add((os.path.dirname(path), os.path.basename(path)))


def map_path(plan, path, root='.'):
    d, name = os.path.dirname(path), os.path.basename(path)
    if path in ('go.mod', 'go.sum'):
        plan.go_all = True
    elif name.endswith('.go'):
        plan.go.add(d or '.')
    elif '/testdata/' in '/' + path:  # fixtures belong to the nearest Go package above them
        pkg = ('/' + path).split('/testdata/')[0].lstrip('/')
        while pkg and not any(f.endswith('.go') for f in _listdir(root, pkg)):
            pkg = os.path.dirname(pkg)
        if pkg or any(f.endswith('.go') for f in _listdir(root, '.')):
            plan.go.add(pkg or '.')
    elif path == 'tools/ui-mod/build_packs.py':
        _py(plan, 'tools/az01-packer/test_ui_mod.py'); _py(plan, 'tools/az01-packer/test_ui_perf.py')
    elif d == 'tools/az01-packer' and name.endswith('.py'):
        if name.startswith('test_'):
            _py(plan, path)
        else:
            _py(plan, 'tools/az01-packer/test_az01_packer.py')
            own = 'test_' + name[:-3].replace('-', '_') + '.py'
            if os.path.exists(os.path.join(root, d, own)):
                _py(plan, f'{d}/{own}')
    elif d == 'tools/stems' and name.endswith('.py'):
        _py(plan, path if name.startswith('test_') else 'tools/stems/test_worker.py')
    elif d == 'tools/splash' and name.endswith('.py'):
        _py(plan, path if name.startswith('test_') else 'tools/splash/test_splash_tool.py')
    elif d == 'tools/dev' and (name.endswith('.py') or name.endswith('.sh') or name.endswith('.txt') or name.endswith('.json')):
        _py(plan, 'tools/dev/test_devtools.py')
        if name in ('integrate.sh', 'test_integrate.sh'):
            plan.sh.add('tools/dev/test_integrate.sh')
    elif re.match(r'(.*/)?test_[^/]*\.py$', path):
        _py(plan, path)
    elif path.startswith('rootfs-overlay/') or path == 'tests/test_rootfs_overlay.sh':
        plan.sh.add('tests/test_rootfs_overlay.sh')
    elif path.startswith('docs/') or re.match(r'README(\.[\w-]+)?\.md$', path):
        _py(plan, 'tools/az01-packer/test_docs_audit.py')


def _listdir(root, rel):
    try:
        return os.listdir(os.path.join(root, rel))
    except OSError:
        return []


def map_paths(paths, root='.'):
    plan = Plan()
    for p in paths:
        map_path(plan, p, root)
    return plan


def full_plan():
    plan = Plan()
    plan.go_all = True
    plan.py = {(d, None) for d in FULL_PY_DIRS}  # None: every test file of the directory
    plan.sh = set(FULL_SH)
    return plan


def _hunks(root, base, path):
    """New-file line ranges changed against base (an untracked file is one big hunk)."""
    tracked = subprocess.run(['git', '-C', root, 'ls-files', '--error-unmatch', path], capture_output=True).returncode == 0
    if not tracked:
        return [(1, 10 ** 9)]
    out = subprocess.run(['git', '-C', root, 'diff', '-U0', '--no-renames', base, '--', path], capture_output=True, text=True).stdout
    rng = []
    for m in re.finditer(r'^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@', out, re.M):
        start, count = int(m.group(1)), int(m.group(2) if m.group(2) is not None else 1)
        rng.append((start, start + max(count, 1) - 1))
    return rng


def _go_funcs(text):
    funcs, name, start = [], None, 0
    for i, line in enumerate(text.splitlines(), 1):
        m = re.match(r'func (?:\([^)]*\) )?(Test\w*|Example\w*|Fuzz\w*)\(', line)
        if m:
            name, start = m.group(1), i
        elif line.startswith('}') and name:
            funcs.append((name, start, i)); name = None
    return funcs


def _py_funcs(text, module):
    out = []
    try:
        tree = ast.parse(text)
    except SyntaxError:
        return out
    for cls in (n for n in ast.walk(tree) if isinstance(n, ast.ClassDef)):
        for fn in cls.body:
            if isinstance(fn, (ast.FunctionDef, ast.AsyncFunctionDef)) and fn.name.startswith('test'):
                first = min([fn.lineno] + [d.lineno for d in fn.decorator_list])
                out.append((f'{module}.{cls.name}.{fn.name}', first, fn.end_lineno))
    return out


def changed_tests(root, base, paths):
    """Tests a diff added or changed: ('go', pkg-dir, TestName) and ('py', dir, 'module.Class.method')."""
    found = set()
    for path in paths:
        full = os.path.join(root, path)
        if not os.path.isfile(full):
            continue
        is_go, is_py = path.endswith('_test.go'), re.match(r'(.*/)?test_[^/]*\.py$', path)
        if not (is_go or is_py):
            continue
        text = pathlib.Path(full).read_text(encoding='utf-8', errors='replace')
        hunks = _hunks(root, base, path)
        if is_go:
            funcs = [(('go', os.path.dirname(path) or '.', n), a, b) for n, a, b in _go_funcs(text)]
        else:
            funcs = [(('py', os.path.dirname(path), n), a, b) for n, a, b in _py_funcs(text, os.path.basename(path)[:-3])]
        for key, a, b in funcs:
            if any(lo <= b and hi >= a for lo, hi in hunks):
                found.add(key)
    return found
