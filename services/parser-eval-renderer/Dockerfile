FROM ghcr.io/astral-sh/uv:0.11.29@sha256:eb2843a1e56fd9e30c7276ce1a52cba86e64c7b385f5e3279a0e08e02dd058fc AS uv

FROM python:3.12.13-slim-bookworm@sha256:4766d8b510c428e595d74b9cc5bbb2fae8e26316fffb4adc89908d79aacd58a2

COPY --from=uv /uv /uvx /usr/local/bin/

ENV PATH="/opt/parser-eval-renderer/.venv/bin:$PATH" \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    HOME=/tmp \
    TMPDIR=/tmp \
    XDG_CACHE_HOME=/tmp/cache \
    UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy \
    PARSER_EVAL_RENDERER_TEMP_ROOT=/tmp

WORKDIR /opt/parser-eval-renderer

# Freeze LibreOffice and font packages to one Debian archive view so rebuilds
# cannot silently change page geometry or pagination.
RUN rm -f /etc/apt/sources.list.d/debian.sources \
    && printf '%s\n' \
      'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/20260801T000000Z bookworm main' \
      'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/20260801T000000Z bookworm-updates main' \
      'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian-security/20260801T000000Z bookworm-security main' \
      > /etc/apt/sources.list \
    && apt-get -o Acquire::Check-Valid-Until=false update \
    && apt-get install -y --no-install-recommends \
      fonts-dejavu-core \
      fonts-liberation \
      libreoffice-calc-nogui \
      libreoffice-impress-nogui \
      libreoffice-writer-nogui \
    && soffice --version \
    && rm -rf /var/lib/apt/lists/*

RUN groupadd --gid 65532 parser-renderer \
    && useradd --uid 65532 --gid 65532 --no-create-home --shell /usr/sbin/nologin parser-renderer

COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-install-project --no-editable

COPY app ./app
RUN uv sync --frozen --no-editable

USER 65532:65532

EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=3s --start-period=15s --retries=3 \
    CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/healthz', timeout=2).read()"]

CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8080", "--no-access-log"]
