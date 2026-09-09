#!/usr/bin/env python3
"""Supplement missing A scores using saved public-benchmark answers; never write DB.

Requires the existing local Docker deployment. Output is an append-only sidecar
response JSONL; reruns skip completed cases, including metric errors. No provider
key is read by the host process. Only the fixed, authorized Open RAGBench run is
eligible. This is an experiment recovery script, not a platform retry endpoint.
"""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

RUN = "rer_74edcb129e6a9a252f5c4fdc6c8b0969"
DATASET = "rdv_c4fa198f-af95-4798-8d15-bc28efd81762"
METRICS = ["context_precision", "context_recall", "faithfulness", "response_relevancy", "factual_correctness"]
OUTPUT = Path(sys.argv[1])
OUTPUT.parent.mkdir(parents=True, exist_ok=True)

sql = f"""START TRANSACTION WITH CONSISTENT SNAPSHOT, READ ONLY;
SELECT JSON_OBJECT('owner',r.created_by,'source',v.source_config_json,
 'sample',JSON_OBJECT('caseId',c.case_id,'userInput',q.user_input,'response',c.response,
 'reference',q.reference_answer,'retrievedContexts',c.contexts_json,
 'retrievedContextIds',c.citations_json,'referenceContexts',q.reference_contexts_json),
 'coverage',(SELECT JSON_ARRAYAGG(m.metric_name) FROM rag_eval_metric_results m
 WHERE m.run_id=c.run_id AND m.case_id=c.case_id AND m.metric_version='rag-core-v1'))
FROM rag_eval_case_results c JOIN rag_eval_runs r ON c.run_id=r.id
JOIN rag_eval_cases q ON c.case_id=q.id JOIN rag_eval_dataset_versions v ON q.dataset_version_id=v.id
WHERE r.id='{RUN}' AND r.status='BUDGET_EXCEEDED' AND c.status='ok'
AND v.id='{DATASET}' AND v.source_type='builtin-catalog'
AND JSON_UNQUOTE(JSON_EXTRACT(v.source_config_json,'$.catalogId'))='vectara-open-ragbench'
AND JSON_UNQUOTE(JSON_EXTRACT(v.source_config_json,'$.revision'))='63f6b052ff83508b08e242db42263ee708815c26'
AND JSON_UNQUOTE(JSON_EXTRACT(v.source_config_json,'$.split'))='arxiv'
ORDER BY c.case_id;
COMMIT;"""
export = subprocess.run(
    ['docker', 'exec', '-i', 'bkcrab-mysql-1', 'sh', '-c',
     'MYSQL_PWD="$MYSQL_PASSWORD" exec mysql -u "$MYSQL_USER" "$MYSQL_DATABASE" --default-character-set=utf8mb4 -N -B --raw'],
    input=sql, text=True, capture_output=True, check=True,
)
rows = [json.loads(line) for line in export.stdout.splitlines()]
assert len(rows) == 50, 'expected exactly 50 saved answers from the authorized public dataset'
owners = {r['owner'] for r in rows}
assert len(owners) == 1
existing = [json.loads(line) for line in OUTPUT.read_text().splitlines()] if OUTPUT.exists() else []
assert all(r['sourceRunId'] == RUN for r in existing)
done = {item['caseId'] for r in existing for item in r['response']['results']}
prior_tokens = sum(r['response']['usage']['llmInputTokens'] + r['response']['usage']['llmOutputTokens'] + r['response']['usage']['embeddingInputTokens'] for r in existing)
pending = []
for row in rows:
    sample = row['sample']
    for key in ['retrievedContexts', 'retrievedContextIds', 'referenceContexts']:
        sample[key] = json.loads(sample[key]) if isinstance(sample[key], str) else sample[key]
        sample[key] = sample[key] or []
    assert len(sample['retrievedContexts']) == len(sample['retrievedContextIds'])
    coverage = row['coverage'] or []
    coverage = json.loads(coverage) if isinstance(coverage, str) else coverage
    missing = [m for m in METRICS if m not in coverage]
    if not missing or sample['caseId'] in done:
        continue
    body = {'metricBundleVersion': 'rag-core-v1', 'metrics': missing, 'samples': [sample]}
    digest = hashlib.sha256(json.dumps(body, sort_keys=True, ensure_ascii=False).encode()).hexdigest()[:32]
    body['requestId'] = f'supplement:{RUN}:{digest}'
    pending.append(body)
assert len(pending) <= 48
print(json.dumps({'pendingCases': len(pending), 'alreadySupplemented': len(done), 'output': str(OUTPUT)}), flush=True)

worker = r'''
import datetime, json, os, time
import httpx
from app.settings import Settings
settings = Settings.from_env()
assert settings.llm_endpoint.rstrip('/') == 'http://bkcrab:18953/internal/rag-eval/judge/v1'
assert settings.llm_model == 'bkcrab-default' and settings.llm_max_tokens == 8192
requests, owner, tokens, run = PAYLOAD
deadline = time.monotonic() + 3 * 3600
headers = {'Authorization': 'Bearer ' + settings.api_key, 'X-BkCrab-Eval-Owner': owner}
with httpx.Client(base_url='http://127.0.0.1:8080', headers=headers, timeout=660) as client:
    for body in requests:
        if tokens >= 2_000_000 or time.monotonic() >= deadline:
            raise RuntimeError('supplement scoring budget reached')
        started = time.monotonic()
        response = client.post('/v1/evaluate', json=body)
        response.raise_for_status()
        result = response.json()
        assert result['requestId'] == body['requestId']
        assert result['ragasVersion'] == '0.3.9' and result['metricBundleVersion'] == 'rag-core-v1'
        assert len(result['results']) == 1 and result['results'][0]['caseId'] == body['samples'][0]['caseId']
        assert set(result['results'][0]['metrics']) == set(body['metrics'])
        assert all(len(m['reason'].encode()) <= 2048 for m in result['results'][0]['metrics'].values())
        usage = result['usage']
        tokens += usage['llmInputTokens'] + usage['llmOutputTokens'] + usage['embeddingInputTokens']
        print(json.dumps({'sourceRunId': run, 'kind': 'supplement',
            'completedAt': datetime.datetime.now(datetime.timezone.utc).isoformat(),
            'durationSeconds': round(time.monotonic()-started, 3),
            'judgeMaxTokens': 8192, 'cumulativeSupplementTokens': tokens,
            'response': result}, ensure_ascii=False), flush=True)
'''.replace('PAYLOAD', repr((pending, next(iter(owners)), prior_tokens, RUN)))
process = subprocess.Popen(['docker', 'exec', '-i', 'bkcrab-rag-evaluator-1', 'python', '-u', '-'],
                           stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
assert process.stdin is not None and process.stdout is not None
process.stdin.write(worker)
process.stdin.close()
with OUTPUT.open('a') as output:
    for line in process.stdout:
        record = json.loads(line)
        output.write(line)
        output.flush()
        os.fsync(output.fileno())
        item = record['response']['results'][0]
        print(json.dumps({'caseId': item['caseId'], 'durationSeconds': record['durationSeconds'],
                          'statuses': {m: v['status'] for m, v in item['metrics'].items()},
                          'tokens': record['cumulativeSupplementTokens']}), flush=True)
raise SystemExit(process.wait())
