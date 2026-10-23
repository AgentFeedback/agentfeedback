# The toolchain image every gate runs in (scripts/in-container.sh): the Go
# toolchain go.mod pins (ci-host fails when they differ), the tools `just ci`
# calls, and the Claude Code CLI for `claude plugin validate` and the live
# harness check. Base images are pinned by digest and the CLIs by version; the
# Debian packages are those of the trixie release the Go image ships. Changing
# this file changes the image tag, so the next run rebuilds it. Caches live in
# a volume mounted at /cache, world-writable with the sticky bit so the
# non-root uid the runner uses can fill it.
FROM node:24.22.1-trixie-slim@sha256:43b9ba1142b8dcf33fa8f922ca1be85714adc444e0586c7cf7726daaeabc1bea AS node
FROM ghcr.io/astral-sh/uv:0.13.5@sha256:32a7bb39b496babbd997b0a39a4d66c126bdaf5661b302bf6f06abda8deca6ff AS uv
FROM koalaman/shellcheck:v0.11.0@sha256:61862eba1fcf09a484ebcc6feea46f1782532571a34ed51fedf90dd25f925a8d AS shellcheck

FROM golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
COPY --from=uv /uv /uvx /usr/local/bin/
COPY --from=shellcheck /bin/shellcheck /usr/local/bin/shellcheck
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
 && apt-get update \
 && apt-get install -y --no-install-recommends just jq python3 procps zip unzip xz-utils file \
 && rm -rf /var/lib/apt/lists/*
ARG CLAUDE_CODE_VERSION=2.1.308
RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
 && npm cache clean --force \
 && mkdir -p /cache \
 && chmod 1777 /cache
ENV GOTOOLCHAIN=local \
    GOPATH=/cache/go \
    GOCACHE=/cache/go-build \
    npm_config_cache=/cache/npm \
    npm_config_update_notifier=false \
    UV_CACHE_DIR=/cache/uv \
    DISABLE_AUTOUPDATER=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
