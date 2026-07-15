# syntax=docker/dockerfile:1
#
# Multi-target build for the two deployable images (hard cutover, design §8.1):
#   docker build --target panel -t natives3-panel .
#   docker build --target node  -t natives3-node  .
#
# The legacy single-binary image (cmd/natives3bridge) is no longer a supported
# deployment target; the panel + node pair replaces it. The panel embeds the
# WebAdmin SPA (built in the web stage); the node has no management UI and skips
# that stage entirely.

FROM --platform=$BUILDPLATFORM node:18-alpine AS web
ARG APP_VERSION
WORKDIR /src/pkg/webadmin/ui
COPY pkg/webadmin/ui/package*.json ./
RUN npm ci
COPY pkg/webadmin/ui/ ./
RUN APP_VERSION="${APP_VERSION}" npm run build

FROM --platform=$BUILDPLATFORM golang:1.21-alpine AS go-base
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG APP_VERSION=dev

FROM go-base AS panel-build
COPY --from=web /src/pkg/webadmin/ui/dist ./pkg/webadmin/ui/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build -trimpath -ldflags="-s -w -X main.version=${APP_VERSION}" -o /out/panel ./cmd/panel

FROM go-base AS node-build
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build -trimpath -ldflags="-s -w -X main.version=${APP_VERSION}" -o /out/node ./cmd/node

# --- panel image: management UI/REST (9001) + node control-plane listener (9443).
#     No S3 data plane; object traffic never transits the panel (design §1.3). ---
FROM alpine:3.20 AS panel
RUN apk add --no-cache ca-certificates \
  && addgroup -S -g 10001 natives3 \
  && adduser -S -D -H -u 10001 -G natives3 natives3
WORKDIR /app
COPY --from=panel-build /out/panel /usr/local/bin/panel
COPY configs/panel.example.yaml /app/configs/
RUN mkdir -p /app/configs /data \
  && chown -R natives3:natives3 /app/configs /data
USER natives3
EXPOSE 9001 9443
VOLUME ["/data"]
ENTRYPOINT ["panel"]
CMD ["-config", "/app/configs/panel.yaml"]

# --- node image: S3 data plane only (9000). No admin/management port whatsoever
#     (design §1.3). Dials the panel over mTLS; the panel never dials back. ---
FROM alpine:3.20 AS node
RUN apk add --no-cache ca-certificates \
  && addgroup -S -g 10001 natives3 \
  && adduser -S -D -H -u 10001 -G natives3 natives3
WORKDIR /app
COPY --from=node-build /out/node /usr/local/bin/node
COPY configs/node.example.yaml /app/configs/
RUN mkdir -p /app/configs /data \
  && chown -R natives3:natives3 /app/configs /data
USER natives3
EXPOSE 9000
VOLUME ["/data"]
ENTRYPOINT ["node"]
CMD ["-config", "/app/configs/node.yaml"]
