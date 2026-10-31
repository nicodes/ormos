"""Exercise the public launcher contract with a separately executed API fixture."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest

BINARY = Path(os.environ.get("ORMOS_CLI", "target/debug/ormos")).resolve()
USAGE = (Path(__file__).resolve().parents[1] / "src/usage.txt").read_text()


class LauncherTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="ormos-launcher-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.api = self.root / "fixture-api"
        self.env = {**os.environ, "HOME": str(self.root), "ORMOS_API_BINARY": str(self.api)}

    def fixture(self, body):
        self.api.write_text(f"#!{sys.executable}\n" + body)
        self.api.chmod(0o700)

    def run_cli(self, *args, **extra):
        return subprocess.run([str(BINARY), *args], env={**self.env, **extra},
                              capture_output=True, text=True, timeout=10)

    def test_help_protocol_and_version(self):
        help_result = self.run_cli("--help")
        self.assertEqual((help_result.returncode, help_result.stdout, help_result.stderr), (0, "", USAGE))
        protocol = self.run_cli("--protocol-version")
        self.assertEqual((protocol.returncode, protocol.stdout, protocol.stderr), (0, "4\n", ""))
        version = self.run_cli("--version")
        self.assertEqual(version.returncode, 0)
        self.assertTrue(version.stdout.strip())
        self.assertEqual(version.stderr, "")

    def test_retired_entries_never_touch_state_or_execute_api(self):
        self.fixture("raise RuntimeError('API must not execute')\n")
        initial = set(self.root.iterdir())
        for args in [("relay",), ("--config", "/unreadable/ormos.json"), ("relay", "--help")]:
            result = self.run_cli(*args)
            self.assertEqual(result.returncode, 2)
            self.assertIn("hosted relay mode is retired", result.stderr)
            self.assertEqual(result.stdout, "")
            self.assertEqual(set(self.root.iterdir()), initial)

    def test_unexpected_and_extra_arguments_fail(self):
        for args in [("unknown",), ("--help", "extra"), ("--version", "extra")]:
            result = self.run_cli(*args)
            self.assertEqual(result.returncode, 2)
            self.assertIn("unexpected argument", result.stderr)
            self.assertIn(USAGE, result.stderr)

    def test_ui_and_default_forward_args_version_and_parent_identity(self):
        self.fixture("import json,os,sys\nprint(json.dumps({'args':sys.argv[1:],'parent':os.getppid()}))\n")
        version = self.run_cli("--version").stdout.strip()
        for args, expected in [((), []), (("ui", "--port", "19342", "--config", "/test/config"),
                                        ["--port", "19342", "--config", "/test/config"])]:
            result = self.run_cli(*args)
            self.assertEqual(result.returncode, 0, result.stderr)
            report = json.loads(result.stdout)
            self.assertEqual(report["args"], expected + ["--app-version", version,
                                                       "--parent-pid", str(report["parent"])])

    def test_ui_preserves_opaque_unix_argument_bytes(self):
        self.fixture("import os,sys\nprint(os.fsencode(sys.argv[2]).hex())\n")
        result = self.run_cli("ui", "--config", b"/test/\xff/config")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), b"/test/\xff/config".hex())

    def test_source_api_path_must_be_absolute_regular_and_executable(self):
        result = self.run_cli("ui", ORMOS_API_BINARY="relative-api")
        self.assertEqual(result.returncode, 2)
        self.assertIn("must be absolute", result.stderr)
        for path in [self.api, self.root]:
            result = self.run_cli("ui", ORMOS_API_BINARY=str(path))
            self.assertEqual(result.returncode, 2)
            self.assertIn("native Rust API is unavailable", result.stderr)
        self.api.write_text("not executable")
        self.assertEqual(self.run_cli("ui").returncode, 2)

    def test_api_failure_is_reported_as_launcher_failure(self):
        self.fixture("raise SystemExit(7)\n")
        result = self.run_cli("ui")
        self.assertEqual(result.returncode, 2)
        self.assertIn("error:", result.stderr)

    def test_sigint_and_sigterm_are_forwarded_to_api(self):
        ready = self.root / "ready"
        for forwarded in [signal.SIGINT, signal.SIGTERM]:
            ready.unlink(missing_ok=True)
            self.fixture("import signal,sys,time\nfrom pathlib import Path\n"
                         "def finish(number,frame):\n print(number,flush=True)\n raise SystemExit(0)\n"
                         "signal.signal(signal.SIGINT,finish)\nsignal.signal(signal.SIGTERM,finish)\n"
                         f"Path({str(ready)!r}).write_text('ready')\n"
                         "while True: time.sleep(.05)\n")
            child = subprocess.Popen([str(BINARY), "ui"], env=self.env,
                                     stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                deadline = time.monotonic() + 5
                while not ready.exists():
                    self.assertIsNone(child.poll())
                    self.assertLess(time.monotonic(), deadline)
                    time.sleep(.01)
                child.send_signal(forwarded)
                stdout, stderr = child.communicate(timeout=5)
                self.assertEqual(child.returncode, 0, stderr)
                self.assertEqual(stdout, f"{forwarded.value}\n")
            finally:
                if child.poll() is None:
                    child.kill()
                child.wait(timeout=5)


if __name__ == "__main__":
    unittest.main()
