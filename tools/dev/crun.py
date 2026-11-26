#!/usr/bin/env python3
"""Build and run C tests the way the release checklist names them (tools/dev/c-tests.json), in parallel.

  crun.py [-j N] [--rerun] [--json OUT] [--list] SRC...

SRC is a C test source (tools/ui-mod/test_*.c, tools/edisksd-hook/test_edisksd_hook.c). Each one expands to its
variants (for example test_dsp_smooth.c: plain, ASan/UBSan and TSan; test_rt_callback.c: native and armhf, each
run plain and with `selftest`); every other tools/ui-mod test builds and runs the armhf way with the Engine
arguments it needs. A run marked `root:` (edisksd-hook's --mounts, inside `unshare -m --propagation private`) needs
root; without it the variant is reported as skipped with the reason. A variant that needs the firmware lab fails
when the lab is missing.
Runs at the same time never share anything: as root every run gets private mount and network namespaces, with
empty /tmp, /run and /dev/shm and a loopback-only network (C tests write fixed paths such as /tmp/test-oa-replay
and the hook's own paths); without root, or without unshare and ip, the variants run one at a time. Each test also
holds a host-wide lock while it runs, so two lanes never run the same test on one host at once.
--rerun: a failed variant runs once more alone after the others; a pass is reported as flaky, not as passed.
--json OUT: tools/dev/results.py's format. Exit 0 only when every variant built and every run passed.
"""
import argparse, fcntl, json, os, pathlib, re, shutil, subprocess, sys, tempfile, time
from concurrent.futures import ThreadPoolExecutor

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import results  # noqa: E402

ROOT = os.environ.get('ENGINEX_TREE') or os.getcwd()  # the tree under test (test.sh runs this from its root)
CFG = json.loads(pathlib.Path(HERE, 'c-tests.json').read_text())
ISOLATE = ('mount -t tmpfs -o mode=1777 tmpfs /tmp && mount -t tmpfs -o mode=755 tmpfs /run && '
           'mount -t tmpfs -o mode=1777 tmpfs /dev/shm && ip link set lo up && cd "$1" && shift && exec bash -c "$@"')


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


def can_isolate():
    return os.geteuid() == 0 and bool(shutil.which('unshare')) and bool(shutil.which('ip'))


def run_job(job, work, lab, isolate):
    stem = os.path.basename(job['src'])[:-2]
    wd = tempfile.mkdtemp(prefix=f'{stem}-{job["variant"]}-', dir=work)  # this variant's own binary and TMPDIR
    os.chmod(wd, 0o1777)
    out = os.path.join(wd, 'bin')
    res = {'name': f"{job['src']} {job['variant']}", 'ok': True, 'secs': 0.0, 'runs': [], 'tail': '', 'skipped': []}
    t0 = time.monotonic()
    if 'lab' in job['needs']:
        text = job['build'] + ' ' + ' '.join(job['runs'])
        wanted = [os.path.join(lab, v) for k, v in CFG['engines'].items() if '{' + k + '}' in text]
        missing = [p for p in [lab] + wanted if not lab or not os.path.exists(p)]
        if missing:
            res.update(ok=False, tail=f'firmware lab files missing: {missing[:3]} (set ENGINEX_LAB)')
            shutil.rmtree(wd, ignore_errors=True)
            return res

    def sh(cmd, run):
        if run and isolate:  # private /tmp, /run, /dev/shm and network for this run only
            argv = ['unshare', '-m', '-n', '--propagation', 'private', 'bash', '-c', ISOLATE, 'bash', ROOT, cmd]
            env = dict(os.environ, TMPDIR='/tmp')
        else:
            argv, env = ['bash', '-c', cmd], dict(os.environ, TMPDIR=wd)
        p = subprocess.run(argv, cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, errors='replace', env=env)
        return p.returncode, p.stdout

    try:
        rc, text = sh(expand(job['build'], job, out, lab), False)
        if rc:
            res.update(ok=False, secs=time.monotonic() - t0, tail='build failed:\n' + text[-4000:])
            return res
        lock = open(f'/var/lock/enginex-ctest-{stem}.lock', 'w')
        fcntl.flock(lock, fcntl.LOCK_EX)  # another lane's run of the same test on this host finishes first
        try:
            for run in job['runs']:
                needs_root = run.startswith('root: ')
                cmd = expand(run[len('root: '):] if needs_root else run, job, out, lab)
                if needs_root and os.geteuid() != 0:
                    res['skipped'].append(f'{cmd.replace(out, "<bin>")} (needs root)')
                    continue
                rc, text = sh(cmd, True)
                res['runs'].append({'cmd': cmd.replace(out, '<bin>'), 'rc': rc})
                if rc:
                    res.update(ok=False, tail=f'{cmd.replace(out, "<bin>")} exited {rc}:\n' + text[-6000:])
                    break
        finally:
            lock.close()
    finally:
        shutil.rmtree(wd, ignore_errors=True)
    res['secs'] = time.monotonic() - t0
    return res


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
    res = results.Results()
    lab = lab_dir()
    isolate = can_isolate()
    workers = a.j if isolate else 1
    if not isolate:
        res.notes.append('no private namespaces here (needs root, unshare and ip): the variants ran one at a time')
    work = tempfile.mkdtemp(prefix='c', dir=os.environ.get('TMPDIR'))
    os.chmod(work, 0o755)
    t0 = time.monotonic()
    flaky = []
    try:
        with ThreadPoolExecutor(max_workers=max(1, workers)) as pool:
            done = list(pool.map(lambda j: run_job(j, work, lab, isolate), jobs))
        if a.rerun and sum(1 for r in done if not r['ok']) <= 3:  # more means a broken tree or host, not load
            for i, (job, r) in enumerate(zip(jobs, done)):
                if not r['ok'] and not r['tail'].startswith(('build failed', 'firmware lab')):
                    again = run_job(job, work, lab, isolate)
                    if again['ok']:
                        flaky.append((job, r['tail']))
                        done[i] = again
    finally:
        shutil.rmtree(work, ignore_errors=True)  # only the directory created above
    for job, r in zip(jobs, done):
        status = 'fail' if not r['ok'] else 'skip' if r['skipped'] else 'pass'
        reason = r['tail'] if status == 'fail' else '; '.join(r['skipped'])
        res.add('c', job['src'], job['variant'], status, r['secs'], reason)
    for job, first in flaky:
        res.flaky_one('c', job['src'], job['variant'], first[-1500:])
    for b in bad:
        res.error(f'{b}: no recipe in tools/dev/c-tests.json')
    res.wall = time.monotonic() - t0
    res.extra['runs'] = {f"{j['src']} {j['variant']}": r['runs'] for j, r in zip(jobs, done)}
    if a.json:
        res.write(a.json)
    print(res.summary(f'crun ({len(jobs)} variants of {len(set(j["src"] for j in jobs))} C tests, {"isolated" if isolate else "one at a time"})'))
    for r in res.items:
        if r['status'] != 'pass':
            print(f"crun: {r['status']} {r['where']} {r['test']}: {r['reason'][-3000:]}", file=sys.stderr)
    for e in res.errors:
        print('crun: ERROR ' + e, file=sys.stderr)
    return 0 if res.ok() and not any(r['status'] == 'skip' for r in res.items) else 1


if __name__ == '__main__':
    sys.exit(main())
