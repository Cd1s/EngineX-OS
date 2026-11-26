"""The one path-to-tests map used by tools/dev/test.sh (and through it by tools/dev/integrate.sh).

map_paths(paths, root) -> Plan: which Go package directories, Python test files, bash tests, C tests and manual
(sandbox) tests a set of changed files touches, and why. The reverse-dependency closure of Go packages is added by
devtest.py (tools/dev/gotree.py). changed_tests(...) lists the tests a diff added or changed, so one that did not
run fails the run as "not proven".

Rules, in this order (a file can select from several):
  go.mod / go.sum                      every Go package
  *.go, testdata/, data files of a Go package with tests     that package (devtest adds every importer)
  tools/ui-mod/build_packs.py, tools/az01-packer/test_ui_mod.py and the pack's data files (not C, not docs)
                                       the WHOLE test_ui_mod.py (sharded by pyrun, never a subset) + test_ui_perf.py
  a Python module                      its own test_<name>.py and every test_*.py of its directory that imports it,
                                       plus the directory's catch-all (test_az01_packer.py, tools/stems/test_worker.py,
                                       tools/splash/test_splash_tool.py)
  C sources (.c/.h)                    every C test that includes the file, directly or through other sources
                                       (tools/dev/c-tests.json: the release checklist's variants for its six tests,
                                       the armhf way for the other tools/ui-mod tests), and, when the shipped hooks
                                       include it, tests/test_rootfs_overlay.sh (it rebuilds both hooks)
  rootfs-overlay/, the hook build scripts             tests/test_rootfs_overlay.sh
  docs/, READMEs                       tools/az01-packer/test_docs_audit.py
  tools/dev/                           tools/dev/test_devtools.py (+ test_integrate.sh for the merge train)
  any file a test names                every test file (Go, Python, bash, C) that names it by repository path or by a
                                       distinctive file name, or (data and scripts) names its directory of three or
                                       more components when at most three tests do; for C files also the sources
                                       that include it. A Go package whose tests name the file runs its own tests.
A selected bash or C file that tools/dev/sh-tests.json lists as manual is reported with its command (an Engine
sandbox: the lane runs it), never dropped. A file nothing selects is listed as unmapped.
"""
import ast, json, os, pathlib, re, subprocess

HERE = os.path.dirname(os.path.abspath(__file__))
FULL_PY_DIRS = ['tools/az01-packer', 'tools/splash', 'tools/stems', 'tools/dev']  # what --full runs through pyrun
UI_MOD_TEST = ('tools/az01-packer', 'test_ui_mod.py')
UI_PERF_TEST = ('tools/az01-packer', 'test_ui_perf.py')
OVERLAY = 'tests/test_rootfs_overlay.sh'
HOOK_SOURCES = ('tools/ui-mod/engine_ui_hook.c', 'tools/edisksd-hook/edisksd_hook.c')
HOOK_BUILDS = ('tools/ui-mod/build.sh', 'tools/edisksd-hook/build.sh')
C_TEST_DIRS = ('tools/ui-mod', 'tools/edisksd-hook')
GENERIC = {'main.go', 'doc.go', 'build.sh', 'README.md', 'Makefile', 'go.mod', 'go.sum', 'index.html', 'content.js',
           'app.js', 'style.css', 'config.json', 'version.txt', '__init__.py', 'settings.conf', 'LICENSE'}
_TOKEN = re.compile(r'[A-Za-z0-9_][A-Za-z0-9_./-]*')
DIR_MENTION_LIMIT = 3
MENTION_EXCLUDE = {'tools/dev/test_devtools.py'}  # its file names are fixtures, not dependencies  # a directory more test files name is a broad one (tools/ui-mod/...), not a reference


def _load(name):
    return json.loads(pathlib.Path(HERE, name).read_text())


C_TESTS = _load('c-tests.json')
SH_TESTS = _load('sh-tests.json')
FULL_SH = sorted(SH_TESTS['auto'])  # every automatic bash test, run by --full


