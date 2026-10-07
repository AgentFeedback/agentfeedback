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
skill_renders := "skills/agentfeedback/SKILL.md skills/agentfeedback-docs plugins/agentfeedback .claude-plugin .agents/plugins"

# Regenerate every checked-in skill render from internal/skillgen/source and the embedded reference files, including the plugin bundle and both marketplace manifests. Never edit a render by hand.
skills: build
    #!/usr/bin/env bash
    set -euo pipefail
    tmp=$(mktemp)
    tmpd=$(mktemp -d)
    trap 'rm -f "$tmp"; rm -rf "$tmpd"' EXIT
    ./bin/agentfeedback skill render skill-md > "$tmp"
    ./bin/agentfeedback skill render docs --out "$tmpd/out"
    ./bin/agentfeedback skill render marketplace --out "$tmpd/mkt"
    cp "$tmp" skills/agentfeedback/SKILL.md
    rm -rf skills/agentfeedback-docs
    cp -R "$tmpd/out" skills/agentfeedback-docs
    rm -rf plugins/agentfeedback .claude-plugin .agents/plugins
    mkdir -p plugins .agents
    cp -R "$tmpd/mkt/plugins/agentfeedback" plugins/agentfeedback
    cp -R "$tmpd/mkt/.claude-plugin" .claude-plugin
    cp -R "$tmpd/mkt/.agents/plugins" .agents/plugins

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

# Local-mode end-to-end: the client commands against the data-directory database, no server process.
e2e-local: build
    bash scripts/gate-local.sh

# Run the install playbooks' fenced commands in a clean Linux container (Docker, privileged for systemd). source: github (the published tag), tree (the working tree built under the tag) or a release directory such as dist/release.
[positional-arguments]
playbooks tag source="github" *flags:
    python3 scripts/playbooks.py run "$@"

# The merge gate: every gate of ci-host inside the toolchain container, then the live Claude Code check. There is no hosted CI: this runs on the tree that merges, and touches nothing on the machine but the repository and Docker.
ci:
    bash scripts/in-container.sh just ci-host
    bash scripts/live-harness.sh

# Run a recipe or a command inside the toolchain container: `just box test`, `just box go test ./pkg/client`; `just box -- <command>` when the command shares a recipe's name. Recipes that start containers themselves run on the host instead.
[positional-arguments]
box +args:
    #!/usr/bin/env bash
    set -euo pipefail
    case "$1" in
        --) shift; exec bash scripts/in-container.sh "$@" ;;
        ci|live-harness|playbooks|docker-build|release|release-check|box)
            echo "just box: $1 starts its own containers; run just $1 on the host" >&2
            exit 2 ;;
    esac
    for recipe in $(just --summary); do
        if [ "$recipe" = "$1" ]; then
            exec bash scripts/in-container.sh just "$@"
        fi
    done
    exec bash scripts/in-container.sh "$@"

# Install, list and uninstall the agentfeedback MCP entry, skill and Stop hook with the real Claude Code CLI in a network-less container, against a server on its loopback.
live-harness:
    bash scripts/live-harness.sh

# Every gate in order on the host itself. For debugging only: `just ci` runs this inside the toolchain container and is the merge gate.
ci-host:
    #!/usr/bin/env bash
    set -euo pipefail
    want=$(sed -n 's/^toolchain //p' go.mod)
    if [ "$(go env GOVERSION)" != "$want" ]; then
        echo "just ci: go is $(go env GOVERSION), go.mod pins $want; update tests/ci/Dockerfile or go.mod so they agree" >&2
        exit 1
    fi
    if [ "${AF_CI_CONTAINER:-}" = 1 ]; then
        echo "just ci: toolchain $(go env GOVERSION), node $(node --version), $(uv --version), shellcheck $(shellcheck --version | sed -n 's/^version: //p'), $(just --version), claude $(claude --version)" >&2
    fi
    # Untracked files and their content; a nested repository (a linked
    # worktree under the checkout) is listed as a directory and skipped.
    snapshot() { { git diff HEAD --; git ls-files --others --exclude-standard -z | { grep -zv '/$' || true; } | xargs -0 -r sha256sum; } | sha256sum; }
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
    if command -v claude >/dev/null 2>&1; then
        claude plugin validate --strict plugins/agentfeedback
        claude plugin validate --strict .
    elif [ "${AF_CI_CONTAINER:-}" = 1 ]; then
        echo "just ci: claude is missing from the toolchain image; rebuild it (AF_CI_IMAGE_REBUILD=1)" >&2
        exit 1
    else
        echo "just ci: claude is not on PATH; claude plugin validate skipped (validate plugins/agentfeedback and the repository root by hand)" >&2
    fi
    go test -race -count=1 ./...
    just fuzz
    just e2e
    just e2e-local
    shellcheck -x -P SCRIPTDIR skills/*/scripts/*.sh tests/skill/*.sh
    bash tests/skill/run-tests.sh
    python3 tests/skill/test_cluster.py
    python3 tests/skill/triage-playbooks.py
    python3 tests/playbooks/test_playbooks.py
    python3 scripts/playbooks.py check
    shellcheck -x scripts/*.sh tests/live/*.sh
    just contract
    if [ "$(snapshot)" != "$before" ]; then
        echo "just ci: a gate changed the tree" >&2
        git status --short >&2
        exit 1
    fi

# Build the Docker image tagged agentfeedback from source. The published image is built by the release step (GoReleaser).
docker-build:
    #!/usr/bin/env bash
    set -euo pipefail
    version=$(git describe --tags --match 'v[0-9]*' --dirty --always)
    docker build \
        --build-arg VERSION="${version#v}" \
        --build-arg COMMIT="$(git rev-parse HEAD)" \
        -t agentfeedback .

# Publish a release of the pushed main: gates, tag, GoReleaser upload of the release and the image, verification. Maintainers only; docs/releases.md.
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
