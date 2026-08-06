FROM ghcr.io/astral-sh/uv:0.11.29 AS uv
FROM python:3.12.13-slim-bookworm

COPY --from=uv /uv /uvx /usr/local/bin/
ENV PATH="/opt/rag-evaluator/.venv/bin:$PATH" PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 \
    HOME=/tmp TMPDIR=/tmp XDG_CACHE_HOME=/tmp/cache UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy
WORKDIR /opt/rag-evaluator
RUN groupadd --gid 65532 rag-evaluator && useradd --uid 65532 --gid 65532 --no-create-home --shell /usr/sbin/nologin rag-evaluator
COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-install-project --no-editable
COPY app ./app
RUN uv sync --frozen --no-editable
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s --start-period=15s --retries=5 CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/healthz', timeout=2).read()"]
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8080", "--no-access-log"]
