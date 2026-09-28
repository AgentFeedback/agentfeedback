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

# Build the Docker image tagged agentfeedback.
docker-build:
    docker build -t agentfeedback .

# Remove build artifacts.
clean:
    rm -rf bin/
