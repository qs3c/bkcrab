FROM ghcr.io/astral-sh/uv:0.11.29 AS uv

FROM python:3.12.13-slim-bookworm

COPY --from=uv /uv /uvx /usr/local/bin/

ENV PATH="/opt/rag-parser/.venv/bin:$PATH" \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    RAG_PARSER_PORT=8080 \
    RAG_PARSER_TEMP_ROOT=/tmp/rag-parser \
    UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy

WORKDIR /opt/rag-parser

RUN groupadd --gid 65532 rag-parser \
    && useradd --uid 65532 --gid 65532 --no-create-home --shell /usr/sbin/nologin rag-parser \
    && install -d -m 0700 -o rag-parser -g rag-parser /tmp/rag-parser

COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-install-project --no-editable
RUN python -c "from importlib.metadata import distribution; from pathlib import PurePosixPath; dist = distribution('pypdfium2'); files = dist.files or (); assert any('BUILD_LICENSES' in PurePosixPath(str(item)).parts and dist.locate_file(item).is_file() for item in files), 'pypdfium2 wheel BUILD_LICENSES missing'"

COPY app ./app
RUN uv sync --frozen --no-editable

USER 65532:65532

EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=3 \
    CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/healthz', timeout=2).read()"]

CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8080", "--no-access-log"]
