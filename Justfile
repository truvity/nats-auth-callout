# Development commands for nats-auth-callout

# Disable go.work (parent workspace interferes with standalone module builds)
export GOWORK := "off"

# Format all Go files (gofmt + goimports via golangci-lint)
fmt:
    golangci-lint fmt ./...

# Build all binaries
build: fmt
    go build -o bin/nats-auth-callout ./cmd/responder/

# Run unit tests
test:
    go test ./... -coverprofile=coverage.out


# Run linters. `config verify` first: the v2 schema silently accepts a
# stale top-level `linters-settings:` block, and only verify rejects it.
lint:
    golangci-lint config verify
    golangci-lint run ./...

# Run Go vulnerability check
vuln:
    govulncheck ./...

# Run go mod tidy
tidy:
    go mod tidy

# Clean build artifacts
clean:
    rm -rf bin/ dist/ coverage.out

# Run all checks (build + unit tests + integration tests + lint + vuln)
# Render the chart with representative values; prove the schema rejects
# an unknown key (values.schema.json is the contract — a typo must fail
# the render, not be silently ignored).
chart-lint:
    helm lint charts/nats-auth-callout
    helm template nats-auth-callout charts/nats-auth-callout \
        --set image.tag=0.0.0 \
        --set natsURL=nats://nats.nats.svc:4222 >/dev/null
    ! helm template nats-auth-callout charts/nats-auth-callout --set bogusKey=1 >/dev/null 2>&1

# The reason this repository can be public. Runs in CI as its own job.
leak-canary:
    hack/leak-canary.sh

# Run the tests under the race detector. The responder answers callout
# requests concurrently and shares the broker connection between them, so a
# data race there would be a wrong answer rather than a crash, and would not
# show up in an ordinary run.
#
# This is not part of `check`, and deliberately. Everything else here builds
# with cgo off, which is what makes the binary static and the image small;
# the race detector is the one thing that needs a C toolchain. Putting it in
# the gate would mean every contributor needs one to run the gate at all. CI
# runs this as its own job, where the toolchain is the runner's own.
race:
    CGO_ENABLED=1 go test -race ./...

check: build test lint chart-lint leak-canary vuln

# Build a snapshot release locally (no push, no tag)
snapshot:
    goreleaser release --snapshot --clean

# Package Helm chart locally
helm-package:
    helm package charts/nats-auth-callout --destination dist/
