#!/usr/bin/env python3
"""Build and run C tests the way the release checklist names them (tools/dev/c-tests.json), in parallel.

  crun.py [-j N] [--rerun] [--json OUT] [--list] SRC...

SRC is a C test source (tools/ui-mod/test_*.c, tools/edisksd-hook/test_edisksd_hook.c). Each one expands to its
variants (for example test_dsp_smooth.c: plain, ASan/UBSan and TSan; test_rt_callback.c: native and armhf, each
run plain and with `selftest`); every other tools/ui-mod test builds and runs the armhf way with the Engine
arguments it needs. A run marked `root:` (edisksd-hook's --mounts, inside `unshare -m --propagation private`) is
skipped with a reason when not root; a variant that needs the firmware lab fails when the lab is missing.
Each test's runs hold a host-wide lock of that test (/var/tmp/enginex-ctest-<name>.lock): two lanes never run the same
C test on one host at once (some use fixed paths such as /tmp/enginex-shadow-position-test).
--rerun: a variant that failed runs once more alone after the others; a pass is reported as FLAKY, not as passed.
Exit status: 0 when every variant built and every run passed (or passed on the rerun) or was skipped with a reason.
"""
import argparse, fcntl, json, os, pathlib, re, shutil, subprocess, sys, tempfile, time
from concurrent.futures import ThreadPoolExecutor

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
CFG = json.loads(pathlib.Path(HERE, 'c-tests.json').read_text())


def lab_dir():
    if os.environ.get('ENGINEX_LAB'):
        return os.environ['ENGINEX_LAB']
    try:  # the same default the sandbox scripts read
        text = pathlib.Path(ROOT, 'rootfs-overlay/usr/libexec/enginex-migration').read_text()
        m = re.search(r"^legacy_lab='([^']+)'", text, re.M)
        if m:
            return m.group(1)
    except OSError:
        pass
    return ''


def recipe(src):
    """(needs, {variant: {'build', 'runs'}}) for a source, or None when c-tests.json has no way to run it."""
    entry = CFG['tests'].get(src)
    if entry:
        return (entry.get('needs', []), entry['variants']) if 'variants' in entry else None
    if os.path.dirname(src) == CFG['default']['dir']:
        args = CFG['default_args'].get(src, '')
        variants = {k: {'build': v['build'], 'runs': [r.replace('{args}', args).rstrip() for r in v['runs']]}
                    for k, v in CFG['default']['variants'].items()}
        return CFG['default'].get('needs', []), variants
    return None


def jobs_for(srcs):
    out, bad = [], []
    for src in srcs:
        r = recipe(src)
        if r is None:
            bad.append(src)
            continue
        needs, variants = r
        for name, v in variants.items():
            out.append({'src': src, 'variant': name, 'needs': needs, 'build': v['build'], 'runs': v['runs']})
    return out, bad


def expand(cmd, job, out, lab):
    eng = {k: os.path.join(lab, v) for k, v in CFG['engines'].items()}
    return cmd.format(src=job['src'], out=out, lab=lab, cc_arm='arm-linux-gnueabihf-gcc', arm_flags=CFG['arm_flags'],
                      rt_wrap=CFG['rt_wrap'], **eng)


def run_job(job, work, lab, is_root):
    stem = os.path.basename(job['src'])[:-2]
    out = os.path.join(work, f"{stem}-{job['variant']}")
    res = {'name': f"{job['src']} {job['variant']}", 'ok': True, 'secs': 0.0, 'runs': [], 'tail': '', 'skipped': []}
    t0 = time.monotonic()
    if 'lab' in job['needs']:
        text = job['build'] + ' ' + ' '.join(job['runs'])
        wanted = [os.path.join(lab, v) for k, v in CFG['engines'].items() if '{' + k + '}' in text]
        missing = [p for p in [lab] + wanted if not lab or not os.path.exists(p)]
        if missing:
            res.update(ok=False, tail=f'firmware lab files missing: {missing[:3]} (set ENGINEX_LAB)')
            return res

    def sh(cmd):
        p = subprocess.run(['bash', '-c', cmd], cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
                           errors='replace', env=dict(os.environ, TMPDIR=work))
        return p.returncode, p.stdout

    rc, text = sh(expand(job['build'], job, out, lab))
    if rc:
        res.update(ok=False, secs=time.monotonic() - t0, tail='build failed:\n' + text[-4000:])
        return res
    lock = open(f'/var/tmp/enginex-ctest-{stem}.lock', 'w')
    fcntl.flock(lock, fcntl.LOCK_EX)  # another lane's run of the same test on this host finishes first
    try:
        _runs(job, res, out, lab, is_root, sh)
    finally:
        lock.close()
    res['secs'] = time.monotonic() - t0
    return res


