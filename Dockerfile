# syntax=docker/dockerfile:1
FROM python:3.11-slim-bookworm@sha256:528257d48c1da0dcecc2e725d1ae34498d60c965f1241e39cd6a85a8859bdf84 AS model
COPY scripts/download_model.py /tmp/download_model.py
RUN python /tmp/download_model.py

FROM python:3.11-slim-bookworm@sha256:528257d48c1da0dcecc2e725d1ae34498d60c965f1241e39cd6a85a8859bdf84 AS base
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 PATH=/app/.venv/bin:$PATH
WORKDIR /app
RUN pip install --no-cache-dir uv==0.8.22
COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-dev --no-install-project
LABEL org.opencontainers.image.source="https://github.com/adapt2move/parakeet-api" \
      org.opencontainers.image.licenses="EUPL-1.2"

FROM base AS api
RUN mkdir /data && chown 10001:10001 /data
COPY parakeet_api ./parakeet_api
COPY LICENSE NOTICE ./
USER 10001:10001
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://localhost:8080/health/ready',timeout=2)"]
CMD ["uvicorn", "parakeet_api.api:app", "--host", "0.0.0.0", "--port", "8080", "--no-access-log", "--no-proxy-headers", "--timeout-graceful-shutdown", "30"]

FROM base AS worker
ENV OMP_NUM_THREADS=1 OPENBLAS_NUM_THREADS=1
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg && rm -rf /var/lib/apt/lists/*
RUN uv sync --frozen --no-dev --no-install-project --extra worker
RUN mkdir /scratch && chown 10001:10001 /scratch
COPY --from=model /models /models
COPY parakeet_api ./parakeet_api
COPY LICENSE NOTICE ./
USER 10001:10001
EXPOSE 8765
HEALTHCHECK --interval=15s --timeout=3s --start-period=120s CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://localhost:8765/health',timeout=2)"]
CMD ["uvicorn", "parakeet_api.worker:app", "--host", "0.0.0.0", "--port", "8765", "--no-access-log", "--timeout-graceful-shutdown", "30"]
