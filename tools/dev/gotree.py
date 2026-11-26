"""Go package directories and the packages that import them, for tools/dev/test.sh.

static_closure(root, dirs) needs no Go toolchain (it reads the import lines of every .go file, test files included,
and ignores build tags, so it can only select more than `go list` does). go_list_closure(root, dirs) asks `go list`.
The runner uses the static closure to print a plan anywhere and, on the build host, the union of both, reporting
any package `go list` added (a miss of the static parser).
"""
import os, re, subprocess

_BLOCK = re.compile(r'^import\s*\((.*?)^\)', re.M | re.S)
_SINGLE = re.compile(r'^import\s+(?:[\w.]+\s+)?"([^"]+)"', re.M)
_QUOTED = re.compile(r'"([^"]+)"')
_SKIP = {'.git', 'node_modules', 'vendor', 'testdata'}


def module_path(root):
    try:
        with open(os.path.join(root, 'go.mod')) as f:
            for line in f:
                if line.startswith('module '):
                    return line.split()[1].strip()
    except OSError:
        pass
    return ''


def _imports(text):
    out = set(_SINGLE.findall(text))
    for block in _BLOCK.findall(text):
        for line in block.splitlines():
            line = line.split('//')[0]
            out.update(_QUOTED.findall(line))
    return out


def packages(root):
    """{package dir (relative, '.' for the root): set of imported paths} over every directory with .go files."""
    pkgs = {}
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = sorted(d for d in dirnames if d not in _SKIP and not d.startswith(('.', '_')))
        gofiles = [f for f in filenames if f.endswith('.go')]
        if not gofiles:
            continue
        # A directory with its own go.mod (other than the root) is another module: not ours.
        if dirpath != root and 'go.mod' in filenames:
            dirnames[:] = []
            continue
        rel = os.path.relpath(dirpath, root)
        imps = pkgs.setdefault(rel, set())
        for name in gofiles:
            try:
                with open(os.path.join(dirpath, name), encoding='utf-8', errors='replace') as f:
                    head = f.read(65536)
            except OSError:
                continue
            imps |= _imports(head)
    return pkgs


def static_closure(root, dirs):
    """dirs plus every package of this module that imports one of them, directly, transitively or from its tests."""
    mod = module_path(root)
    pkgs = packages(root)
    path_of = {d: (mod if d == '.' else f'{mod}/{d}') for d in pkgs}
    dir_of = {p: d for d, p in path_of.items()}
    users = {}
    for d, imps in pkgs.items():
        for imp in imps:
            if imp in dir_of and dir_of[imp] != d:
                users.setdefault(dir_of[imp], set()).add(d)
    want = {d for d in dirs if d in pkgs}
    todo = list(want)
    while todo:
        for u in users.get(todo.pop(), ()):
            if u not in want:
                want.add(u)
                todo.append(u)
    return sorted(want)


def buildable(root, env=None):
    """Package dirs that have Go files for this host's build constraints and are not test-only stubs (go list -e).
    None if go list fails."""
    fmt = '{{.Dir}}\t{{len .GoFiles}}\t{{len .TestGoFiles}}\t{{len .XTestGoFiles}}\t{{len .IgnoredGoFiles}}'
    p = subprocess.run(['go', 'list', '-e', '-f', fmt, './...'], cwd=root, capture_output=True, text=True, env=env)
    if p.returncode:
        return None
    out = set()
    for line in p.stdout.splitlines():
        d, gofiles, tests, xtests, _ = line.split('\t')
        if int(gofiles) or int(tests) or int(xtests):
            out.add(os.path.relpath(d, root))
    return out


def go_list_closure(root, dirs, env=None):
    """The same closure from `go list` (needs Go; build constraints of this host apply). None if go list fails."""
    fmt = '{{.ImportPath}}\t{{.Dir}}\t{{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}'
    p = subprocess.run(['go', 'list', '-e', '-f', fmt, './...'], cwd=root, capture_output=True, text=True, env=env)
    if p.returncode:
        return None
    rel, users = {}, {}
    for line in p.stdout.splitlines():
        imp, d, deps = line.split('\t')
        rel[imp] = os.path.relpath(d, root)
        for dep in deps.split():
            users.setdefault(dep, set()).add(imp)
    want = {imp for imp, d in rel.items() if d in dirs}
    todo = list(want)
    while todo:
        for u in users.get(todo.pop(), ()):
            if u not in want:
                want.add(u)
                todo.append(u)
    return sorted(rel[i] for i in want)


_FUNC = re.compile(r'^func (?:\([^)]*\) )?(\w+)\(', re.M)


def smb_tests(root, dirs):
    """[[dir, TestName], ...]: the tests of these package dirs that start the Samba fixture (internal/testsmb),
    directly or through a helper in their package's test files. They need smbd and smbpasswd, and a private UTS
    namespace whose hostname is localhost (the fixture's server name; with the build host's own name every SMB
    login times out)."""
    mod = module_path(root)
    want = re.compile(r'^\s*(?:import\s+)?(\w+\s+)?"' + re.escape(mod + '/internal/testsmb') + r'"', re.M)
    out = []
    for d in sorted(set(dirs)):
        try:
            names = sorted(f for f in os.listdir(os.path.join(root, d)) if f.endswith('_test.go'))
        except OSError:
            continue
        bodies, aliases = {}, set()
        for f in names:
            try:
                with open(os.path.join(root, d, f), encoding='utf-8', errors='replace') as fh:
                    text = fh.read()
            except OSError:
                continue
            for m in want.finditer(text):
                aliases.add((m.group(1) or 'testsmb').strip())
            starts = [(m.start(), m.group(1)) for m in _FUNC.finditer(text)]
            for i, (pos, name) in enumerate(starts):
                end = starts[i + 1][0] if i + 1 < len(starts) else len(text)
                bodies[name] = bodies.get(name, '') + text[pos:end]
        if not aliases:
            continue
        users = {n for n, b in bodies.items() if any(a + '.Start(' in b for a in aliases)}
        grew = True
        while grew:  # helpers that start the fixture, and whatever calls them
            grew = False
            for n, b in bodies.items():
                if n not in users and any(re.search(r'(?<![\w.])' + re.escape(u) + r'\(', b) for u in users):
                    users.add(n)
                    grew = True
        out += [[d, n] for n in sorted(users) if re.match(r'(Test|Fuzz)\w', n) and n != 'TestMain']
    return out

