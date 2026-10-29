# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
FROM cgr.dev/chainguard/python:latest-dev@sha256:01280a9ffd0c9c427d3127540203af6707ebf35cdb0481182aef303c03db239c AS python-build
USER root

FROM python-build AS model
COPY scripts/download_model.py /tmp/download_model.py
RUN python /tmp/download_model.py

FROM --platform=$BUILDPLATFORM golang:1.27.3-trixie@sha256:c65ecfe613b539ac74c62a4c3b2c27e8f8a6076384c8f27e8a3c0f3be17cc385 AS api-build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/parakeet-api ./cmd/parakeet-api \
    && mkdir /out/data

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99 AS api
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
COPY --from=ghcr.io/astral-sh/uv:0.13.6@sha256:af22a52f5eafef3d78e551599d405c02df61919d18028f0cea8eda93bc9986d0 /uv /usr/local/bin/uv
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
