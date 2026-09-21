#!/usr/bin/env python3
"""Run the isolated benchmark with the live deployment's model/storage bindings.

Run on the Linux server. Optional OpenRouter credential arrives on stdin as
JSON and is passed through the child environment, never a command argument or
artifact. No production container is stopped or reconfigured.
"""
import json
import os
from pathlib import Path
import subprocess
import sys

root = Path(sys.argv[1]).resolve()
name = sys.argv[2]
arguments = sys.argv[3:]
assert name.startswith('jev-bench-') and root.is_dir()
items = json.loads(subprocess.check_output(['docker', 'inspect', 'bkcrab-bkcrab-1']))
source = dict(item.split('=', 1) for item in items[0]['Config']['Env'])
env = os.environ.copy()
keys = []
for key, value in source.items():
    if key.startswith('BKCRAB_RAG_') or key in ['BKCRAB_STORAGE_TYPE', 'BKCRAB_STORAGE_DSN']:
        env[key] = value
        keys.append(key)
if not sys.stdin.isatty():
    payload = sys.stdin.read().strip()
    if payload:
        credential = json.loads(payload).get('openrouterKey', '')
        if credential:
            env['OPENROUTER_API_KEY'] = credential
            keys.append('OPENROUTER_API_KEY')
cmd = ['docker', 'run', '-d', '--name', name, '--network', 'bkcrab_default',
       '--cpus', '1', '--memory', '768m', '--read-only', '--cap-drop', 'ALL',
       '--security-opt', 'no-new-privileges', '--tmpfs', '/tmp:rw,noexec,nosuid,size=64m',
       '--mount', f'type=bind,src={root},dst=/experiment', '--workdir', '/experiment']
for key in keys:
    cmd += ['--env', key]
cmd += ['--entrypoint', '/experiment/rag-rerank-bench', items[0]['Image'], *arguments]
subprocess.run(cmd, env=env, check=True)
