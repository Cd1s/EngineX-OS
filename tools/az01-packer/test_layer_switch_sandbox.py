"""The layer-switch tool rejects paths that leave its private work directory."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class LayerSwitchSandboxPathsTest(unittest.TestCase):
    def test_rejects_escaping_paths_before_creating_work_directory(self):
        repo = Path(__file__).resolve().parents[2]
        script = repo / "tools/az01-packer/layer-switch-sandbox.sh"
        with tempfile.TemporaryDirectory(prefix="layer-switch-paths-") as tmp:
            root = Path(tmp)
            outside = root / "outside"
            outside.write_text("keep")
            for index, settings in enumerate((
                    {"LSW_ARMS": "stock ../outside"},
                    {"LSW_ARMS": "stock /outside"},
                    {"LSW_DROP": "../outside"},
                    {"LSW_DROP": str(outside)},
            )):
                with self.subTest(settings=settings):
                    work = root / f"work-{index}"
                    env = dict(os.environ, ENGINEX_LAB=str(root / "missing-lab"),
                               ENGINEX_MIGRATION=str(repo / "rootfs-overlay/usr/libexec/enginex-migration"),
                               LSW_ARMS="stock pack", LSW_DROP="")
                    env.update(settings)
                    result = subprocess.run(["bash", str(script), str(work)], cwd=repo,
                                            env=env, text=True, capture_output=True, timeout=10)
                    self.assertEqual(result.returncode, 2, result.stderr)
                    self.assertIn("LSW_", result.stderr)
                    self.assertFalse(work.exists(), "invalid paths reached sandbox setup")
                    self.assertEqual(outside.read_text(), "keep")
