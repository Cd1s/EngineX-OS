import unittest

import numpy as np

from score import bss_metrics


class ScoreTests(unittest.TestCase):
    def test_tiny_fixture_scores_perfect_reconstruction(self):
        t = np.arange(32, dtype=np.float64)
        references = np.stack([
            np.stack((np.sin(t), np.cos(t))),
            np.stack((t / 32, -t / 32)),
            np.stack((np.where(t % 4 == 0, 1.0, 0.0), np.zeros_like(t))),
            np.stack((np.zeros_like(t), np.where(t % 3 == 0, .5, 0.0))),
        ])
        values = bss_metrics(references, references.copy(), filter_length=1)
        self.assertTrue(np.all(values[:, 0] > 50))
        self.assertTrue(np.all(values[:, 1] > 50))
        self.assertTrue(np.all(values[:, 2] > 50))


if __name__ == "__main__":
    unittest.main()
