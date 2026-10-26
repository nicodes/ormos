"""Installer integration checks with published-archive fixtures, no network."""
import hashlib
import io
import json
import os
import re
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class InstallerTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ormos-installer-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.home = self.root / "home"
        self.home.mkdir()
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.dest = self.home / "app bin"
        self.assets = self.root / "assets"
        self.assets.mkdir()
        self.calls = self.root / "calls.jsonl"
        self.env = {**os.environ, "HOME": str(self.home), "SHELL": "/bin/bash",
                    "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
                    "TEST_ASSETS": str(self.assets), "TEST_CALLS": str(self.calls),
                    "TEST_OS": "Linux", "TEST_ARCH": "x86_64"}
        self.env.pop("ORMOS_INSTALL_DIR", None)
        self.make_tool("uname", '#!/bin/sh\ncase "$1" in -s) printf "%s\\n" "$TEST_OS";; -m) printf "%s\\n" "$TEST_ARCH";; esac\n')
        self.make_tool("sysctl", '#!/bin/sh\nprintf "%s\\n" "${TEST_ROSETTA:-0}"\n')
        self.make_tool("curl", f"#!{sys.executable}\n" + '''import json, os, pathlib, shutil, sys
args=sys.argv[1:]
with open(os.environ['TEST_CALLS'],'a') as log: log.write(json.dumps(args)+'\\n')
url=next(arg for arg in args if arg.startswith('https://'))
if os.environ.get('TEST_DOWNLOAD_FAIL'): sys.exit(22)
if url.endswith('/latest'):
 print(os.environ.get('TEST_LATEST_URL','https://github.com/nicodes/ormos/releases/tag/v0.2.0'),end='')
else:
 if '/download/'+os.environ.get('TEST_RELEASE_TAG','v0.2.0')+'/' not in url: sys.exit(22)
 output=args[args.index('--output')+1]
 shutil.copyfile(pathlib.Path(os.environ['TEST_ASSETS'])/url.rsplit('/',1)[-1],output)
''')
        self.archive()

    def make_tool(self, name, content):
        path = self.bin / name
        path.write_text(content)
        path.chmod(0o755)

    def archive(self, platform="Linux", arch="x86_64", reported="0.2.0", symlink=False):
        asset = f"ormos_{platform}_{arch}.tar.gz"
        path = self.assets / asset
        with tarfile.open(path, "w:gz") as archive:
            binary = f'#!/bin/sh\n[ "$1" = --version ] || exit 2\nprintf "%s\\n" "{reported}"\n'.encode()
            info = tarfile.TarInfo("ormos")
            info.mode = 0o755
            if symlink:
                info.type = tarfile.SYMTYPE
                info.linkname = "/etc/passwd"
                archive.addfile(info)
            else:
                info.size = len(binary)
                archive.addfile(info, io.BytesIO(binary))
        (self.assets / "checksums.txt").write_text(f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {asset}\n")

    def run_install(self, *args, default=False):
        args = args if default else ("--install-dir", str(self.dest), *args)
        return subprocess.run(["sh", str(ROOT / "install.sh"), *args], env=self.env,
                              capture_output=True, text=True, timeout=10)

    def assert_failure_preserves_install(self, *args):
        self.dest.mkdir(exist_ok=True)
        old = self.dest / "ormos"
        old.write_text("keep previous installation")
        result = self.run_install(*args)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(old.read_text(), "keep previous installation")
        self.assertFalse(list(self.dest.glob(".ormos-install.*")))
        return result

    def test_latest_install_update_and_custom_path(self):
        self.dest.mkdir()
        (self.dest / "ormos").write_text("old binary")
        for _ in range(2):
            result = self.run_install()
            self.assertEqual(result.returncode, 0, result.stderr)
        executable = self.dest / "ormos"
        self.assertEqual(subprocess.check_output([executable, "--version"], text=True).strip(), "0.2.0")
        self.assertFalse(list(self.dest.glob(".ormos-install.*")))
        self.assertFalse((self.home / ".bashrc").exists())
        calls = [json.loads(row) for row in self.calls.read_text().splitlines()]
        self.assertTrue(all('--proto' in call and '=https' in call for call in calls))
        self.assertTrue(all('api.github.com' not in arg for call in calls for arg in call))

    def test_all_release_platforms_and_explicit_versions(self):
        for platform in ["Linux", "Darwin"]:
            for arch in ["x86_64", "arm64"]:
                with self.subTest(platform=platform, arch=arch):
                    self.env.update(TEST_OS=platform, TEST_ARCH=arch)
                    self.archive(platform, arch)
                    result = self.run_install("--version", "0.2.0")
                    self.assertEqual(result.returncode, 0, result.stderr)
        self.env.update(TEST_OS="Linux", TEST_ARCH="aarch64")
        self.archive("Linux", "arm64")
        self.assertEqual(self.run_install("--version", "v0.2.0").returncode, 0)
        calls = [json.loads(row) for row in self.calls.read_text().splitlines()]
        self.assertFalse(any(arg.endswith('/latest') for call in calls for arg in call))

    def test_version_alias_and_explicit_latest(self):
        for flag in ["--version", "-v"]:
            for version in ["0.2.0", "v0.2.0", "latest"]:
                with self.subTest(flag=flag, version=version):
                    result = self.run_install(flag, version)
                    self.assertEqual(result.returncode, 0, result.stderr)
        for flag in ["--version", "-v"]:
            result = self.run_install(flag)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("needs a value", result.stderr)

    def test_rosetta_uses_native_arm64(self):
        self.env.update(TEST_OS="Darwin", TEST_ARCH="x86_64", TEST_ROSETTA="1")
        self.archive("Darwin", "arm64")
        result = self.run_install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Darwin arm64', result.stdout)

    def test_checksum_tampering_missing_and_duplicate_entries(self):
        for mode in ["tampered", "missing", "duplicate"]:
            with self.subTest(mode=mode):
                self.archive()
                checksums = self.assets / "checksums.txt"
                if mode == "tampered":
                    with (self.assets / "ormos_Linux_x86_64.tar.gz").open("ab") as archive:
                        archive.write(b"tampered")
                elif mode == "missing":
                    checksums.write_text("not a digest  another.tar.gz\n")
                else:
                    checksums.write_text(checksums.read_text()*2)
                self.assert_failure_preserves_install()

    def test_wrong_binary_version_and_symlink_are_rejected(self):
        for version in ["0.1.15", "dev", "v0.2.0-0.20261006190000-7c79e160410c"]:
            self.archive(reported=version)
            self.assert_failure_preserves_install()
        self.archive(symlink=True)
        self.assert_failure_preserves_install()

    def test_download_failure_and_unsupported_targets(self):
        self.env["TEST_DOWNLOAD_FAIL"] = "1"
        self.assert_failure_preserves_install()
        self.env.pop("TEST_DOWNLOAD_FAIL")
        for key, value in [("TEST_OS", "Windows_NT"), ("TEST_ARCH", "riscv64")]:
            self.env[key] = value
            self.assert_failure_preserves_install()
            self.env.update(TEST_OS="Linux", TEST_ARCH="x86_64")

    def test_version_and_redirect_validation(self):
        self.assert_failure_preserves_install("--version", "../../evil")
        self.env["TEST_LATEST_URL"] = "https://example.com/v0.2.0"
        self.assert_failure_preserves_install()
        self.env["TEST_LATEST_URL"] = "https://github.com/nicodes/ormos/releases/tag/not-semver"
        self.assert_failure_preserves_install()

    def test_profiles_are_preserved_and_not_duplicated(self):
        for shell, names in [("/bin/bash", [".bashrc", ".profile"]), ("/bin/zsh", [".zshrc", ".zprofile"])]:
            with self.subTest(shell=shell):
                self.env["SHELL"] = shell
                for name in names:
                    (self.home / name).write_text("# keep user settings\n")
                for _ in range(2):
                    result = self.run_install(default=True)
                    self.assertEqual(result.returncode, 0, result.stderr)
                for name in names:
                    contents = (self.home / name).read_text()
                    self.assertTrue(contents.startswith("# keep user settings\n"))
                    self.assertEqual(contents.count("# Ormos installer: user executables"), 1)

    def test_archive_from_the_shared_build_action(self):
        fixture = os.environ.get("ORMOS_INSTALL_TEST_ARCHIVE")
        if not fixture:
            self.skipTest("Set ORMOS_INSTALL_TEST_ARCHIVE to validate an actual release build")
        path = self.assets / "ormos_Linux_x86_64.tar.gz"
        shutil.copyfile(fixture, path)
        (self.assets / "checksums.txt").write_text(f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n")
        version = os.environ.get("ORMOS_INSTALL_TEST_VERSION", "0.2.0")
        if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version):
            result = self.assert_failure_preserves_install("--version", "v0.2.0")
            self.assertIn(f"The binary reports {version}, expected 0.2.0", result.stderr)
        else:
            self.env["TEST_RELEASE_TAG"] = "v"+version
            result = self.run_install("--version", "v"+version)
            self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
