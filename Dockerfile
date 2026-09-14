# syntax=docker/dockerfile:1
FROM python:3.11-slim-bookworm@sha256:528257d48c1da0dcecc2e725d1ae34498d60c965f1241e39cd6a85a8859bdf84 AS model
COPY scripts/download_model.py /tmp/download_model.py
RUN python /tmp/download_model.py

FROM --platform=$BUILDPLATFORM golang:1.27@sha256:f44f6e88636cfb311f9ebace870ded69d943f227bb3cb27d32ffd84ea18c43ea AS api-build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/parakeet-api ./cmd/parakeet-api \
    && mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS api
LABEL org.opencontainers.image.source="https://github.com/adapt2move/parakeet-api" \
      org.opencontainers.image.licenses="EUPL-1.2" \
      org.opencontainers.image.vendor="Adapt2Move GmbH"
COPY --from=api-build --chown=10001:10001 /out/data /data
COPY --from=api-build /out/parakeet-api /parakeet-api
COPY LICENSE NOTICE /
USER 10001:10001
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s CMD ["/parakeet-api", "healthcheck"]
ENTRYPOINT ["/parakeet-api"]

FROM python:3.11-slim-bookworm@sha256:528257d48c1da0dcecc2e725d1ae34498d60c965f1241e39cd6a85a8859bdf84 AS worker
LABEL org.opencontainers.image.source="https://github.com/adapt2move/parakeet-api" \
      org.opencontainers.image.licenses="EUPL-1.2" \
      org.opencontainers.image.vendor="Adapt2Move GmbH"
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 PATH=/app/.venv/bin:$PATH \
    OMP_NUM_THREADS=1 OPENBLAS_NUM_THREADS=1
WORKDIR /app
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir uv==0.8.22
COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-dev --no-install-project --extra worker
RUN mkdir /scratch && chown 10001:10001 /scratch
COPY --from=model /models /models
COPY parakeet_api ./parakeet_api
COPY LICENSE NOTICE ./
USER 10001:10001
EXPOSE 8765
HEALTHCHECK --interval=15s --timeout=3s --start-period=120s CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://localhost:8765/health',timeout=2)"]
CMD ["uvicorn", "parakeet_api.worker:app", "--host", "0.0.0.0", "--port", "8765", "--no-access-log", "--timeout-graceful-shutdown", "30"]
