#!/usr/bin/env python3
"""Compact progress from append-only artifacts; no provider configuration read."""
import collections
import json
from pathlib import Path
import statistics
import sys

root = Path(sys.argv[1])
data = json.loads((root/'combined.json').read_text(encoding='utf-8'))
cases = {c['id']:c for c in data['cases']}
for filename in ['formal-ranks.jsonl','formal-answers.jsonl','formal-scores.jsonl',
                 'official-formal-answers.jsonl','official-formal-scores.jsonl']:
    path = root/filename
    if not path.exists():
        continue
    groups = collections.defaultdict(list)
    for line in path.read_text(encoding='utf-8').splitlines(keepends=True):
        if not line.endswith('\n'):
            continue  # The producer may still be appending the final record.
        r = json.loads(line)
        if r.get('phase', 'test') != 'test':
            continue
        groups[(cases[r['caseId']]['language'],r['arm'])].append(r)
    out = {}
    for (language,arm),rows in groups.items():
        ok = [r for r in rows if r['status'] == 'ok']
        out[language+':'+arm] = {'attempted':len(rows), 'ok':len(ok),
             'meanMs':round(statistics.mean(r['durationMs'] for r in ok),1) if ok else None,
             'errors':dict(collections.Counter(r.get('error',r.get('errorType')) for r in rows if r['status']!='ok')),
             'metricStatuses':dict(collections.Counter(
                 metric['status'] for r in rows
                 for result in r.get('response',{}).get('results',[])
                 for metric in result.get('metrics',{}).values()))}
    print(json.dumps({'file':filename,'groups':out}, ensure_ascii=False))
