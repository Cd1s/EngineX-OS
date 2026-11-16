import io
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
import numpy as np
import worker
from worker import DemucsSeparator, Input, Separator, header_read, resolve_backend, serve
from render import run

WORKER_PY = str(Path(__file__).with_name('worker.py'))

class WorkerTests(unittest.TestCase):
    def test_reconstruction_and_frame_accounting(self):
        rng=np.random.default_rng(6596)
        for channels in (1,2):
            for length in (0,1,511,512,1024,1025,12347):
                x=(rng.normal(size=(channels,length))*.1).astype(np.float32)
                s=Separator(channels,44100)
                ys=[s.feed(x[:,i:i+237]) for i in range(0,length,237)]
                ys.append(s.drain())
                y=np.concatenate(ys,axis=2)
                self.assertEqual(y.shape,(4,channels,length))
                if length:
                    self.assertLess(np.max(abs(y.sum(axis=0)-x)),2e-6)
                self.assertEqual(s.drain().shape[-1],0)

    def test_batching_does_not_change_the_result(self):
        x=(np.random.default_rng(7).normal(size=(2,30000))*.1).astype(np.float32)
        outs=[]
        for step in (237,1024,30000):
            s=Separator(2,44100)
            outs.append(np.concatenate([s.feed(x[:,i:i+step]) for i in range(0,x.shape[1],step)]+[s.drain()],axis=2))
        for y in outs[1:]:
            np.testing.assert_allclose(y,outs[0],atol=1e-6)

    def test_pipelined_client_gets_one_reply_per_block(self):
        x=np.random.default_rng(2).normal(0,.05,(2,20000)).astype('f4')
        req=b'numChannels=2\nsampleRate=44100\nmaxBlockSize=1024\n\n'
        blocks=[x[:,i:i+1024] for i in range(0,x.shape[1],1024)]
        for b in blocks:
            req+=b'action=block\nnumSamples=%d\n\n'%b.shape[1]+np.ascontiguousarray(b).tobytes()
        out=io.BytesIO()
        serve(Input(io.BytesIO(req+b'action=done\n\n')),out)
        src=io.BytesIO(out.getvalue()); replies=[]; audio=[]
        while (f:=header_read(src)) is not None:
            if b'returnFrames' in f:
                n=int(f[b'returnFrames']); replies.append(n)
                audio.append(np.frombuffer(src.read(n*8*4),'<f4').reshape(4,2,n))
        self.assertGreater(len(replies),len(blocks))
        self.assertTrue(all(n<=1024 for n in replies) and replies[-1]==0)
        np.testing.assert_allclose(np.concatenate(audio,axis=2).sum(axis=0),x,atol=2e-6)

    def test_native_pipe_roundtrip(self):
        for channels in (1,2):
            x=np.random.default_rng(1).normal(0,.05,(channels,4381)).astype('f4')
            output=[]
            result=run([sys.executable,'-u',WORKER_PY],
                       (x[:,i:i+1024] for i in range(0,x.shape[1],1024)),
                       channels,48000,output.append)
            self.assertEqual(result['input_frames'],4381)
            np.testing.assert_allclose(np.concatenate(output,axis=2).sum(axis=0),x,atol=2e-6)

    def test_non_hop_aligned_native_block_limit(self):
        for limit in (1, 511, 513, 1000):
            x=np.random.default_rng(4).normal(0,.05,(2,2107)).astype('f4'); output=[]
            run([sys.executable,'-u',WORKER_PY],
                (x[:,i:i+limit] for i in range(0,x.shape[1],limit)),
                consume=output.append,max_block=limit)
            np.testing.assert_allclose(np.concatenate(output,axis=2).sum(axis=0),x,atol=2e-6)

    def test_reject_unbounded_input(self):
        for header in (b'numChannels=200\nsampleRate=44100\nmaxBlockSize=1024\n\n',
                       b'numChannels=2\nsampleRate=44100\nmaxBlockSize=1000000000\n\n'):
            with self.assertRaises(ValueError):serve(io.BytesIO(header),io.BytesIO())
        with self.assertRaises(ValueError):header_read(io.BytesIO(b'x=' + b'1'*8192+b'\n\n'))
        with self.assertRaises(ValueError):header_read(io.BytesIO(b'x=1\nx=2\n\n'))
        # #38: an incomplete init header is a clear error, not KeyError.
        with self.assertRaises(ValueError):serve(io.BytesIO(b'numChannels=2\n\n'),io.BytesIO())

    def test_blank_lines_before_init_are_skipped(self):
        x=np.random.default_rng(3).normal(0,.05,(2,1024)).astype('f4')
        req=(b'\n\r\nnumChannels=2\nsampleRate=44100\nmaxBlockSize=1024\n\n'
             b'action=block\nnumSamples=1024\n\n'+x.tobytes(order='C')+b'action=done\n\n')
        out=io.BytesIO()
        serve(Input(io.BytesIO(req)),out)
        self.assertTrue(out.getvalue().startswith(b'session='))

    def test_demucs_chunks_stay_aligned(self):
        # A fake model with a fixed split: crossfading equal estimates must
        # leave it exact, so any misaligned frame shows up.
        def fake(x,rate):
            return np.stack((x*.5,x*.25,np.zeros_like(x),x*.25))
        rng=np.random.default_rng(3)
        for channels in (1,2):
            for length in (0,1,399,400,401,3001):
                x=rng.normal(0,.1,(channels,length)).astype('f4')
                s=DemucsSeparator(channels,8000,separate=fake,chunk=.05,overlap=.01)
                y=np.concatenate([s.feed(x[:,i:i+237]) for i in range(0,length,237)]+[s.drain()],axis=2)
                self.assertEqual(y.shape,(4,channels,length))
                np.testing.assert_allclose(y[0],x*.5,atol=1e-6)
                np.testing.assert_allclose(y.sum(axis=0),x,atol=1e-6)

    def test_backend_names(self):
        self.assertEqual(resolve_backend('hpss'),'hpss')
        self.assertIn(resolve_backend('auto'),('hpss','htdemucs'))

    def test_windows_pipe_select_oserror_does_not_crash(self):
        # On Windows (run-home-worker.ps1), stdin is a pipe (fd 0) and Winsock
        # select.select([0], [], [], 0) raises OSError [WinError 10038] (BH-GF2-6).
        x = np.random.default_rng(11).normal(0, .05, (2, 2048)).astype('f4')
        chunks = [
            b'numChannels=2\nsampleRate=44100\nmaxBlockSize=1024\n\n'
            b'action=block\nnumSamples=1024\n\n' + np.ascontiguousarray(x[:, :1024]).tobytes(),
            b'action=block\nnumSamples=1024\n\n' + np.ascontiguousarray(x[:, 1024:]).tobytes(),
            b'action=done\n\n',
            b'',
        ]
        idx = 0

        class FakePipe:
            def fileno(self):
                return 0

        def fake_read(fd, n):
            nonlocal idx
            self.assertEqual(fd, 0)
            out = chunks[idx]
            if idx < len(chunks) - 1:
                idx += 1
            return out

        def win_select(*_args, **_kwargs):
            raise OSError(10038, 'An operation was attempted on something that is not a socket')

        out = io.BytesIO()
        with mock.patch.object(worker.os, 'read', side_effect=fake_read), \
             mock.patch.object(worker.select, 'select', side_effect=win_select):
            serve(Input(FakePipe()), out)

        src = io.BytesIO(out.getvalue())
        audio = []
        while (f := header_read(src)) is not None:
            if b'returnFrames' in f:
                n = int(f[b'returnFrames'])
                audio.append(np.frombuffer(src.read(n * 8 * 4), '<f4').reshape(4, 2, n))
        np.testing.assert_allclose(np.concatenate(audio, axis=2).sum(axis=0), x, atol=2e-6)

        # Also verify the Windows PeekNamedPipe fast path avoids Winsock select.
        fake_msvcrt = type('msvcrt', (), {'get_osfhandle': staticmethod(lambda fd: 1234)})

        def fake_peek(_h, _b, _sz, _rd, avail_ptr, _left):
            avail_ptr._obj.value = 512
            return 1

        fake_windll = type('windll', (), {'kernel32': type('k32', (), {'PeekNamedPipe': staticmethod(fake_peek)})})
        with mock.patch.dict(sys.modules, {'msvcrt': fake_msvcrt}), \
             mock.patch.object(worker.os, 'name', 'nt'), \
             mock.patch.object(worker.ctypes, 'windll', fake_windll, create=True), \
             mock.patch.object(worker.select, 'select', side_effect=win_select):
            self.assertTrue(Input(FakePipe()).waiting())

    def test_run_home_worker_tunnel_requires_token_file(self):
        # --with-tunnel without enginex-relay must never launch enginex-stemsd with
        # an empty -token-file behind a loopback Quick Tunnel proxy (BH-GF2-14).
        script = Path(__file__).with_name('run-home-worker.sh')
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            home = root / 'home'
            venv_bin = home / 'venv' / 'bin'
            venv_bin.mkdir(parents=True)
            os.symlink(sys.executable, venv_bin / 'python3')

            bin_dir = root / 'bin'
            bin_dir.mkdir()
            args_file = root / 'stemsd_args.txt'
            stemsd = bin_dir / 'enginex-stemsd'
            stemsd.write_text(f'#!/bin/sh\nprintf "%s\\n" "$@" > "{args_file}"\n')
            stemsd.chmod(0o755)
            cf = bin_dir / 'cloudflared'
            cf.write_text('#!/bin/sh\nsleep 1\n')
            cf.chmod(0o755)

            env = dict(os.environ)
            env['ENGINEX_STEMS_HOME'] = str(home)
            env['ENGINEX_STEMSD'] = str(stemsd)
            env['ENGINEX_RELAY'] = ''
            env['ENGINEX_RELAY'] = ''
            env['PATH'] = f"{bin_dir}:/usr/bin:/bin"

            res = subprocess.run(['/bin/sh', str(script), '--with-tunnel', 'hpss'],
                                 env=env, capture_output=True, text=True, timeout=10)
            self.assertEqual(res.returncode, 0, res.stderr)
            argv = args_file.read_text().splitlines()
            idx = argv.index('-token-file')
            tok_path = argv[idx + 1]
            self.assertNotEqual(tok_path, '', 'run-home-worker.sh --with-tunnel passed empty -token-file')
            self.assertGreaterEqual(len(Path(tok_path).read_text().strip()), 24)

    def test_windows_runner_quotes_empty_token_file_and_scopes_firewall(self):
        ps1 = (Path(__file__).resolve().parent / 'run-home-worker.ps1').read_text()
        self.assertIn("'-token-file='", ps1,
                      'PowerShell 5.1 splits unquoted -token-file= and drops the empty value')
        self.assertNotRegex(ps1, r'(?<![\'"])-token-file=(?![\'"])')
        self.assertIn('[switch]$AllowPublicFirewall', ps1,
                      'Opening Public firewall profile must require an explicit switch')
        self.assertRegex(
            ps1,
            r'if\s*\(\$AllowPublicFirewall\)\s*\{[^}]*-Profile\s+Domain,Private,Public\s+-RemoteAddress\s+LocalSubnet[^}]*\}\s*else\s*\{[^}]*-Profile\s+Domain,Private\s+-RemoteAddress\s+LocalSubnet',
            'Default firewall rule must be -Profile Domain,Private -RemoteAddress LocalSubnet',
        )

    def test_macos_command_wrapper_runs_home_worker_from_any_cwd(self):
        cmd_file = Path(__file__).resolve().with_name('run-home-worker.command')
        self.assertTrue(cmd_file.exists(), 'tools/stems/run-home-worker.command missing')
        self.assertTrue(os.access(cmd_file, os.X_OK), 'run-home-worker.command must be executable')
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            stems_dir = root / 'stems'
            stems_dir.mkdir()
            (stems_dir / 'run-home-worker.command').write_text(cmd_file.read_text(), encoding='utf-8')
            (stems_dir / 'run-home-worker.command').chmod(0o755)
            args_file = root / 'args.txt'
            (stems_dir / 'run-home-worker.sh').write_text(
                f'#!/bin/sh\npwd > "{root / "cwd.txt"}"\nprintf "%s\\n" "$@" > "{args_file}"\nexit "${{TEST_EXIT:-0}}"\n',
                encoding='utf-8',
            )

            res_ok = subprocess.run(
                ['/bin/sh', str(stems_dir / 'run-home-worker.command'), 'hpss'],
                cwd=str(root),
                input='\n',
                capture_output=True,
                text=True,
                timeout=10,
            )
            self.assertEqual(res_ok.returncode, 0, res_ok.stderr)
            self.assertEqual(Path(root / 'cwd.txt').read_text().strip(), str(stems_dir.resolve()))
            self.assertEqual(args_file.read_text().splitlines(), ['hpss'])

            env = dict(os.environ, TEST_EXIT='2')
            res_fail = subprocess.run(
                ['/bin/sh', str(stems_dir / 'run-home-worker.command')],
                cwd=str(root),
                env=env,
                input='\n',
                capture_output=True,
                text=True,
                timeout=10,
            )
            self.assertEqual(res_fail.returncode, 2)
            self.assertIn('Press Enter to close...', res_fail.stdout)


