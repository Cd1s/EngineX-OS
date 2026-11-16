#!/usr/bin/env python3
"""#234: current repository text and paths use the EngineX project name."""
import fnmatch
import json
import re
import subprocess
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
ALLOW = json.loads((Path(__file__).with_name('rename234_allowlist.json')).read_text())
OLD = 'sc' + '5000'
MODEL = re.compile(r'\bSC5000M?\b')  # Denon hardware and official image prefixes.


def findings(repo, paths=None):
    if paths is None:
        paths = subprocess.check_output(
            ['git', '-C', str(repo), 'ls-files', '-co', '--exclude-standard', '-z']).decode().split('\0')
    out = []
    allowed = [pattern for group in ALLOW.values() for pattern in group]
    for rel in sorted(set(paths) - {''}):
        if any(fnmatch.fnmatch(rel, p) for p in allowed):
            continue
        if OLD in MODEL.sub('', rel).lower():
            out.append(f'{rel}: old project name in path')
        path = repo / rel
        if not path.exists() and not path.is_symlink():
            continue
        try:
            text = path.read_text() if not path.is_symlink() else str(path.readlink())
        except (UnicodeError, OSError):
            continue
        for n, line in enumerate(text.splitlines(), 1):
            if OLD in MODEL.sub('', line).lower():
                out.append(f'{rel}:{n}: {line.strip()[:160]}')
    return out


class Rename234NamesTest(unittest.TestCase):
    def test_repository_uses_current_names(self):
        self.assertEqual(findings(REPO), [], 'Old project names outside the explicit allow-list')

    def test_guard_checks_docs_tests_paths_and_only_exact_hardware_names(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            files = {'guide.md': 'Denon SC5000 and SC5000M\n' + OLD + '.lan\n',
                     'pkg/x_test.go': '// ' + OLD + '-suite\n',
                     OLD + '-tool': 'EngineX\n',
                     'internal/migration/legacy.go': '// ' + OLD + '\n'}
            for rel, text in files.items():
                p = root / rel
                p.parent.mkdir(parents=True, exist_ok=True)
                p.write_text(text)
            self.assertEqual([f.split(':')[0] for f in findings(root, files)],
                             ['guide.md', 'pkg/x_test.go', OLD + '-tool'])


if __name__ == '__main__':
    unittest.main()
