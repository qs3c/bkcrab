#!/usr/bin/env python3
"""Construct a reproducible component diagnostic, NOT a full retrieval benchmark.

Source: CMRC2018 dev, ymcui/cmrc2018 commit
c0eb1b6ba219847457e6af3180da722bbeb656af, squad-style-data/cmrc2018_dev.json.
One question per source paragraph; calibration/test paragraphs are disjoint.
Distractors use character-bigram BM25. For answerable cases the labelled gold
paragraph is inserted at a random rank 6..20, deliberately testing recovery
from weak first-stage retrieval. Ten held-out cases remove gold and passages
containing its annotated answer: these are evidence-removal diagnostics, not
human-certified real-world unanswerable questions.
"""
import collections
import hashlib
import json
import math
from pathlib import Path
import random
import re
import sys

source, destination = map(Path, sys.argv[1:3])
raw = source.read_bytes()
data = json.loads(raw)
rng = random.Random(20260921)
paragraphs = []
for doc in data['data']:
    for p in doc['paragraphs']:
        if p['qas'] and 200 <= len(p['context']) <= 1600:
            paragraphs.append({'id': doc.get('id', p['qas'][0]['id'].split('_QUERY')[0]),
                               'title': doc.get('title', ''), **p})
# Deduplicate source texts before selecting groups.
paragraphs = list({p['context']: p for p in paragraphs}.values())

def terms(text):
    text = re.sub(r'\s+', '', text.lower())
    return [text[i:i+2] for i in range(len(text)-1)]

tfs = [collections.Counter(terms(p['context'])) for p in paragraphs]
df = collections.Counter(t for tf in tfs for t in tf)
lengths = [sum(tf.values()) for tf in tfs]
avg = sum(lengths) / len(lengths)

def bm25(query):
    qt = set(terms(query))
    result = []
    for i, tf in enumerate(tfs):
        score = sum(math.log(1+(len(tfs)-df[t]+.5)/(df[t]+.5)) * tf[t]*2.2 /
                    (tf[t]+1.2*(.25+.75*lengths[i]/avg)) for t in qt if tf[t])
        result.append((score, i))
    return sorted(result, reverse=True)

indices = list(range(len(paragraphs)))
rng.shuffle(indices)
selected = indices[:60]
calibration_ids = set(selected[:10])
test_ids = set(selected[10:])
cases = []
for n, idx in enumerate(selected):
    p = paragraphs[idx]
    q = p['qas'][0]
    answers = list(dict.fromkeys(a['text'] for a in q['answers'] if a['text']))
    assert answers
    no_answer = n >= 50
    excluded = test_ids if n < 10 else calibration_ids
    ranked = [(score, i) for score, i in bm25(q['question']) if i != idx and i not in excluded]
    if no_answer:
        ranked = [(score, i) for score, i in ranked if not any(a in paragraphs[i]['context'] for a in answers)]
    chosen = ranked[:20 if no_answer else 19]
    gold_rank = None
    if not no_answer:
        gold_rank = rng.randrange(5, 20)
        chosen.insert(gold_rank, (0.0, idx))
    candidates = []
    for rank, (_, i) in enumerate(chosen):
        text = paragraphs[i]['context']
        cid = 'cmrc-' + hashlib.sha256(text.encode()).hexdigest()[:16]
        candidates.append({'id': cid, 'searchText': text,
                           'hit': {'kbId': 'eval-cmrc', 'kbName': 'CMRC2018', 'docId': cid,
                                   'docName': paragraphs[i]['title'], 'content': text,
                                   'chunkIndex': 0, 'score': 1/(rank+61), 'recallScore': 1/(rank+61)}})
    gold_id = 'cmrc-' + hashlib.sha256(p['context'].encode()).hexdigest()[:16]
    cases.append({'id': 'zh-'+q['id'], 'language': 'zh', 'split': 'calibration' if n < 10 else 'test',
                  'query': q['question'], 'reference': '提供的资料不足以回答此问题。' if no_answer else answers[0],
                  'acceptableAnswers': [] if no_answer else answers,
                  'referenceContexts': [] if no_answer else [p['context']],
                  'referenceDocumentIds': [] if no_answer else [gold_id],
                  'relevantIds': [] if no_answer else [gold_id], 'group': gold_id,
                  'expectedAbstention': no_answer, 'candidates': candidates,
                  'construction': {'kind': 'gold-removed' if no_answer else 'gold-injected', 'goldRank': gold_rank}})

result = {'version': 1, 'generation': 'CMRC2018-component-diagnostic',
          'source': {'url': 'https://github.com/ymcui/cmrc2018', 'revision': 'c0eb1b6ba219847457e6af3180da722bbeb656af',
                     'sha256': hashlib.sha256(raw).hexdigest(), 'seed': 20260921}, 'cases': cases}
destination.write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding='utf-8')
print(json.dumps({'paragraphs': len(paragraphs), 'cases': len(cases), 'test': 50, 'calibration': 10,
                  'outputSHA256': hashlib.sha256(destination.read_bytes()).hexdigest()}))