class Plan:
    def __init__(self):
        self.go_all = False
        self.go = set()      # package dirs to test, relative to the module root ('.' for the root package)
        self.go_code = set() # of those, the ones whose non-test code or embedded data changed: devtest adds importers
        self.py = set()      # (dir, test file name); file None = the whole directory
        self.sh = set()      # automatic bash tests, repo relative
        self.c = set()       # C test sources (tools/dev/c-tests.json), every variant
        self.manual = {}     # path -> {'how', 'why'}: selected tests the runner does not run (Engine sandboxes)
        self.unmapped = []   # changed files nothing selects
        self.why = {}        # selection key -> reasons (changed files)
        self.hits = 0        # selections made (map_path lists a file that made none as unmapped)

    def empty(self):
        return not (self.go_all or self.go or self.py or self.sh or self.c)

    def add_go(self, d, reason, code):
        self.go.add(d)
        if code:
            self.go_code.add(d)
        self.note('go ' + d, reason)

    def note(self, key, reason):
        self.hits += 1
        r = self.why.setdefault(key, [])
        if reason not in r and len(r) < 6:
            r.append(reason)


# ---------------------------------------------------------------- repository index (cached per root)

_INDEX = {}


def _git_files(root):
    p = subprocess.run(['git', '-C', root, 'ls-files', '-co', '--exclude-standard'], capture_output=True, text=True)
    if p.returncode == 0 and p.stdout.strip():
        return sorted(set(p.stdout.split('\n')) - {''})
    out = []
    for d, dirs, files in os.walk(root):
        dirs[:] = [x for x in dirs if x not in ('.git', 'node_modules')]
        out += [os.path.relpath(os.path.join(d, f), root) for f in files]
    return sorted(out)


def is_test_file(path):
    name = os.path.basename(path)
    return bool(re.match(r'test_[^/]*\.(py|sh|c)$', name) or name.endswith('_test.go') or
                (path.startswith('tests/') and name.endswith('.sh')))


class Index:
    def __init__(self, root):
        self.root = root
        self.files = [f for f in _git_files(root) if os.path.isfile(os.path.join(root, f))]
        self.fileset = set(self.files)
        self.tests = [f for f in self.files if is_test_file(f) or f in SH_TESTS['auto'] or f in SH_TESTS['manual']]
        self.testset = set(self.tests)
        self._text = {}
        self._tok = self._seg = None
        self.go_test_dirs = {os.path.dirname(f) or '.' for f in self.files if f.endswith('_test.go')}
        self.go_dirs = {os.path.dirname(f) or '.' for f in self.files if f.endswith('.go')}
        self.c_files = [f for f in self.files if f.startswith('tools/') and f.endswith(('.c', '.h'))]
        self.includers = {}  # source -> C files that #include it (directly)
        for f in self.c_files:
            for inc in re.findall(r'^\s*#\s*include\s+"([^"]+)"', self.text(f), re.M):
                target = os.path.normpath(os.path.join(os.path.dirname(f), inc))
                self.includers.setdefault(target, set()).add(f)

    def tokens(self):
        """{test file: its path-like tokens} and {path segment: test files}, built once."""
        if self._seg is None:
            self._tok, self._seg = {}, {}
            for t in self.tests:
                toks = set(_TOKEN.findall(self.text(t)))
                self._tok[t] = toks
                for tok in toks:
                    for s in tok.split('/'):
                        if s not in ('', '.', '..'):
                            self._seg.setdefault(s, set()).add(t)
        return self._tok, self._seg

    def text(self, rel):
        if rel not in self._text:
            try:
                self._text[rel] = pathlib.Path(self.root, rel).read_text(encoding='utf-8', errors='replace')
            except OSError:
                self._text[rel] = ''
        return self._text[rel]

    def include_closure(self, path):
        """path plus every C source that includes it, directly or through other sources."""
        seen, todo = {path}, [path]
        while todo:
            for u in self.includers.get(todo.pop(), ()):
                if u not in seen:
                    seen.add(u)
                    todo.append(u)
        return seen


def index(root):
    key = os.path.abspath(root)
    if key not in _INDEX:
        _INDEX[key] = Index(root)
    return _INDEX[key]


# ---------------------------------------------------------------- selection helpers

def _py(plan, path, reason):
    key = (os.path.dirname(path), os.path.basename(path))
    plan.py.add(key)
    plan.note('py ' + path, reason)


def _manual(plan, path, reason):
    info = SH_TESTS['manual'].get(path)
    if info is None:
        info = {'how': f'see the header of {path}', 'why': 'not in tools/dev/sh-tests.json: classify it (auto or manual)'}
    plan.manual[path] = info
    plan.note('manual ' + path, reason)


