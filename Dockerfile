# syntax=docker/dockerfile:1
# One Dockerfile, two sources for the binary, one runtime stage. By default the
# binary is compiled from source (the local Compose stack, `just docker-build`).
# The release step builds the image through GoReleaser (.goreleaser.yaml), which
# passes BINARY_FROM=prebuilt and a build context holding its own binaries at
# linux/<arch>/agentfeedback, so the published image carries the release
# archive's binary byte for byte. BuildKit builds only the stage selected.
ARG BINARY_FROM=source

# Build on the BuildKit builder platform and cross-compile the static binary for
# the target image platform. CGO stays off: the SQLite driver is pure Go.
FROM --platform=$BUILDPLATFORM golang:1.27.1-bookworm AS source

ARG TARGETOS
ARG TARGETARCH
# VERSION and COMMIT come from `just docker-build` (git describe without the
# leading v, and git rev-parse); `agentfeedback version` reports them.
ARG VERSION
ARG COMMIT

ENV CGO_ENABLED=0

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY ./ ./

RUN GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/agentfeedback ./cmd/agentfeedback

FROM scratch AS prebuilt
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/agentfeedback /out/agentfeedback

FROM ${BINARY_FROM} AS binary

# Distroless static: CA roots, timezone data, no shell and no package manager.
# Pinned by the digest of the multi-platform index.
FROM gcr.io/distroless/static-debian13@sha256:58133991db06659feaabe0f4e97a35cebf15ef4ea08f8a4c6d2ee5f75e4aa6a0

COPY --from=binary /out/agentfeedback /opt/agentfeedback

# The service runs as uid 10001, as every earlier image did, so an existing
# named volume stays writable. WORKDIR after USER creates /data owned by that
# uid, and a freshly created named volume inherits the ownership.
USER 10001:10001
WORKDIR /data
VOLUME /data
ENV DATABASE_PATH=/data/agentfeedback.db

EXPOSE 8080
ENTRYPOINT ["/opt/agentfeedback"]
CMD ["serve"]
