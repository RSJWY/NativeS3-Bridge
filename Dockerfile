# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM node:18-alpine AS web
WORKDIR /src/pkg/webadmin/ui
COPY pkg/webadmin/ui/package*.json ./
RUN npm ci
COPY pkg/webadmin/ui/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.21-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/pkg/webadmin/ui/dist ./pkg/webadmin/ui/dist
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build -trimpath -ldflags="-s -w" -o /out/natives3bridge ./cmd/natives3bridge

FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
  && addgroup -S -g 10001 natives3 \
  && adduser -S -D -H -u 10001 -G natives3 natives3
WORKDIR /app
COPY --from=build /out/natives3bridge /usr/local/bin/natives3bridge
COPY configs/config.example.yaml configs/config.docker.example.yaml /app/configs/
RUN mkdir -p /app/configs /data /state \
  && chown -R natives3:natives3 /app/configs /data /state
USER natives3
EXPOSE 9000 9001
VOLUME ["/data", "/state"]
ENTRYPOINT ["natives3bridge"]
CMD ["-config", "/app/configs/config.yaml"]
