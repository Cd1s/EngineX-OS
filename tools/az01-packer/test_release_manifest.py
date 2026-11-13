import hashlib
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
CHECK = REPO / "tools/az01-packer/check-release-manifest.sh"
VERSION = "5.3.0.5"
TAG = "v5.3.0.5-r2"
MODELS = [f"M{i}" for i in range(16)]
BUNDLES = [f"B{i}" for i in range(8)]
PLATFORMS = ["linux-amd64.tar.gz", "macos-amd64.zip", "macos-arm64.zip", "windows-amd64.zip"]


def release_files(version=VERSION):
    names = [f"{m}-{version}-EngineX-OS-Model-{m}-Update.img" for m in MODELS]
    names += [f"{b}-{version}-EngineX-OS-Data-Bundle.tar.gz" for b in BUNDLES]
    names += [f"EngineX-Installer-{version}-{p}" for p in PLATFORMS]
    return names


class ReleaseManifestCheck(unittest.TestCase):
    """#239 review: sign-release.sh matched names as substrings, so an entry for X-Update.img.bak
    satisfied the check for X-Update.img and an incomplete list was signed."""

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp(prefix="manifest-"))
        self.addCleanup(shutil.rmtree, self.dir, True)

    def build(self, tag=TAG, version=VERSION, extra=(), skip=()):
        (self.dir / "RELEASE-TAG.txt").write_text(tag + "\n")
        names = [n for n in release_files(version) if n not in skip]
        for n in names:
            (self.dir / n).write_bytes(n.encode())
        for n in extra:
            (self.dir / n).write_bytes(n.encode())
        self.write_sums(names + ["RELEASE-TAG.txt"])

    def write_sums(self, names):
        lines = []
        for n in names:
            digest = hashlib.sha256((self.dir / n).read_bytes()).hexdigest()
            lines.append(f"{digest}  {n}\n")
        (self.dir / "SHA256SUMS.txt").write_text("".join(lines))

    def check(self, **env):
        e = dict(os.environ)
        e.update({k: str(v) for k, v in env.items()})
        return subprocess.run(["sh", str(CHECK), str(self.dir)], capture_output=True, text=True, env=e)

    def assertRefused(self, why, **env):
        r = self.check(**env)
        self.assertNotEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn(why, r.stderr)

    def test_a_complete_release_passes(self):
        self.build()
        r = self.check()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("29 entries", r.stdout)  # 28 artifacts + RELEASE-TAG.txt

    def test_plain_tag_and_later_releases_without_bundles(self):
        self.build(tag="v5.3.0.6", version="5.3.0.6", skip=[n for n in release_files("5.3.0.6") if "Data-Bundle" in n])
        self.assertEqual(self.check(ENGINEX_EXPECT_BUNDLES=0).returncode, 0)
        self.assertRefused("expected 8 *-Data-Bundle.tar.gz")  # the default is the 5.3.0.4/5.3.0.5 set

    def test_a_backup_file_does_not_stand_in_for_the_image(self):
        # The review's reproduction: both files exist, the list names only the backup.
        self.build(extra=["SC5000-x-Update.img", "SC5000-x-Update.img.bak"])
        names = [n for n in release_files()] + ["SC5000-x-Update.img.bak", "RELEASE-TAG.txt"]
        self.write_sums(names)
        self.assertRefused("not a release artifact")

    def test_an_artifact_missing_from_the_list_is_refused_by_its_whole_name(self):
        self.build(extra=["SC5000-5.3.0.5-EngineX-OS-x-Update.img"])  # in the directory, not in the list
        self.assertRefused("SC5000-5.3.0.5-EngineX-OS-x-Update.img is in the directory but not in SHA256SUMS.txt")

    def test_a_longer_name_does_not_cover_a_shorter_one(self):
        self.build()
        victim = release_files()[0]
        names = [n for n in release_files() if n != victim] + [victim + "x", "RELEASE-TAG.txt"]
        (self.dir / (victim + "x")).write_bytes(b"x")
        self.write_sums(names)
        self.assertRefused("not a release artifact")

    def test_wrong_counts(self):
        self.build(skip=[release_files()[0]])
        self.assertRefused("expected 16 *-Update.img")
        self.setUp()
        self.build(skip=[release_files()[-1]])
        self.assertRefused("expected 4 EngineX-Installer-*")

    def test_files_of_another_version(self):
        self.build(tag="v5.3.0.6")
        self.assertRefused("is not a file of version 5.3.0.6")

    def test_a_listed_file_that_changed(self):
        self.build()
        (self.dir / release_files()[3]).write_bytes(b"changed after the list was written")
        self.assertRefused("does not match SHA256SUMS.txt")

    def test_a_name_listed_twice(self):
        self.build()
        sums = self.dir / "SHA256SUMS.txt"
        first = sums.read_text().splitlines()[0]
        sums.write_text(sums.read_text() + first + "\n")
        self.assertRefused("twice")

    def test_a_malformed_line(self):
        self.build()
        sums = self.dir / "SHA256SUMS.txt"
        sums.write_text(sums.read_text() + "not a checksum line\n")
        self.assertRefused("not '<sha256>  <name>'")

    def test_the_release_tag_file(self):
        self.build()
        (self.dir / "RELEASE-TAG.txt").unlink()
        self.assertRefused("RELEASE-TAG.txt is missing")
        self.setUp()
        self.build(tag="5.3.0.5")
        self.assertRefused("does not hold a release tag")

    def test_scripts_carry_the_tag_entry_through(self):
        # build-release.sh writes RELEASE-TAG.txt and lists it; build-installer.sh keeps listing it when it
        # rewrites the list; sign-release.sh runs this check and verifies the signature for the tag.
        rel = (REPO / "tools/az01-packer/build-release.sh").read_text()
        self.assertIn('printf \'%s\\n\' "$TAG" > RELEASE-TAG.txt', rel)
        self.assertIn("*-Data-Bundle.tar.gz RELEASE-TAG.txt > SHA256SUMS.txt", rel)
        self.assertIn("RELEASE-TAG.txt; do", (REPO / "tools/az01-packer/build-installer.sh").read_text())
        sign = (REPO / "tools/az01-packer/sign-release.sh").read_text()
        self.assertIn("check-release-manifest.sh", sign)
        self.assertIn('--verify --tag "$TAG"', sign)


if __name__ == "__main__":
    unittest.main()
