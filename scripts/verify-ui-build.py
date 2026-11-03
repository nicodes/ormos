"""Verify the generated UI is complete and reproducible before embedding it."""
import hashlib
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]


def snapshot(directory):
    result = {}
    for path in directory.rglob("*"):
        if path.is_symlink():
            raise SystemExit("UI output must not contain symlinks")
        if path.is_file():
            result[path.relative_to(directory).as_posix()] = hashlib.sha256(path.read_bytes()).hexdigest()
    if "index.html" not in result:
        raise SystemExit("Build the UI before verifying or embedding it")
    return result


expected = snapshot(ROOT / ".artifacts/ui")
with tempfile.TemporaryDirectory(prefix="ui-rebuild-") as directory:
    output = Path(directory) / "ui"
    subprocess.run(["npm", "run", "build", "--", "--outDir", str(output)],
                   cwd=ROOT / "app", check=True)
    if snapshot(output) != expected:
        raise SystemExit("Locked UI builds produced different output bytes")
print("UI output is complete and byte-reproducible")