class TorchCudaTests(unittest.TestCase):
    """A tester's RTX 5090 Laptop (Blackwell, compute capability 12.0) got the
    CUDA 12.6 wheels, which cannot run on it, and the worker rendered on the CPU
    without a word."""

    def test_wheel_index_follows_the_gpu(self):
        for caps in ('12.0', '10.0', '8.6 12.0', '8.6, 12.0'):
            self.assertEqual(worker.torch_cuda_indexes(caps), ['cu128'], caps)
        # Older cards, and a driver too old to report the capability, keep 12.6 then 12.4.
        for caps in ('9.0', '8.9', '8.6', '7.5', '6.1', '', None,
                     'Field "compute_cap" is not a valid field to query.'):
            self.assertEqual(worker.torch_cuda_indexes(caps), ['cu126', 'cu124'], caps)
        for arg, out in (('--torch-index=12.0', 'cu128'), ('--torch-index=8.6', 'cu126 cu124'),
                         ('--torch-index=', 'cu126 cu124')):
            res = subprocess.run([sys.executable, WORKER_PY, arg], capture_output=True, text=True)
            self.assertEqual((res.returncode, res.stdout.strip()), (0, out), arg)

    def test_cuda_check_says_why_the_gpu_is_not_used(self):
        def fake(cuda='12.6', available=True, fails=None):
            ones = mock.Mock(side_effect=fails) if fails else mock.Mock(return_value=mock.MagicMock())
            return mock.Mock(__version__='2.7.1', version=mock.Mock(cuda=cuda), ones=ones,
                             cuda=mock.Mock(is_available=lambda: available,
                                            get_device_capability=lambda i: (12, 0)))
        cases = (
            (fake(cuda=None), 'PyTorch 2.7.1 is a CPU-only build'),
            (fake(available=False), 'PyTorch 2.7.1 (CUDA 12.6) finds no usable CUDA device'),
            (fake(fails=RuntimeError('CUDA error: no kernel image is available for execution on the device\nmore')),
             'PyTorch 2.7.1 (CUDA 12.6) cannot run on this GPU (sm_120): CUDA error: no kernel image is available for execution on the device'),
            (fake(cuda='12.8'), 'ok'),
        )
        # Review 9 of 555722d3: a broken CUDA context also fails the capability query;
        # the reason still comes, without the capability.
        broken = fake(fails=RuntimeError('CUDA initialization error'))
        broken.cuda.get_device_capability = mock.Mock(side_effect=RuntimeError('CUDA initialization error'))
        cases += ((broken, 'PyTorch 2.7.1 (CUDA 12.6) cannot run on this GPU: CUDA initialization error'),)
        for torch, want in cases:
            with mock.patch.dict(sys.modules, {'torch': torch}):
                self.assertEqual(worker.cuda_check(), want)
        with mock.patch.dict(sys.modules, {'torch': None}):
            self.assertEqual(worker.cuda_check(), 'PyTorch is not installed')
        # The CLI, with a torch that fails everywhere: one line, exit 0, so the
        # launchers (sh under set -e) go on to print their warning.
        fakes = {
            'broken': "ones = None\nclass cuda:\n    is_available = staticmethod(lambda: True)\n"
                      "    def get_device_capability(i): raise RuntimeError('CUDA initialization error')\n"
                      "def ones(*a, **k): raise RuntimeError('CUDA initialization error')\n",
            'raising': "class cuda:\n    def is_available(): raise RuntimeError('driver gone')\n",
        }
        wants = {'broken': 'PyTorch 2.7.1 (CUDA 12.6) cannot run on this GPU: CUDA initialization error',
                 'raising': 'the CUDA check failed: driver gone'}
        for name, body in fakes.items():
            with tempfile.TemporaryDirectory() as td:
                Path(td, 'torch.py').write_text("__version__ = '2.7.1'\nclass version:\n    cuda = '12.6'\n" + body)
                env = dict(os.environ, PYTHONPATH=td)
                res = subprocess.run([sys.executable, WORKER_PY, '--cuda-check'], capture_output=True, text=True, env=env)
                self.assertEqual((res.returncode, res.stdout.splitlines()), (0, [wants[name]]), res.stderr)

    def test_home_worker_scripts_pick_the_wheel_and_say_when_the_gpu_is_unused(self):
        here = Path(__file__).resolve().parent
        ps1 = (here / 'run-home-worker.ps1').read_text()
        # The index comes from the GPU, one token so an empty value survives Windows PowerShell.
        self.assertIn('--query-gpu=compute_cap', ps1)
        self.assertIn('"--torch-index=$caps"', ps1)
        self.assertNotRegex(ps1, r'whl/cu12\d')
        self.assertIn('--index-url "https://download.pytorch.org/whl/$index"', ps1)
        sh = (here / 'run-home-worker.sh').read_text()
        for script in (ps1, sh):
            self.assertIn('--cuda-check', script)
            self.assertIn('WARNING', script)
            self.assertLess(script.index('get_model'), script.index('--cuda-check'))

if __name__=='__main__':unittest.main()
