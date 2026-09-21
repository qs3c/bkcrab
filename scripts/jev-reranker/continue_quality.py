#!/usr/bin/env python3
"""Complete the already running experiment after a valid official-judge smoke.

This is a single bounded continuation of the current run, not a recurring job.
Credentials are read by the existing launchers and never enter this script.
"""
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import time

from analyze import analyze, records

root=Path(sys.argv[1]).resolve()
scripts=Path(__file__).parent
data=json.loads((root/'combined.json').read_text(encoding='utf-8'))
smoke=records(root/'official-direct-smoke-v3-scores.jsonl')
assert len(smoke)==15, 'all five questions and three arms must finish smoke first'
assert all(v['status']=='ok' for row in smoke for v in row['response']['results'][0]['metrics'].values()), 'judge smoke has failed metrics; full scoring not started'
print(json.dumps({'stage':'smoke-validated','samples':len(smoke),'metrics':sum(len(row['response']['results'][0]['metrics']) for row in smoke)}),flush=True)
expected={(s['id'],a) for s in data['cases'] if s['split']=='test' for a in ['qwen3','jev','rrf']}
deadline=time.monotonic()+4*3600
while True:
    # Ignore an incomplete last line while the active process appends it.
    lines=(root/'formal-ranks.jsonl').read_text(encoding='utf-8').splitlines(keepends=True)
    rows=[json.loads(line) for line in lines if line.endswith('\n')]
    first=[r for r in rows if r['phase']=='test' and r['repeat']==0]
    keys={(r['caseId'],r['arm']) for r in first}
    assert len(keys)==len(first), 'duplicate first-round results'
    if keys==expected:break
    if time.monotonic()>deadline:raise RuntimeError('first-round wait budget reached')
    time.sleep(10)
snapshot=root/'formal-first-round.jsonl'
encoded=''.join(json.dumps(r,ensure_ascii=False)+'\n' for r in first)
if snapshot.exists():assert snapshot.read_text(encoding='utf-8')==encoded
else:snapshot.write_text(encoded,encoding='utf-8')
print(json.dumps({'stage':'first-round-frozen','records':len(first),'sha256':hashlib.sha256(encoded.encode()).hexdigest()}),flush=True)

def run_container(args,name):
    subprocess.run(args,stdin=subprocess.DEVNULL,check=True)
    exit_code=subprocess.check_output(['docker','wait',name],text=True).strip()
    if exit_code!='0':raise RuntimeError(name+' failed; inspect its logs')

run_container([sys.executable,str(scripts/'docker_run.py'),str(root),'jev-bench-official-formal-answer',
    '-mode','answer','-input','combined.json','-ranks','formal-first-round.jsonl','-output','official-formal-answers.jsonl',
    '-answer-model','deepseek-official/deepseek-v4-flash','-answer-token-budget','1500000','-warmups','0','-repeats','1'],
    'jev-bench-official-formal-answer')
print(json.dumps({'stage':'formal-answers-finished'}),flush=True)
run_container([sys.executable,str(scripts/'direct_judge_docker.py'),str(root),'jev-bench-official-formal-judge',
    '/experiment/combined.json','/experiment/official-formal-answers.jsonl','/experiment/official-formal-scores.jsonl',
    '--token-budget','6000000','--cost-budget-usd','4'],'jev-bench-official-formal-judge')
print(json.dumps({'stage':'formal-judge-finished'}),flush=True)
exit_code=subprocess.check_output(['docker','wait','jev-bench-formal'],text=True).strip()
for language in ['en','zh']:
    selected=dict(data,cases=[s for s in data['cases'] if s['language']==language])
    result=analyze(selected,records(root/'formal-ranks.jsonl'),records(root/'official-formal-answers.jsonl'),records(root/'official-formal-scores.jsonl'))
    result['rankContainerExitCode']=exit_code
    result['answerModel']='deepseek-official/deepseek-v4-flash (served as deepseek-flash / V4.1)'
    result['judgeThinking']='disabled'
    (root/f'official-formal-summary-{language}.json').write_text(json.dumps(result,ensure_ascii=False,indent=2),encoding='utf-8')
print(json.dumps({'stage':'complete','rankExitCode':exit_code}),flush=True)
