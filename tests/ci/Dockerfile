# The toolchain image every gate runs in (scripts/in-container.sh): the Go
# toolchain go.mod pins (ci-host fails when they differ), the tools `just ci`
# calls, and the Claude Code CLI for `claude plugin validate` and the live
# harness check. Base images are pinned by digest and the CLIs by version; the
# Debian packages are those of the trixie release the Go image ships. Changing
# this file changes the image tag, so the next run rebuilds it. Caches live in
# a volume mounted at /cache, world-writable with the sticky bit so the
# non-root uid the runner uses can fill it.
FROM node:22.23.2-trixie-slim@sha256:c5849ff9c9ebcd66615412f0b548ca5b8ecaef84003dc9ac2e077ebe46aaa3f6 AS node
FROM ghcr.io/astral-sh/uv:0.12.5@sha256:e85be844203885286c60ffad8a858d48afb6c5a5c237ca0e67f12e74b8f174b1 AS uv
FROM koalaman/shellcheck:v0.11.0@sha256:61862eba1fcf09a484ebcc6feea46f1782532571a34ed51fedf90dd25f925a8d AS shellcheck

FROM golang:1.27.1-trixie@sha256:8f58fd67ea075142d947a60e0caa4317746a55118d312f027793d382c7741734
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
COPY --from=uv /uv /uvx /usr/local/bin/
COPY --from=shellcheck /bin/shellcheck /usr/local/bin/shellcheck
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
 && apt-get update \
 && apt-get install -y --no-install-recommends just jq python3 procps zip unzip xz-utils file \
 && rm -rf /var/lib/apt/lists/*
ARG CLAUDE_CODE_VERSION=2.1.292
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
