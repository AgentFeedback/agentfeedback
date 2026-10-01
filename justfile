# The repository does not vendor: every recipe ignores a stray vendor/
# (-mod=readonly, appended so a GOFLAGS from the environment is kept).
export GOFLAGS := trim(env("GOFLAGS", "") + " -mod=readonly")

# Build the service binary to bin/agentfeedback.
build:
    go build -o bin/agentfeedback ./cmd/agentfeedback

# Run all checks: fmt, vet, tidy, build. The pre-commit gate.
check: fmt vet tidy build

# Format every package.
fmt:
    go fmt ./...

# Run go vet.
vet:
    go vet ./...

# Run go mod tidy.
tidy:
    go mod tidy

staticcheck_version := "2026.2.1"

# Run staticcheck at the pinned version.
staticcheck:
    go run honnef.co/go/tools/cmd/staticcheck@{{staticcheck_version}} ./...

# Cross-compile the binary for every release platform into dist/<os>-<arch>/. Release artifacts are built by the release step; this proves the matrix.
build-all:
    #!/usr/bin/env bash
    set -euo pipefail
    for os in linux darwin windows; do
        for arch in amd64 arm64; do
            ext=""
            if [ "$os" = windows ]; then ext=".exe"; fi
            CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -o "dist/$os-$arch/agentfeedback$ext" ./cmd/agentfeedback
        done
    done

# Run the tests with race detection. No Docker, no database required.
test:
    go test -race -count=1 ./...

# Fuzz the decoder for a bounded time on top of its committed seed corpus.
fuzz:
    go test -run='^$' -fuzz='^FuzzDecode$' -fuzztime=30s ./pkg/envelope

# The checked-in skill renders: `just skills` writes them, `just ci` checks them.
skill_renders := "skills/agentfeedback/SKILL.md skills/agentfeedback-docs"

# Regenerate every checked-in skill render from internal/skillgen/source and the embedded reference files. Never edit a render by hand.
skills: build
    #!/usr/bin/env bash
    set -euo pipefail
    tmp=$(mktemp)
    tmpd=$(mktemp -d)
    trap 'rm -f "$tmp"; rm -rf "$tmpd"' EXIT
    ./bin/agentfeedback skill render skill-md > "$tmp"
    ./bin/agentfeedback skill render docs --out "$tmpd/out"
    cp "$tmp" skills/agentfeedback/SKILL.md
    rm -rf skills/agentfeedback-docs
    cp -R "$tmpd/out" skills/agentfeedback-docs

# Run locally against a database in ./local (created on demand).
run-local: build
    mkdir -p local
    DATABASE_PATH=${DATABASE_PATH:-local/agentfeedback.db} \
    HTTP_LISTEN_ADDR=${HTTP_LISTEN_ADDR:-127.0.0.1:8090} \
    API_KEY=${API_KEY:-local-dev-key} \
    ./bin/agentfeedback serve

# Validate the contract files: schemas, OpenAPI document, examples, conformance fixtures. Needs uv and npx.
contract:
    uv run --locked --script scripts/contract-check.py
    npx --yes @redocly/cli@2.54.2 lint docs/openapi.yaml

# Live HTTP contract suite against a fresh server on a temporary database (port 18080).
e2e: build
    bash scripts/gate-e2e.sh

# Run the install playbooks' fenced commands in a clean Linux container (Docker, privileged for systemd). source: github (the published tag), tree (the working tree built under the tag) or a release directory such as dist/release.
[positional-arguments]
playbooks tag source="github" *flags:
    python3 scripts/playbooks.py run "$@"

# Every gate in order. There is no hosted CI: this is the merge gate, run on the tree that merges.
ci:
    #!/usr/bin/env bash
    set -euo pipefail
    snapshot() { { git diff HEAD --; git ls-files --others --exclude-standard -z | xargs -0 -r sha256sum; } | sha256sum; }
    before=$(snapshot)
    just check
    after=$(snapshot)
    if [ "$before" != "$after" ]; then
        echo "just ci: just check rewrote files; review them and commit them" >&2
        git status --short >&2
        exit 1
    fi
    just staticcheck
    just build-all
    if [ -n "$(git status --porcelain -- {{skill_renders}})" ]; then
        echo "just ci: a generated skill render has uncommitted edits; renders are never edited by hand: move the change into internal/skillgen/source" >&2
        git status --short -- {{skill_renders}} >&2
        exit 1
    fi
    just skills
    if [ -n "$(git status --porcelain -- {{skill_renders}})" ]; then
        git status --short -- {{skill_renders}} >&2
        echo "just ci: a checked-in skill render differs from its source; edit internal/skillgen/source, run just skills and commit both" >&2
        exit 1
    fi
    go test -race -count=1 ./...
    just fuzz
    just e2e
    shellcheck -x skills/agentfeedback/scripts/install.sh
    if ls skills/agentfeedback-triage/scripts/*.sh >/dev/null 2>&1; then
        (cd skills/agentfeedback-triage/scripts && shellcheck -x ./*.sh)
    fi
    bash tests/skill/run-tests.sh
    bash tests/skill/triage-tests.sh
    python3 tests/playbooks/test_playbooks.py
    python3 scripts/playbooks.py check
    shellcheck -x scripts/*.sh
    just contract

# Build the Docker image tagged agentfeedback.
docker-build:
    #!/usr/bin/env bash
    set -euo pipefail
    docker build \
        --build-arg VERSION="$(git describe --tags --match 'v[0-9]*' --dirty --always)" \
        --build-arg COMMIT="$(git rev-parse HEAD)" \
        -t agentfeedback .

# Build the multi-arch image and push it to GHCR under every tag given. Release step only; needs `docker login ghcr.io`.
image-push +tags:
    #!/usr/bin/env bash
    set -euo pipefail
    args=()
    for t in {{tags}}; do args+=(-t "ghcr.io/agentfeedback/agentfeedback:$t"); done
    docker buildx build --platform linux/amd64,linux/arm64 \
        --build-arg VERSION="$(git describe --tags --match 'v[0-9]*' --dirty --always)" \
        --build-arg COMMIT="$(git rev-parse HEAD)" \
        --push "${args[@]}" .

# Publish a release of the pushed main: gates, tag, GoReleaser upload, verification, image. Maintainers only; docs/releases.md.
[positional-arguments]
[no-cd]
release tag title notes:
    bash "{{justfile_directory()}}/scripts/release.sh" "$@"

# Every precondition of `just release` plus a snapshot build of the matrix; publishes nothing.
[positional-arguments]
[no-cd]
release-check tag title notes:
    bash "{{justfile_directory()}}/scripts/release.sh" --check "$@"

# Remove build artifacts.
clean:
    rm -rf bin/ dist/
