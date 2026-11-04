"""Keep the reusable relay SDK portable and reject unsupported CLI targets."""
from pathlib import Path
import subprocess

root = Path(__file__).resolve().parents[2]
target = 'x86_64-pc-windows-gnu'
subprocess.run(['cargo', 'check', '--locked', '--offline', '-p', 'ormos-relay',
                '--all-features', '--target', target], cwd=root, check=True)
result = subprocess.run(['cargo', 'check', '--locked', '--offline', '--bin', 'ormos',
                         '--target', target], cwd=root, text=True, capture_output=True)
assert result.returncode != 0, 'unsupported CLI platform unexpectedly compiled'
assert 'The ormos CLI supports Linux and macOS only' in result.stderr, result.stderr
print('Windows SDK compiles; unsupported CLI platform is explicitly refused')
