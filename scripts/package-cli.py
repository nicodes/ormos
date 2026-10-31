"""Build native Rust launchers and assemble archives from exact source receipts."""
import gzip
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import tarfile

root = Path(__file__).resolve().parents[1]
os.chdir(root)
artifacts = root / ".artifacts/native-api"
source = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
version = os.environ.get("VERSION", "dev").removeprefix("v")
targets = {
    "Linux_x86_64": "x86_64-unknown-linux-musl",
    "Linux_arm64": "aarch64-unknown-linux-musl",
    "Darwin_x86_64": "x86_64-apple-darwin",
    "Darwin_arm64": "aarch64-apple-darwin",
}


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def read_api(label):
    path = artifacts / f"ormos-api_{label}"
    receipt = json.loads(path.with_suffix(".json").read_text())
    assert receipt["source"] == source, "native API source mismatch"
    assert receipt["sha256"] == digest(path), "native API digest mismatch"
    return path, receipt


def linkage(binary, label):
    if label.startswith("Linux_"):
        result = subprocess.check_output(["readelf", "-l", "-d", str(binary)], text=True)
        assert "INTERP" not in result and "NEEDED" not in result, "Linux release must be static"


def build(label):
    api, receipt = read_api(label)
    payload = api.with_suffix(".gz")
    payload.write_bytes(gzip.compress(api.read_bytes(), mtime=0))
    target = targets[label]
    env = {**os.environ, "ORMOS_API_PAYLOAD": str(payload),
           "ORMOS_API_DIGEST": receipt["sha256"], "VERSION": version, "COMMIT": source}
    try:
        subprocess.run(["cargo", "build", "--locked", "--release", "--target", target,
                        "--bin", "ormos"], env=env, check=True)
    finally:
        payload.unlink(missing_ok=True)
    built = Path(os.environ.get("CARGO_TARGET_DIR", "target")) / target / "release/ormos"
    output = artifacts / f"ormos-cli_{label}"
    shutil.copy2(built, output)
    linkage(output, label)
    output.with_suffix(".json").write_text(json.dumps({
        "schema": 1, "source": source, "version": version, "sha256": digest(output),
        "api_sha256": receipt["sha256"], "platform": label,
    }, sort_keys=True) + "\n")


def package(labels):
    dist = root / "dist"
    if dist.is_symlink():
        raise SystemExit("refusing a symlinked dist directory")
    shutil.rmtree(dist, ignore_errors=True)
    dist.mkdir()
    for label in labels:
        _, api = read_api(label)
        binary = artifacts / f"ormos-cli_{label}"
        receipt = json.loads(binary.with_suffix(".json").read_text())
        assert receipt == {"schema": 1, "source": source, "version": version,
                           "sha256": digest(binary), "api_sha256": api["sha256"],
                           "platform": label}, "native launcher identity mismatch"
        linkage(binary, label)
        name = f"ormos_{label}"
        directory = dist / name
        directory.mkdir()
        shutil.copy2(binary, directory / "ormos")
        # Artifact downloads do not preserve Unix executable permissions.
        (directory / "ormos").chmod(0o755)
        for file in ("README.md", "LICENSE"):
            shutil.copy2(root / file, directory / file)
        third_party = directory / "yamux-licenses.txt"
        third_party.write_text("Yamux 0.14.1, modified for Ormos framing and stream limits.\n\n" +
                               (root / "vendor/yamux/LICENSE-MIT").read_text() + "\n" +
                               (root / "vendor/yamux/LICENSE-APACHE").read_text())
        with tarfile.open(dist / f"{name}.tar.gz", "w:gz") as archive:
            for file in ("ormos", "README.md", "LICENSE", "yamux-licenses.txt"):
                archive.add(directory / file, arcname=file)
    shutil.copy2(root / "install.sh", dist / "install.sh")
    files = sorted(dist.glob("*.tar.gz")) + [dist / "install.sh"]
    (dist / "checksums.txt").write_text("".join(f"{digest(path)}  {path.name}\n" for path in files))


native_arch = "arm64" if platform.machine() in ("aarch64", "arm64") else "x86_64"
native = f"{platform.system()}_{native_arch}"
if native not in targets:
    raise SystemExit("The CLI supports Linux and macOS on x86_64 and arm64.")
if sys.argv[1:] == ["all"]:
    package(targets)
else:
    build(native)
    package([native])