def _c_test(plan, path, reason):
    """A C test source selected: run it (c-tests.json), through its script, or report it as manual."""
    entry = C_TESTS['tests'].get(path) or {}
    if entry.get('via'):
        return _sh(plan, entry['via'], reason)
    if entry or os.path.dirname(path) == C_TESTS['default']['dir']:
        plan.c.add(path)
        plan.note('c ' + path, reason)
    else:  # a sandbox driver, or a C test without a recipe in c-tests.json
        _manual(plan, path, reason)


def _sh(plan, path, reason):
    if path in SH_TESTS['auto']:
        plan.sh.add(path)
        plan.note('sh ' + path, reason)
    else:
        _manual(plan, path, reason)


def _select_test_file(plan, t, reason):
    name = os.path.basename(t)
    if name.endswith('_test.go'):  # the test names the changed file: its package's tests, not its importers
        plan.add_go(os.path.dirname(t) or '.', reason, code=False)
    elif name.endswith('.py'):
        _py(plan, t, reason)
    elif name.endswith('.c'):
        _c_test(plan, t, reason)
    elif name.endswith('.sh'):
        _sh(plan, t, reason)
    elif t in SH_TESTS['manual']:
        _manual(plan, t, reason)


def _distinctive(name):
    stem = name.rsplit('.', 1)[0]
    return name not in GENERIC and (len(stem) >= 6 and re.search(r'[_.-]', stem) is not None or len(name) >= 14)


def _mentions(idx, names, exclude):
    """Test files that name one of `names` (repo paths) by path, by a distinctive file name, or (C test sources) by
    their stem, as bash loops over test names do."""
    tok, seg = idx.tokens()
    exclude = set(exclude) | MENTION_EXCLUDE
    out = set()
    for n in names:
        base = os.path.basename(n)
        for t in seg.get(base, ()):
            if t not in exclude and (_distinctive(base) or any(x == n or x.endswith('/' + n) for x in tok[t] if x.endswith(base))):
                out.add(t)
        if re.match(r'test_\w+\.c$', base):
            out |= {t for t in seg.get(base[:-2], ()) if t not in exclude and base[:-2] in tok[t]}
    return sorted(out)


def _dir_mentions(idx, path, exclude):
    """Test files outside a directory of path (three or more components) that name it, when at most DIR_MENTION_LIMIT do;
    the nearest directory any test names decides."""
    tok, seg = idx.tokens()
    exclude = set(exclude) | MENTION_EXCLUDE
    d = os.path.dirname(path)
    while d.count('/') >= 2:
        last, needle = os.path.basename(d), '/' + d + '/'
        hits = sorted(t for t in seg.get(last, ()) if t not in exclude and not t.startswith(d + '/')
                      and any(needle in '/' + x.strip('/') + '/' for x in tok[t] if last in x))
        if hits:
            return d, (hits if len(hits) <= DIR_MENTION_LIMIT else [])
        d = os.path.dirname(d)
    return None, []


def _py_importers(idx, path):
    """test_*.py files of path's directory that import its module (unittest discover puts the directory on sys.path)."""
    d, mod = os.path.dirname(path), os.path.basename(path)[:-3]
    rx = re.compile(r'^\s*(?:from\s+' + re.escape(mod) + r'\s+import|import\s+(?:[\w, ]*,\s*)?' + re.escape(mod) + r'\b)', re.M)
    return [t for t in idx.tests if os.path.dirname(t) == d and t.endswith('.py') and t != path and rx.search(idx.text(t))]


