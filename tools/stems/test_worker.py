import io
import sys
import unittest
import numpy as np
from worker import Input, Separator, header_read, serve
from render import run

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
            result=run([sys.executable,'-u','worker.py'],
                       (x[:,i:i+1024] for i in range(0,x.shape[1],1024)),
                       channels,48000,output.append)
            self.assertEqual(result['input_frames'],4381)
            np.testing.assert_allclose(np.concatenate(output,axis=2).sum(axis=0),x,atol=2e-6)

    def test_non_hop_aligned_native_block_limit(self):
        for limit in (1, 511, 513, 1000):
            x=np.random.default_rng(4).normal(0,.05,(2,2107)).astype('f4'); output=[]
            run([sys.executable,'-u','worker.py'],
                (x[:,i:i+limit] for i in range(0,x.shape[1],limit)),
                consume=output.append,max_block=limit)
            np.testing.assert_allclose(np.concatenate(output,axis=2).sum(axis=0),x,atol=2e-6)

    def test_reject_unbounded_input(self):
        for header in (b'numChannels=200\nsampleRate=44100\nmaxBlockSize=1024\n\n',
                       b'numChannels=2\nsampleRate=44100\nmaxBlockSize=1000000000\n\n'):
            with self.assertRaises(ValueError):serve(io.BytesIO(header),io.BytesIO())
        with self.assertRaises(ValueError):header_read(io.BytesIO(b'x=' + b'1'*8192+b'\n\n'))
        with self.assertRaises(ValueError):header_read(io.BytesIO(b'x=1\nx=2\n\n'))

if __name__=='__main__':unittest.main()
