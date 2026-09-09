FROM node:24.18.0-bookworm-slim@sha256:6f7b03f7c2c8e2e784dcf9295400527b9b1270fd37b7e9a7285cf83b6951452d AS node
FROM python:3.13.15-slim-bookworm@sha256:ed86c82274b3c69b52fb5820f358f0bd7df0b603332063cb5c6e32bd220c3e6e
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules/npm /opt/npm
WORKDIR /opt/probe
COPY package.json package-lock.json ./
RUN node /opt/npm/bin/npm-cli.js ci --ignore-scripts --no-audit --no-fund
COPY probe.py run.py preflight.mjs cursor_probe.mjs cursor_run.py cursor_catalog.mjs cursor_deny_probe.mjs cursor_deny_run.py ./
COPY codex_probe.py codex_run.py codex_catalog.py ./
COPY tools/ ./tools/
COPY tests/ ./tests/
RUN mkdir -p /state/home /state/workspace && chown -R 10001:10001 /state
ENV PATH="/opt/probe/node_modules/.bin:${PATH}" PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
LABEL ru.h1-cloud.hl240.purpose="sdk-feasibility-probe"
USER 10001:10001
WORKDIR /state/workspace
ENTRYPOINT ["python3", "/opt/probe/probe.py"]
