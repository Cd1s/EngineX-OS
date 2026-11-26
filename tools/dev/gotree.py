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
