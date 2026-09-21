#!/usr/bin/env python3
"""Finish this running experiment's local analysis after its Docker process exits."""
import gzip
import json
from pathlib import Path
import subprocess
import sys

from analyze import analyze, records

root = Path(sys.argv[1]).resolve()
container = sys.argv[2]
assert container.startswith('jev-bench-')
exit_code = subprocess.check_output(['docker', 'wait', container], text=True).strip()
data = json.loads((root/'combined.json').read_text(encoding='utf-8'))
ranks = records(root/'formal-ranks.jsonl')
for language in ['en', 'zh']:
    selected = dict(data, cases=[c for c in data['cases'] if c['language'] == language])
    result = analyze(selected, ranks, [], [])
    result['containerExitCode'] = exit_code
    result['qualityStatus'] = 'ranking-only summary; see official-formal-summary files for answer/judge results'
    (root/f'formal-ranking-summary-{language}.json').write_text(json.dumps(result,ensure_ascii=False,indent=2),encoding='utf-8')
for name in ['formal-ranks.jsonl', 'combined.json']:
    (root/(name+'.gz')).write_bytes(gzip.compress((root/name).read_bytes()))
print(json.dumps({'containerExitCode':exit_code,'records':len(ranks),'summaries':['en','zh']}),flush=True)
