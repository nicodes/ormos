"""Keep the released launcher's protocol and local workspace help contracts."""
import os
import subprocess

binary = os.environ['COOKBOOK_CLI_BINARY']
assert subprocess.check_output([binary, '--protocol-version'], text=True, timeout=10).strip() == '4'
result = subprocess.run([binary, 'ui', '--help'], text=True, capture_output=True, timeout=10, check=True)
assert 'local workspace' in result.stdout, result.stdout
print('Released launcher: protocol v4 and local workspace help passed')
