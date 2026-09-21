#!/usr/bin/env python3
"""Standard-library analysis; failures remain missing, never become zero scores."""
import argparse
import collections
import json
import math
from pathlib import Path
import random
import statistics


def percentile(values, p):
    if not values:
        return None
    values = sorted(values)
    at = (len(values)-1)*p
    low = math.floor(at)
    high = math.ceil(at)
    return values[low]*(high-at)+values[high]*(at-low) if high != low else values[low]


def summary(values):
    return {'n': len(values), 'mean': statistics.mean(values) if values else None,
            'p50': percentile(values, .5), 'p95': percentile(values, .95)}


def paired_cluster_ci(left, right, groups, iterations=10000):
    ids = sorted(left.keys() & right.keys())
    clusters = collections.defaultdict(list)
    for case in ids:
        clusters[groups[case]].append(right[case]-left[case])
    values = list(clusters.values())
    if not values:
        return {'pairs': 0, 'clusters': 0, 'difference': None, 'ci95': None}
    rng = random.Random(20260921)
    boot = []
    for _ in range(iterations):
        selected = [v for _ in values for v in rng.choice(values)]
        boot.append(statistics.mean(selected))
    return {'pairs': len(ids), 'clusters': len(values),
            'difference': statistics.mean([v for vs in values for v in vs]),
            'ci95': [percentile(boot, .025), percentile(boot, .975)]}


def records(path):
    return [json.loads(line) for line in path.read_text(encoding='utf-8').splitlines()] if path.exists() else []


def analyze(data, ranks, answers, scores):
    cases = {s['id']: s for s in data['cases'] if s['split'] == 'test'}
    groups = {i: s['group'] for i, s in cases.items()}
    result = {'cases': len(cases), 'latency': {}, 'quality': {}, 'paired': {}, 'answer': {}, 'jevUsage': {}}
    arms = sorted({r['arm'] for r in ranks})
    quality = collections.defaultdict(lambda: collections.defaultdict(dict))
    costs = []
    models = collections.Counter()
    for r in ranks:
        for call in r.get('calls') or []:
            costs.append(call['costUSD'])
            if call['model']:
                models[call['model']] += 1
    result['jevUsage'] = {'reportedCostUSD': sum(costs), 'attemptedCalls': len(costs), 'actualModels': dict(models)}
    for arm in arms:
        rs = [r for r in ranks if r['phase'] == 'test' and r['arm'] == arm and r['caseId'] in cases]
        success = [r for r in rs if r['status'] == 'ok']
        result['latency'][arm] = {'attempted': len(rs), 'succeeded': len(success),
                                  'successRate': len(success)/len(rs) if rs else None,
                                  'successfulMs': summary([r['durationMs'] for r in success]),
                                  'allAttemptMs': summary([r['durationMs'] for r in rs]),
                                  'errors': dict(collections.Counter(r.get('error') for r in rs if r['status'] != 'ok'))}
        for r in success:
            if r['repeat'] != 0:
                continue
            s = cases[r['caseId']]
            selected = [s['candidates'][i['Index']] for i in r['scores'][:5]]
            gold = set(s.get('relevantIds') or [])
            if gold:
                positions = [i+1 for i, c in enumerate(selected) if c['id'] in gold]
                quality[arm]['labelled_hit_at_5'][s['id']] = float(bool(positions))
                quality[arm]['labelled_mrr_at_5'][s['id']] = 1/min(positions) if positions else 0
                dcg = sum(1/math.log2(p+1) for p in positions)
                idcg = sum(1/math.log2(p+1) for p in range(1, min(5,len(gold))+1))
                quality[arm]['labelled_ndcg_at_5'][s['id']] = dcg/idcg
            docs = set(s.get('referenceDocumentIds') or [])
            if docs:
                quality[arm]['document_recall_at_5'][s['id']] = len(docs & {c['hit']['docId'] for c in selected})/len(docs)
        ars = [r for r in answers if r['arm'] == arm and r['caseId'] in cases]
        ok = [r for r in ars if r['status'] == 'ok']
        result['answer'][arm] = {'attempted': len(ars), 'succeeded': len(ok), 'successfulMs': summary([r['durationMs'] for r in ok]),
                                 'inputTokens': sum(r['answer']['usage']['inputTokens'] for r in ars if r.get('answer')),
                                 'outputTokens': sum(r['answer']['usage']['outputTokens'] for r in ars if r.get('answer'))}
        for r in ok:
            s = cases[r['caseId']]
            # String inclusion is an auxiliary factual coverage check, not a
            # substitute for semantic correctness or justified abstention.
            if s.get('acceptableAnswers'):
                quality[arm]['reference_string_in_answer'][s['id']] = float(any(a in r['answer']['response'] for a in s['acceptableAnswers']))
    statuses = collections.defaultdict(collections.Counter)
    for row in scores:
        if row['status'] != 'ok' or row['caseId'] not in cases:
            continue
        for metric, value in row['response']['results'][0]['metrics'].items():
            statuses[row['arm']+':'+metric][value['status']] += 1
            if value['status'] == 'ok' and isinstance(value.get('value'), (int,float)):
                quality[row['arm']][metric][row['caseId']] = value['value']
    result['metricStatuses'] = {k:dict(v) for k,v in statuses.items()}
    for arm, metrics in quality.items():
        result['quality'][arm] = {m:summary(list(v.values())) for m,v in metrics.items()}
    for other in ['jev','rrf']:
        for metric in quality['qwen3'].keys() & quality[other].keys():
            result['paired'][other+'-qwen3:'+metric] = paired_cluster_ci(quality['qwen3'][metric], quality[other][metric], groups)
    return result


if __name__ == '__main__':
    p = argparse.ArgumentParser()
    p.add_argument('input', type=Path)
    p.add_argument('ranks', type=Path)
    p.add_argument('output', type=Path)
    p.add_argument('--answers', type=Path, default=Path('missing-answers'))
    p.add_argument('--scores', type=Path, default=Path('missing-scores'))
    a = p.parse_args()
    result = analyze(json.loads(a.input.read_text(encoding='utf-8')), records(a.ranks), records(a.answers), records(a.scores))
    a.output.write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding='utf-8')
    print(json.dumps(result, ensure_ascii=False, indent=2))
