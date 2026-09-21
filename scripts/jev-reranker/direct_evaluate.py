#!/usr/bin/env python3
"""Use the existing Ragas engine with an isolated official DeepSeek connection.

Run inside the evaluator image, not on the host. The application's judge proxy
does not preserve forced tool_choice. Direct evaluation preserves it and uses
non-thinking mode for structured judging. Production services are unchanged.
"""
import argparse
import asyncio
import hashlib
from importlib.metadata import version
import json
import os
from pathlib import Path
import time

import openai


class StructuredJudgeClient(openai.AsyncOpenAI):
    def __init__(self, *args, **kwargs):
        kwargs['max_retries'] = 0
        super().__init__(*args, **kwargs)
        if str(self.base_url).rstrip('/') != 'https://api.deepseek.com/v1':
            return  # The embedding client remains local and unchanged.
        create = self.chat.completions.create

        async def structured_create(*args, **kwargs):
            extra = dict(kwargs.get('extra_body') or {})
            extra['thinking'] = {'type': 'disabled'}
            kwargs['extra_body'] = extra
            headers = dict(kwargs.get('extra_headers') or {})
            headers.pop('X-BkCrab-Eval-Owner', None)
            kwargs['extra_headers'] = headers
            return await create(*args, **kwargs)

        self.chat.completions.create = structured_create


openai.AsyncOpenAI = StructuredJudgeClient
from app.metrics import build_ragas_engine, judge_owner_scope
from app.protocol import CaseResult, EvaluateResponse, Sample
from app.settings import Settings
from app.usage import usage_scope


def read_rows(path):
    return [json.loads(line) for line in path.read_text(encoding='utf-8').splitlines()] if path.exists() else []


async def main():
    p = argparse.ArgumentParser()
    p.add_argument('input', type=Path)
    p.add_argument('answers', type=Path)
    p.add_argument('output', type=Path)
    p.add_argument('--limit', type=int, default=0)
    p.add_argument('--token-budget', type=int, default=4000000)
    a = p.parse_args()
    settings = Settings.from_env()
    assert settings.llm_endpoint.rstrip('/') == 'https://api.deepseek.com/v1'
    assert settings.llm_model == 'deepseek-v4-flash'
    engine = build_ragas_engine(settings)
    cases = {s['id']:s for s in json.loads(a.input.read_text(encoding='utf-8'))['cases']}
    prior = read_rows(a.output)
    done = {r['requestId'] for r in prior}
    total = sum(r.get('tokens',0) for r in prior)
    metrics = ['context_precision','context_recall','factual_correctness','faithfulness','response_relevancy']
    completed = 0
    started = time.monotonic()
    with a.output.open('a',encoding='utf-8') as f:
        for answer in read_rows(a.answers):
            if answer['status'] != 'ok':
                continue
            case = cases[answer['caseId']]
            if case['expectedAbstention']:
                continue
            candidates = {c['id']:c for c in case['candidates']}
            sample = Sample(caseId=case['id']+':'+answer['arm'],userInput=case['query'],
                response=answer['answer']['response'],reference=case['reference'],
                referenceContexts=case['referenceContexts'],retrievedContextIds=answer['contextIds'],
                retrievedContexts=[candidates[i]['hit']['content'] for i in answer['contextIds']])
            contract={'sample':sample.model_dump(),'metrics':metrics,'judge':'deepseek-v4-flash',
                      'endpoint':settings.llm_endpoint,'thinking':'disabled','maxTokens':settings.llm_max_tokens,
                      'ragas':version('ragas'),'transport':'official-direct-v1'}
            request_id='jev-direct:'+hashlib.sha256(json.dumps(contract,sort_keys=True,ensure_ascii=False).encode()).hexdigest()[:48]
            if request_id in done:
                continue
            if total >= a.token_budget or time.monotonic()-started>5*3600:
                raise RuntimeError('scoring budget reached')
            begin=time.monotonic()
            with judge_owner_scope('public-jevrerank-experiment'),usage_scope() as meter:
                values=await asyncio.gather(*(engine.evaluate(m,sample) for m in metrics))
                usage=meter.response(settings)
            result=EvaluateResponse(requestId=request_id,ragasVersion=version('ragas'),
                results=[CaseResult(caseId=sample.caseId,metrics=dict(zip(metrics,values)))],usage=usage)
            tokens=usage.llmInputTokens+usage.llmOutputTokens+usage.embeddingInputTokens
            total+=tokens
            row={'caseId':case['id'],'arm':answer['arm'],'requestId':request_id,'status':'ok',
                 'tokens':tokens,'durationMs':round((time.monotonic()-begin)*1000),'response':result.model_dump(),
                 'judgeContract':{k:v for k,v in contract.items() if k not in ['sample','metrics']}}
            f.write(json.dumps(row,ensure_ascii=False)+'\n');f.flush();os.fsync(f.fileno())
            print(json.dumps({'caseId':case['id'],'arm':answer['arm'],'tokens':tokens,
                  'durationMs':row['durationMs'],'statuses':{m:v.status for m,v in zip(metrics,values)}}),flush=True)
            completed+=1
            if a.limit and completed>=a.limit:
                break


if __name__=='__main__':
    asyncio.run(main())
