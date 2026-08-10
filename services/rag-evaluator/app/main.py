from __future__ import annotations

import asyncio
import hashlib
import hmac
import threading
from collections import OrderedDict
from concurrent.futures import Future
from importlib.metadata import version

from fastapi import Depends, FastAPI, Header, HTTPException, Request
from fastapi.responses import JSONResponse

from .metrics import MetricEngine, build_ragas_engine
from .protocol import (
    METRIC_BUNDLE_VERSION,
    PROTOCOL_VERSION,
    CaseResult,
    EvaluateRequest,
    EvaluateResponse,
)
from .settings import Settings


class IdempotencyCache:
    def __init__(self, max_entries: int) -> None:
        self._max_entries = max(1, max_entries)
        self._lock = threading.Lock()
        self._completed: OrderedDict[str, tuple[str, EvaluateResponse]] = OrderedDict()
        self._inflight: dict[str, tuple[str, Future[EvaluateResponse]]] = {}

    def claim(
        self, request_id: str, body_hash: str
    ) -> tuple[bool, EvaluateResponse | Future[EvaluateResponse] | None]:
        with self._lock:
            if completed := self._completed.get(request_id):
                if completed[0] != body_hash:
                    raise ValueError("requestId body mismatch")
                self._completed.move_to_end(request_id)
                return False, completed[1]
            if inflight := self._inflight.get(request_id):
                if inflight[0] != body_hash:
                    raise ValueError("requestId body mismatch")
                return False, inflight[1]
            future: Future[EvaluateResponse] = Future()
            self._inflight[request_id] = (body_hash, future)
            return True, future

    def finish(self, request_id: str, body_hash: str, response: EvaluateResponse) -> None:
        with self._lock:
            _, future = self._inflight.pop(request_id)
            self._completed[request_id] = (body_hash, response)
            self._completed.move_to_end(request_id)
            while len(self._completed) > self._max_entries:
                self._completed.popitem(last=False)
            future.set_result(response)

    def fail(self, request_id: str, error: BaseException) -> None:
        with self._lock:
            inflight = self._inflight.pop(request_id, None)
            if inflight and not inflight[1].done():
                inflight[1].set_exception(error)


def create_app(settings: Settings, engine: MetricEngine | None = None) -> FastAPI:
    metric_engine = engine or build_ragas_engine(settings)
    cache = IdempotencyCache(settings.idempotency_cache_entries)
    app = FastAPI(title="bkcrab-rag-evaluator", docs_url=None, redoc_url=None)

    def authorize(authorization: str = Header(default="")) -> None:
        if not settings.api_key:
            return
        expected = f"Bearer {settings.api_key}"
        if not hmac.compare_digest(authorization, expected):
            raise HTTPException(status_code=401, detail="unauthorized")

    @app.middleware("http")
    async def body_limit(request: Request, call_next):
        body = await request.body()
        if len(body) > settings.max_request_bytes:
            return JSONResponse(status_code=413, content={"detail": "request too large"})
        return await call_next(request)

    @app.get("/healthz")
    async def healthz() -> dict[str, object]:
        return {
            "ok": True,
            "serviceVersion": settings.service_version,
            "protocolVersion": PROTOCOL_VERSION,
            "ragasVersion": version("ragas"),
            "metricBundleVersion": METRIC_BUNDLE_VERSION,
            "judgeConfigured": settings.judge_configured,
        }

    @app.post("/v1/evaluate", response_model=EvaluateResponse, dependencies=[Depends(authorize)])
    async def evaluate(payload: EvaluateRequest) -> EvaluateResponse:
        total_context_bytes = sum(
            len(context.encode("utf-8"))
            for sample in payload.samples
            for context in (*sample.retrievedContexts, *sample.referenceContexts)
        )
        if total_context_bytes > settings.max_total_context_bytes:
            raise HTTPException(status_code=422, detail="total context bytes exceeded")
        canonical = payload.model_dump_json(exclude_none=False)
        body_hash = hashlib.sha256(canonical.encode()).hexdigest()
        try:
            owner, cached = cache.claim(payload.requestId, body_hash)
        except ValueError as exc:
            raise HTTPException(status_code=409, detail=str(exc)) from exc
        if not owner:
            if isinstance(cached, EvaluateResponse):
                return cached
            assert isinstance(cached, Future)
            return await asyncio.wrap_future(cached)

        try:
            results: list[CaseResult] = []
            for sample in payload.samples:
                scores = {
                    metric: await metric_engine.evaluate(metric, sample)
                    for metric in payload.metrics
                }
                results.append(CaseResult(caseId=sample.caseId, metrics=scores))
            response = EvaluateResponse(
                requestId=payload.requestId,
                ragasVersion=version("ragas"),
                results=results,
            )
            cache.finish(payload.requestId, body_hash, response)
            return response
        except BaseException as exc:
            cache.fail(payload.requestId, exc)
            raise

    return app


settings = Settings.from_env()
app = create_app(settings)


def main() -> None:
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=settings.port, access_log=False)
