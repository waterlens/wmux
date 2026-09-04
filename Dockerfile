# syntax=docker/dockerfile:1.7

FROM node:24-bookworm-slim AS web
RUN corepack enable
WORKDIR /src
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile
COPY client ./client
COPY tsconfig.base.json tsconfig.client.json vite.config.ts ./
RUN pnpm build:web

FROM golang:1.26-bookworm AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/webui/dist ./internal/webui/dist
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/waterlens/wmux/internal/version.Version=${VERSION} -X github.com/waterlens/wmux/internal/version.Commit=${COMMIT}" \
    -o /out/wmux ./cmd/wmux

FROM debian:bookworm-slim
RUN apt-get update \
	&& apt-get install -y --no-install-recommends bash ca-certificates curl tmux tini \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 --shell /bin/bash wmux \
    && install -d -o wmux -g wmux /data
COPY --from=server /out/wmux /usr/local/bin/wmux
USER wmux
ENV WMUX_HOST=0.0.0.0 WMUX_PORT=8787 WMUX_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8787
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD curl --fail --silent --show-error http://127.0.0.1:8787/api/health >/dev/null || exit 1
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/wmux"]
