# syntax=docker/dockerfile:1.7

ARG NODE_VERSION=22.19.0
ARG PNPM_VERSION=11.0.9
ARG GO_VERSION=1.25.1
ARG ALPINE_VERSION=3.22

FROM node:${NODE_VERSION}-alpine AS web-builder

WORKDIR /src/web
ARG PNPM_VERSION
RUN apk add --no-cache zip \
    && npm install --global "pnpm@${PNPM_VERSION}"

COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN --mount=type=cache,id=relayops-pnpm,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile

COPY web/ ./
COPY browser-extension/ /src/browser-extension/
RUN mkdir -p public/downloads \
    && cd /src \
    && zip -q -r web/public/downloads/relayops-login-helper.zip browser-extension \
    && cd /src/web \
    && pnpm run build


FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS go-builder

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,id=relayops-gomod,target=/go/pkg/mod \
    go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

ARG TARGETOS=linux
ARG TARGETARCH
RUN --mount=type=cache,id=relayops-gomod,target=/go/pkg/mod \
    --mount=type=cache,id=relayops-gobuild,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/relayops ./cmd/relayops


FROM alpine:${ALPINE_VERSION} AS runtime

RUN apk add --no-cache ca-certificates curl tzdata \
    && addgroup -S -g 10001 relayops \
    && adduser -S -D -H -u 10001 -G relayops relayops \
    && mkdir -p /app/web/dist /var/lib/relayops \
    && chown -R relayops:relayops /app /var/lib/relayops

WORKDIR /app

COPY --from=go-builder --chown=relayops:relayops /out/relayops /usr/local/bin/relayops
COPY --from=web-builder --chown=relayops:relayops /src/web/dist/ /app/web/dist/

ENV RELAYOPS_LISTEN=0.0.0.0:18180 \
    RELAYOPS_DATA_DIR=/var/lib/relayops \
    RELAYOPS_STATIC_DIR=/app/web/dist \
    RELAYOPS_DEV_MODE=false

USER 10001:10001

EXPOSE 18180
VOLUME ["/var/lib/relayops"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["curl", "-fsS", "--max-time", "3", "http://127.0.0.1:18180/health"]

ENTRYPOINT ["/usr/local/bin/relayops"]
