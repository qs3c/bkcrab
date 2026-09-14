"""Assign ext4 project quotas before exposing an agent directory to a sandbox.

Run in a short-lived privileged helper with only the quota filesystem mounted.
The gateway supplies the authenticated owner; agents never receive this helper.
"""
import fcntl
import json
import os
from pathlib import Path
import re
import subprocess
import sys


def ensure(owner, agent, byte_limit, file_limit, root=Path('/quota')):
    for value in (owner, agent):
        if not re.fullmatch(r'[A-Za-z0-9_-]{1,128}', value):
            raise ValueError('invalid owner/agent identifier')
    if byte_limit < 1024 or file_limit < 1:
        raise ValueError('quota limits must be positive')
    # Registry is outside every agent mount. Lock persists across helper runs.
    with (root / '.quota.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        registry_path = root / '.quota.json'
        registry = json.loads(registry_path.read_text()) if registry_path.exists() else {'users': {}, 'agents': {}}
        previous = registry['agents'].get(agent)
        if previous and previous != owner:
            raise ValueError('agent owner changed; migrate quota ownership explicitly')
        project = registry['users'].setdefault(owner, max(registry['users'].values(), default=9999) + 1)
        # Persist allocation before changing inodes; retry reuses the same ID.
        temporary = root / '.quota.json.tmp'
        temporary.write_text(json.dumps(registry))
        with temporary.open('r') as stream:
            os.fsync(stream.fileno())
        os.replace(temporary, registry_path)
        directory = root / agent
        if directory.is_symlink():
            raise ValueError('agent directory cannot be a symlink')
        directory.mkdir(exist_ok=True)
        subprocess.run(['setquota', '-P', str(project), '0', str(byte_limit // 1024), '0', str(file_limit), str(root)], check=True)
        # Existing trees are provisioned only once, before sandbox creation.
        # No shell expansion; the agent ID cannot inject flags or paths.
        if previous is None:
            subprocess.run(['chattr', '-R', '-p', str(project), str(directory)], check=True)
            for path, dirs, _ in os.walk(directory, followlinks=False):
                subprocess.run(['chattr', '+P', path], check=True)
        else:
            subprocess.run(['chattr', '-p', str(project), '+P', str(directory)], check=True)
        # The user skill mount is writable too, so account for it in the same
        # project rather than letting global skill installs bypass the budget.
        user_directory = root / '.users' / owner
        if user_directory.is_symlink():
            raise ValueError('user directory cannot be a symlink')
        user_directory.mkdir(parents=True, exist_ok=True)
        subprocess.run(['chattr', '-R', '-p', str(project), str(user_directory)], check=True)
        for path, dirs, _ in os.walk(user_directory, followlinks=False):
            subprocess.run(['chattr', '+P', path], check=True)
        registry['agents'][agent] = owner
        temporary.write_text(json.dumps(registry))
        with temporary.open('r') as stream:
            os.fsync(stream.fileno())
        os.replace(temporary, registry_path)
        print(json.dumps({'project': project, 'bytes': byte_limit, 'files': file_limit}))


if __name__ == '__main__':
    ensure(sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4]))
