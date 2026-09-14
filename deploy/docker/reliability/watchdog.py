#!/usr/bin/env python3
"""Single-host liveness watchdog; readiness/dependency failures never restart."""
import datetime
import json
import os
from pathlib import Path
import subprocess
import time


def decision(state, live, now, started):
    state = dict(state)
    state['restarts'] = [t for t in state.get('restarts', []) if now - t < 3600]
    if live or now - started < 180:
        state['failures'] = 0
        return state, False
    state['failures'] = state.get('failures', 0) + 1
    restart = state['failures'] >= 3 and len(state['restarts']) < 3 and now - state.get('last_restart', 0) >= 300
    return state, restart


def run(args, timeout=10):
    return subprocess.run(args, capture_output=True, text=True, timeout=timeout, check=True).stdout


def main():
    project = os.environ.get('BKCRAB_COMPOSE_PROJECT', 'bkcrab')
    ids = run(['docker', 'ps', '-aq', '--filter', f'label=com.docker.compose.project={project}',
               '--filter', 'label=com.docker.compose.service=bkcrab']).split()
    if len(ids) != 1:
        print('No unique gateway container; watchdog takes no action.')
        return
    cid = ids[0]
    container = json.loads(run(['docker', 'inspect', cid]))[0]
    if not container['State']['Running'] or container['State'].get('Paused'):
        print('Gateway stopped/paused; preserving operator intent.')
        return
    started = datetime.datetime.fromisoformat(container['State']['StartedAt'].replace('Z', '+00:00')).timestamp()
    state_path = Path('/var/lib/bkcrab-watchdog/state.json')
    state_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    state = json.loads(state_path.read_text()) if state_path.exists() else {}
    if state.get('container') != cid:
        state['failures'] = 0
        state['container'] = cid
    live = True
    try:
        run(['docker', 'exec', cid, 'wget', '-qO-', '-T', '3', 'http://127.0.0.1:18953/livez'], 8)
    except (subprocess.SubprocessError, OSError):
        live = False
    now = time.time()
    state, restart = decision(state, live, now, started)
    if restart:
        # Persist the attempt first so repeated Docker failures cannot hammer it.
        state['restarts'].append(now)
        state['last_restart'] = now
        state['failures'] = 0
    temporary = state_path.with_suffix('.tmp')
    temporary.write_text(json.dumps(state))
    temporary.replace(state_path)
    if restart:
        print('Gateway liveness failed repeatedly; restarting the gateway only.', flush=True)
        run(['docker', 'restart', '--time', '60', cid], 90)
    elif not live:
        print('Gateway liveness failed; waiting for threshold/cooldown.', flush=True)
    if live:
        try:
            run(['docker', 'exec', cid, 'wget', '-qO-', '-T', '3', 'http://127.0.0.1:18953/readyz'], 8)
        except (subprocess.SubprocessError, OSError):
            print('Gateway alive but not ready; check dependencies. No restart.', flush=True)


if __name__ == '__main__':
    main()
