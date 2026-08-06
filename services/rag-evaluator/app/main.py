from __future__ import annotations

import hashlib
import hmac
from importlib.metadata import version

from fastapi import Depends, FastAPI, Header, HTTPException, Request

from .metrics import build_ragas_engine
from .protocol import METRIC_BUNDLE_VERSION, CaseResult, EvaluateRequest, EvaluateResponse
from .settings import Settings

settings = Settings.from_env()
engine = build_ragas_engine(settings)
app = FastAPI(title="bkcrab-rag-evaluator", docs_url=None, redoc_url=None)
_cache: dict[str, tuple[str, EvaluateResponse]] = {}


def authorize(authorization: str = Header(default="")) -> None:
    if not settings.api_key:
        return
    expected = f"Bearer {settings.api_key}"
    if not hmac.compare_digest(authorization, expected):
        raise HTTPException(status_code=401, detail="unauthorized")


@app.middleware("http")
async def body_limit(request: Request, call_next):
    content_length = request.headers.get("content-length")
    if content_length and int(content_length) > settings.max_request_bytes:
        raise HTTPException(status_code=413, detail="request too large")
    return await call_next(request)


@app.get("/healthz")
async def healthz() -> dict[str, object]:
    return {
        "ok": True,
        "serviceVersion": settings.service_version,
        "ragasVersion": version("ragas"),
        "metricBundleVersion": METRIC_BUNDLE_VERSION,
        "judgeConfigured": settings.judge_configured,
    }


@app.post("/v1/evaluate", response_model=EvaluateResponse, dependencies=[Depends(authorize)])
async def evaluate(payload: EvaluateRequest) -> EvaluateResponse:
    canonical = payload.model_dump_json(exclude_none=False)
    body_hash = hashlib.sha256(canonical.encode()).hexdigest()
    cached = _cache.get(payload.requestId)
    if cached:
        if cached[0] != body_hash:
            raise HTTPException(status_code=409, detail="requestId body mismatch")
        return cached[1]
    results: list[CaseResult] = []
    for sample in payload.samples:
        scores = {metric: await engine.evaluate(metric, sample) for metric in payload.metrics}
        results.append(CaseResult(caseId=sample.caseId, metrics=scores))
    response = EvaluateResponse(
        requestId=payload.requestId,
        ragasVersion=version("ragas"),
        results=results,
    )
    _cache[payload.requestId] = response
    return response


def main() -> None:
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=settings.port, access_log=False)
