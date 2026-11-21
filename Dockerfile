# syntax=docker/dockerfile:1@sha256:572cebbc2b15e6f84c077742952674889cb63cd5e9b80abdffcbe587b6f19183
FROM cgr.dev/chainguard/python:latest-dev@sha256:c047e6d1e47b09144a771bfde6595a00c49dc8571caa7dd62df88634e84304ae AS python-build
USER root

FROM python-build AS model
COPY scripts/download_model.py /tmp/download_model.py
RUN python /tmp/download_model.py

FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie@sha256:f18c3d73a184fb0582828510722e723b63a93f7188d11dd223d9be6f6880e65d AS api-build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/parakeet-api ./cmd/parakeet-api \
    && mkdir /out/data

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931 AS api
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

FROM python-build AS ffmpeg-build
ARG FFMPEG_VERSION=9.0.2
RUN apk add --no-cache zlib-dev
# Mirror the upstream release archive; the checksum pins the original bytes.
ADD --checksum=sha256:8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e \
    https://distfiles.macports.org/ffmpeg/ffmpeg-${FFMPEG_VERSION}.tar.xz /tmp/ffmpeg.tar.xz
COPY scripts/build_ffmpeg.sh /tmp/build_ffmpeg.sh
RUN mkdir /tmp/ffmpeg && tar -xJf /tmp/ffmpeg.tar.xz --strip-components=1 -C /tmp/ffmpeg \
    && cd /tmp/ffmpeg && sh /tmp/build_ffmpeg.sh \
    && mkdir -p /out/share/doc/ffmpeg \
    && cp COPYING.LGPLv2.1 /out/share/doc/ffmpeg/ \
    && cp /tmp/ffmpeg.tar.xz /out/share/doc/ffmpeg/source.tar.xz \
    && cp /tmp/build_ffmpeg.sh /out/share/doc/ffmpeg/build.sh

FROM python-build AS worker-deps
COPY --from=ghcr.io/astral-sh/uv:0.13.9@sha256:d3b601f0c7b118bc1753febf7c41cc80981df678445e0254284eecf84db79a7d /uv /usr/local/bin/uv
ENV UV_PYTHON_DOWNLOADS=never UV_LINK_MODE=copy
WORKDIR /app
COPY pyproject.toml uv.lock ./
RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --locked --no-dev --no-install-project --no-build --extra worker --python /usr/bin/python3.15

FROM python-build AS worker-rootfs
ARG PYTHON_VERSION=3.15.1_git20261024-r0
# Install only runtime packages into an empty root. Keep the APK database for scanners.
RUN apk --root /runtime --initdb --no-scripts --keys-dir /etc/apk/keys \
    --repositories-file /etc/apk/repositories add --no-cache \
    python-3.15=${PYTHON_VERSION} libstdc++ libgomp ca-certificates-bundle wolfi-baselayout \
    && mkdir -p /runtime/scratch \
    && chown 10001:10001 /runtime/scratch

FROM scratch AS worker
LABEL org.opencontainers.image.source="https://github.com/adapt2move/parakeet-api" \
      org.opencontainers.image.licenses="EUPL-1.2" \
      org.opencontainers.image.vendor="Adapt2Move GmbH"
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 PATH=/app/.venv/bin:/usr/bin \
    OMP_NUM_THREADS=1 OPENBLAS_NUM_THREADS=1
WORKDIR /app
COPY --from=worker-rootfs /runtime/ /
COPY --from=ffmpeg-build /out/usr/bin/ffmpeg /usr/bin/ffmpeg
COPY --from=ffmpeg-build /out/share/doc/ffmpeg /usr/share/doc/ffmpeg
COPY --from=worker-deps /app/.venv /app/.venv
COPY --from=model /models /models
COPY parakeet_api ./parakeet_api
COPY LICENSE NOTICE ./
USER 10001:10001
EXPOSE 8765
HEALTHCHECK --interval=15s --timeout=3s --start-period=120s CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://localhost:8765/health',timeout=2)"]
CMD ["python", "-m", "uvicorn", "parakeet_api.worker:app", "--host", "0.0.0.0", "--port", "8765", "--no-access-log", "--timeout-graceful-shutdown", "30"]