def map_path(plan, path, root='.'):
    idx = index(root)
    d, name = os.path.dirname(path), os.path.basename(path)
    start = plan.hits
    why = path
    if path in ('go.mod', 'go.sum'):
        plan.go_all = True
        plan.note('go ./...', why)
    elif name.endswith('.go'):  # a _test.go file changes no other package's build: no importers for it
        plan.add_go(d or '.', why, code=not name.endswith('_test.go'))
    elif '/testdata/' in '/' + path:  # fixtures belong to the nearest Go package above them
        pkg = ('/' + path).split('/testdata/')[0].lstrip('/')
        while pkg and pkg not in idx.go_dirs:
            pkg = os.path.dirname(pkg)
        if pkg or '.' in idx.go_dirs:
            plan.add_go(pkg or '.', why, code=False)
    elif (d or '.') in idx.go_test_dirs:  # embedded data, SQL, fixtures next to a tested Go package
        plan.add_go(d or '.', why, code=True)

    # The pack generator and its test: always the WHOLE test_ui_mod.py (11-26: a subset missed a guard test).
    if path in ('tools/ui-mod/build_packs.py', 'tools/az01-packer/test_ui_mod.py') or (
            d.startswith('tools/ui-mod') and not name.endswith(('.c', '.h', '.md', '.sh')) and not name.startswith('test_')):
        _py(plan, '/'.join(UI_MOD_TEST), why)
        _py(plan, '/'.join(UI_PERF_TEST), why)
    if name.endswith('.py') and not name.startswith('test_'):
        own = os.path.join(d, 'test_' + name[:-3].replace('-', '_') + '.py')
        if own in idx.fileset:
            _py(plan, own, why)
        for t in _py_importers(idx, path):
            _py(plan, t, why + ' (imported)')
        if d == 'tools/az01-packer':
            _py(plan, 'tools/az01-packer/test_az01_packer.py', why)
        elif d == 'tools/stems':
            _py(plan, 'tools/stems/test_worker.py', why)
        elif d == 'tools/splash':
            _py(plan, 'tools/splash/test_splash_tool.py', why)
    if d == 'tools/dev' and name.endswith(('.py', '.sh', '.txt', '.json')):
        _py(plan, 'tools/dev/test_devtools.py', why)
        if name in ('integrate.sh', 'test_integrate.sh', 'test.sh', 'devtest.py', 'testmap.py'):
            _sh(plan, 'tools/dev/test_integrate.sh', why)
    if path in idx.testset or is_test_file(path):
        _select_test_file(plan, path, why + ' (the test itself)')

    # C: every test that includes the file; the overlay test when a shipped hook includes it.
    closure = {path}
    if name.endswith(('.c', '.h')):
        closure = idx.include_closure(path)
        for f in sorted(closure):
            if f != path and re.match(r'test_[^/]*\.c$', os.path.basename(f)):
                _c_test(plan, f, f'{why} (included)')
    if path.startswith('rootfs-overlay/') or path == OVERLAY or path in HOOK_BUILDS or (
            name.endswith(('.c', '.h')) and closure & set(HOOK_SOURCES)):
        _sh(plan, OVERLAY, why)
    if path.startswith('docs/') or re.match(r'README(\.[\w-]+)?\.md$', path):
        _py(plan, 'tools/az01-packer/test_docs_audit.py', why)

    # Any test that names the file (or a C source that includes it), or names its directory.
    for t in _mentions(idx, sorted(closure), {path}):
        _select_test_file(plan, t, why + ' (named by the test)')
    if not name.endswith('.go') and path not in idx.testset and not is_test_file(path):
        dname, hits = _dir_mentions(idx, path, {path})
        for t in hits:
            _select_test_file(plan, t, f'{why} (the test names {dname}/)')

    if plan.hits == start:
        plan.unmapped.append(path)


def map_paths(paths, root='.'):
    plan = Plan()
    for p in paths:
        map_path(plan, p, root)
    return plan


def full_plan(root='.'):
    plan = Plan()
    plan.go_all = True
    plan.py = {(d, None) for d in FULL_PY_DIRS}  # None: every test file of the directory
    plan.sh = set(FULL_SH)
    idx = index(root)
    plan.c = {f for f in idx.files if os.path.dirname(f) in C_TEST_DIRS and re.match(r'test_[^/]*\.c$', os.path.basename(f))
              and not (C_TESTS['tests'].get(f) or {}).get('via')}
    return plan


# ---------------------------------------------------------------- tests a diff added or changed

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
            if name:  # the previous function never closed at column 0: it ends where this one starts
                funcs.append((name, start, i - 1))
            name, start = m.group(1), i
            if line.rstrip().endswith('}'):  # one-line function
                funcs.append((name, i, i)); name = None
        elif line.startswith('}') and name:
            funcs.append((name, start, i)); name = None
    if name:
        funcs.append((name, start, len(text.splitlines())))
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
    """Tests a diff added or changed: ('go', pkg-dir, TestName), ('py', dir, 'module.Class.method'), ('c', source, '*')
    and ('sh', script, '*')."""
    found = set()
    for path in paths:
        full = os.path.join(root, path)
        if not os.path.isfile(full):
            continue
        name = os.path.basename(path)
        if re.match(r'test_[^/]*\.c$', name):
            found.add(('c', path, '*'))
            continue
        if is_test_file(path) and name.endswith('.sh'):
            found.add(('sh', path, '*'))
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
