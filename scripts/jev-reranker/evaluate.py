#!/usr/bin/env python3
"""Score saved answers in the existing evaluator container, without DB writes.

Input is explicitly public benchmark text. Provider credentials never leave
the evaluator. Every outcome is persisted once, including metric failures.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess

p = argparse.ArgumentParser()
p.add_argument('input', type=Path)
p.add_argument('answers', type=Path)
p.add_argument('output', type=Path)
p.add_argument('--limit', type=int, default=0)
p.add_argument('--token-budget', type=int, default=3000000)
p.add_argument('--owner', default='u_447a2f8f07032ad989d7')
p.add_argument('--metrics', default='context_precision,context_recall,factual_correctness,faithfulness,response_relevancy')
a = p.parse_args()
cases = {s['id']: s for s in json.loads(a.input.read_text())['cases']}
answers = [json.loads(line) for line in a.answers.read_text().splitlines()]
prior = [json.loads(line) for line in a.output.read_text().splitlines()] if a.output.exists() else []
done = {row['requestId'] for row in prior}
tokens = sum(row.get('tokens', 0) for row in prior)
pending = []
for row in answers:
    if row['status'] != 'ok':
        continue
    case = cases[row['caseId']]
    # Standard Ragas recall requires an answerable reference. Evidence removal
    # is scored with explicit abstention diagnostics in analyze.py instead.
    if case['expectedAbstention']:
        continue
    candidates = {c['id']: c for c in case['candidates']}
    sample = {'caseId': row['caseId'] + ':' + row['arm'], 'userInput': case['query'],
              'response': row['answer']['response'], 'reference': case['reference'],
              'referenceContexts': case['referenceContexts'],
              'retrievedContexts': [candidates[i]['hit']['content'] for i in row['contextIds']],
              'retrievedContextIds': row['contextIds']}
    body = {'metricBundleVersion': 'rag-core-v1', 'metrics': a.metrics.split(','), 'samples': [sample]}
    request_id = 'jev:' + hashlib.sha256(json.dumps(body, sort_keys=True, ensure_ascii=False).encode()).hexdigest()[:48]
    body['requestId'] = request_id
    if request_id not in done:
        pending.append((row['caseId'], row['arm'], body))
if a.limit:
    pending = pending[:a.limit]
print(json.dumps({'pending': len(pending), 'previousTokens': tokens, 'tokenBudget': a.token_budget}), flush=True)

worker = r'''
import datetime,json,time
import httpx
from app.settings import Settings
settings=Settings.from_env()
pending,owner,tokens,budget=PAYLOAD
assert settings.llm_model=='bkcrab-default'
assert settings.llm_endpoint.rstrip('/')=='http://bkcrab:18953/internal/rag-eval/judge/v1'
headers={'Authorization':'Bearer '+settings.api_key,'X-BkCrab-Eval-Owner':owner}
deadline=time.monotonic()+5*3600
with httpx.Client(base_url='http://127.0.0.1:8080',headers=headers,timeout=660) as client:
 for case_id,arm,body in pending:
  if tokens>=budget or time.monotonic()>=deadline:
   raise RuntimeError('scoring budget reached')
  start=time.monotonic()
  row={'caseId':case_id,'arm':arm,'requestId':body['requestId'],'startedAt':datetime.datetime.now(datetime.timezone.utc).isoformat()}
  try:
   response=client.post('/v1/evaluate',json=body)
   row['httpStatus']=response.status_code
   response.raise_for_status()
   result=response.json()
   assert result['requestId']==body['requestId'] and result['ragasVersion']=='0.3.9'
   assert len(result['results'])==1 and result['results'][0]['caseId']==body['samples'][0]['caseId']
   usage=result['usage']
   row['tokens']=sum(usage.get(k,0) for k in ['llmInputTokens','llmOutputTokens','embeddingInputTokens'])
   tokens+=row['tokens']
   row['response']=result
   row['status']='ok'
  except Exception as exc:
   row['status']='error';row['errorType']=type(exc).__name__
  row['durationMs']=round((time.monotonic()-start)*1000)
  print(json.dumps(row,ensure_ascii=False),flush=True)
'''.replace('PAYLOAD', repr((pending, a.owner, tokens, a.token_budget)))
process = subprocess.Popen(['docker','exec','-i','bkcrab-rag-evaluator-1','python','-u','-'],
                           stdin=subprocess.PIPE,stdout=subprocess.PIPE,text=True)
process.stdin.write(worker)
process.stdin.close()
with a.output.open('a', encoding='utf-8') as output:
    for line in process.stdout:
        row = json.loads(line)
        output.write(line)
        output.flush()
        os.fsync(output.fileno())
        print(json.dumps({k: row[k] for k in ['caseId','arm','status','durationMs']}),flush=True)
raise SystemExit(process.wait())
