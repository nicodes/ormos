"""Exercise the embedded native API from a release archive, with owned PTYs only."""
import json
import os
from pathlib import Path
import platform
import signal
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

root = Path(__file__).resolve().parents[1]
arch = "arm64" if platform.machine() in ("arm64", "aarch64") else "x86_64"
binary = root / "dist" / f"ormos_{platform.system()}_{arch}" / "ormos"
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
with tempfile.TemporaryDirectory(prefix="ormos-native-smoke-") as directory:
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    origin = f"http://127.0.0.1:{port}"
    child = subprocess.Popen([str(binary), "ui", "--port", str(port)],
                             env={**os.environ, "HOME": directory, "SHELL": "/bin/sh"},
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    def request(path, body=None):
        raw = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(origin + path, data=raw,
                                     headers={"Origin": origin, "Content-Type": "application/json"})
        with opener.open(req, timeout=5) as response:
            return json.load(response)
    try:
        deadline = time.monotonic() + 15
        while True:
            try:
                report = request("/api/system")
                break
            except (OSError, urllib.error.URLError):
                assert child.poll() is None, "embedded API exited before readiness"
                assert time.monotonic() < deadline, "embedded API readiness timed out"
                time.sleep(0.05)
        assert report["v"] == 1 and report["terminals"] == 0
        opened = request("/api/action", {"action": "open", "shell": "/bin/sh"})
        assert opened["id"].startswith("t_")
        rows = request("/api/terminals")["terminals"]
        assert len(rows) == 1 and rows[0]["alive"]
        killed = request("/api/action", {"action": "kill", "id": opened["id"]})
        assert killed["alive"] is False
        assert request("/api/terminal/" + opened["id"] + "/output")["v"] == 1
    finally:
        if child.poll() is None:
            child.send_signal(signal.SIGTERM)
        child.wait(timeout=10)
    assert child.returncode == 0
print("embedded Rust API: listener, real PTY, scoped kill, and shutdown passed")
