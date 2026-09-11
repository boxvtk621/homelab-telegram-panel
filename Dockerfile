FROM --platform=$BUILDPLATFORM node:24.18.0-alpine@sha256:a0b9bf06e4e6193cf7a0f58816cc935ff8c2a908f81e6f1a95432d679c54fbfd AS web
WORKDIR /src/web/mobile-workspace
COPY web/mobile-workspace/package.json web/mobile-workspace/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/mobile-workspace/ ./
COPY api/ /src/api/
RUN npm run lint && npm run typecheck && npm test && npm run build
COPY internal/mobilegatewayassets/dist/ /expected-dist/
RUN diff -r /expected-dist /src/internal/mobilegatewayassets/dist

FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
WORKDIR /src
COPY go.mod go.work ./
COPY harness/ harness/
COPY cmd/ cmd/
COPY internal/ internal/
COPY api/ api/
COPY compose.yaml ./
COPY web/mobile-workspace/src/ web/mobile-workspace/src/
COPY --from=web /src/internal/mobilegatewayassets/dist/ internal/mobilegatewayassets/dist/
RUN go vet ./... && CGO_ENABLED=0 go test ./...
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
    -ldflags="-buildid= -s -w -X github.com/boxvtk621/homelab-telegram-panel/internal/buildinfo.Version=$VERSION" \
    -o /out/fixik-next-mobile-gateway ./cmd/fixik-next-mobile-gateway

FROM python:3.13.15-slim-bookworm@sha256:ed86c82274b3c69b52fb5820f358f0bd7df0b603332063cb5c6e32bd220c3e6e AS runtime
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="HomeLab Telegram Panel" \
      org.opencontainers.image.source="https://github.com/boxvtk621/homelab-telegram-panel" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$REVISION"
COPY --from=build /out/fixik-next-mobile-gateway /fixik-next-mobile-gateway
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 10001:10001
ENTRYPOINT ["/fixik-next-mobile-gateway"]
CMD ["serve"]