def _runs(job, res, out, lab, is_root, sh):
    for run in job['runs']:
        needs_root = run.startswith('root: ')
        cmd = expand(run[len('root: '):] if needs_root else run, job, out, lab)
        if needs_root and not is_root:
            res['skipped'].append(f'{cmd} (needs root)')
            continue
        rc, text = sh(cmd)
        res['runs'].append({'cmd': cmd.replace(out, '<bin>'), 'rc': rc})
        if rc:
            res.update(ok=False, tail=f'{cmd.replace(out, "<bin>")} exited {rc}:\n' + text[-6000:])
            break


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('srcs', nargs='*'); ap.add_argument('-j', type=int, default=min(os.cpu_count() or 4, 8))
    ap.add_argument('--json'); ap.add_argument('--list', action='store_true')
    ap.add_argument('--rerun', action='store_true', help='run a failed variant once more alone; a pass is reported as flaky')
    a = ap.parse_args()
    jobs, bad = jobs_for(a.srcs)
    if a.list:
        for j in jobs:
            print(f"{j['src']} {j['variant']}: {j['build']} ; {' ; '.join(j['runs'])}")
        for b in bad:
            print(f'{b}: no recipe in tools/dev/c-tests.json')
        return 0 if not bad else 1
    lab = lab_dir()
    is_root = os.geteuid() == 0
    work = tempfile.mkdtemp(prefix='crun-', dir=os.environ.get('TMPDIR'))
    os.chmod(work, 0o755)
    t0 = time.monotonic()
    try:
        with ThreadPoolExecutor(max_workers=max(1, a.j)) as pool:
            results = list(pool.map(lambda j: run_job(j, work, lab, is_root), jobs))
        flaky = []
        if a.rerun and sum(1 for r in results if not r['ok']) <= 3:  # more means a broken tree or host, not load
            for i, (job, r) in enumerate(zip(jobs, results)):
                if not r['ok'] and not r['tail'].startswith(('build failed', 'firmware lab')):
                    again = run_job(job, work, lab, is_root)
                    if again['ok']:
                        flaky.append({'name': r['name'], 'first_failure': r['tail'][-1500:]})
                        results[i] = dict(again, flaky=True)
    finally:
        shutil.rmtree(work, ignore_errors=True)  # only the directory created above
    for b in bad:
        results.append({'name': b, 'ok': False, 'secs': 0.0, 'runs': [], 'skipped': [], 'tail': 'no recipe in tools/dev/c-tests.json'})
    failed = [r for r in results if not r['ok']]
    skipped = [s for r in results for s in r['skipped']]
    for r in failed:
        print(f"===== FAILED {r['name']} =====\n{r['tail']}", file=sys.stderr)
    wall = time.monotonic() - t0
    print(f'crun: {len(results)} variants of {len(set(j["src"] for j in jobs))} C tests, {sum(len(r["runs"]) for r in results)} runs, '
          f'{len(skipped)} skipped, {len(failed)} failed, {len(flaky)} flaky (passed on rerun), {wall:.1f}s -> {"OK" if not failed else "FAILED"}')
    for f in flaky:
        print(f"crun: flaky (failed once, passed alone): {f['name']}")
    for s in skipped:
        print(f'crun: skipped {s}')
    if a.json:
        pathlib.Path(a.json).write_text(json.dumps({'ok': not failed, 'wall': round(wall, 1), 'results': results, 'flaky': flaky,
                                                    'skipped': skipped, 'failed': [r['name'] for r in failed]}, indent=1))
    return 0 if not failed else 1


if __name__ == '__main__':
    sys.exit(main())
