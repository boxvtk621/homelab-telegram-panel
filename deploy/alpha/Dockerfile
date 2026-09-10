# Build context is the allowlisted bundle produced by scripts/harness-alpha/package.py.
FROM ghcr.io/boxvtk621/homelab-telegram-panel@sha256:78210dfb0dad831047f99ebc78d2266e9790b8485453db3e90269ec254b9b481 AS panel
ARG VERSION
ARG SOURCE_BASE
ARG BUNDLE_SHA256
LABEL org.opencontainers.image.version="$VERSION" org.opencontainers.image.revision="" \
    ru.h1-cloud.alpha="cursor-chat" ru.h1-cloud.alpha.source-base="$SOURCE_BASE" ru.h1-cloud.alpha.bundle-sha256="$BUNDLE_SHA256"
COPY panel /fixik-next-mobile-gateway

FROM node:24.18.0-bookworm-slim@sha256:6f7b03f7c2c8e2e784dcf9295400527b9b1270fd37b7e9a7285cf83b6951452d AS harness
WORKDIR /opt/worker
COPY package.json package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund
COPY worker.mjs ./
COPY harness-node /harness-node
COPY --from=panel /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ARG VERSION
ARG SOURCE_BASE
ARG BUNDLE_SHA256
LABEL org.opencontainers.image.version="$VERSION" org.opencontainers.image.revision="" \
    ru.h1-cloud.alpha="cursor-chat" ru.h1-cloud.alpha.source-base="$SOURCE_BASE" ru.h1-cloud.alpha.bundle-sha256="$BUNDLE_SHA256"
ENV HOME=/state/cursor/home
USER 10001:10001
ENTRYPOINT ["/harness-node"]
CMD ["--config", "/config/node.json"]
