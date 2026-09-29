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

# Run the tests with race detection. No Docker, no database required.
test:
    go test -race -count=1 ./...

# Fuzz the decoder for a bounded time on top of its committed seed corpus.
fuzz:
    go test -run='^$' -fuzz='^FuzzDecode$' -fuzztime=30s ./pkg/envelope

# Run locally against a database in ./local (created on demand).
run-local: build
    mkdir -p local
    DATABASE_PATH=${DATABASE_PATH:-local/agentfeedback.db} \
    HTTP_LISTEN_ADDR=${HTTP_LISTEN_ADDR:-127.0.0.1:8090} \
    API_KEY=${API_KEY:-local-dev-key} \
    ./bin/agentfeedback

# Validate the contract files: schemas, OpenAPI document, examples, conformance fixtures. Needs uv and npx.
contract:
    uv run --locked --script scripts/contract-check.py
    npx --yes @redocly/cli@2.54.2 lint docs/openapi.yaml

# Live HTTP contract suite against a fresh server on a temporary database (port 18080).
e2e: build
    bash scripts/gate-e2e.sh

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
    go test -race -count=1 ./...
    just fuzz
    just e2e
    (cd skills/agentfeedback/scripts && shellcheck -x ./*.sh)
    if ls skills/agentfeedback-triage/scripts/*.sh >/dev/null 2>&1; then
        (cd skills/agentfeedback-triage/scripts && shellcheck -x ./*.sh)
    fi
    bash tests/skill/run-tests.sh
    just contract

# Build the Docker image tagged agentfeedback.
docker-build:
    docker build -t agentfeedback .

# Build the multi-arch image and push it to GHCR under every tag given. Release step only; needs `docker login ghcr.io`.
image-push +tags:
    #!/usr/bin/env bash
    set -euo pipefail
    args=()
    for t in {{tags}}; do args+=(-t "ghcr.io/agentfeedback/agentfeedback:$t"); done
    docker buildx build --platform linux/amd64,linux/arm64 --push "${args[@]}" .

# Remove build artifacts.
clean:
    rm -rf bin/
