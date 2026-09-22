# Development commands. Everything CI runs is a recipe here — the shared
# check workflow (truvity/ci-workflows) runs each one as its own job.

# Disable go.work (parent workspace interferes with standalone module builds)
export GOWORK := "off"

charts := "nats-auth-callout"

# Format all Go files (gofmt + goimports via golangci-lint)
fmt:
    golangci-lint fmt ./...

# Build the responder binary.
build: fmt
    go build -o bin/nats-auth-callout ./cmd/responder/

# Lint every chart and the Go module. The schema is part of the lint:
# an unknown key must fail the render, not be silently ignored, and every
# negative fixture under tests/invalid/<chart>/ must fail — one that
# renders is a hole in the validation nobody would otherwise notice.
# `config verify` runs first: the v2 schema silently accepts a stale
# top-level `linters-settings:` block, and only verify rejects it.
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    for chart in {{ charts }}; do
      helm lint "charts/$chart"
      # Not `! helm template ...`: bash's `set -e` ignores a command
      # negated with `!`, so such a probe could never fail the recipe.
      if helm template x "charts/$chart" --set bogusKey=1 >/dev/null 2>&1; then
        echo "$chart: an unknown key rendered" >&2
        exit 1
      fi
      for values in tests/invalid/"$chart"/*.yaml; do
        if helm template invalid "charts/$chart" -f "$values" >/dev/null 2>&1; then
          echo "RENDERED BUT SHOULD HAVE FAILED: $values" >&2
          exit 1
        fi
      done
      echo "$chart: schema and $(ls tests/invalid/"$chart"/*.yaml | wc -l | tr -d ' ') negative fixtures OK"
    done
    golangci-lint config verify
    golangci-lint run ./...

# Golden renders (every test case compared with tests/golden) and the unit tests.
test:
    hack/golden.sh
    go test ./... -coverprofile=coverage.out

# Regenerate the golden renders — review the diff before committing.
golden:
    hack/golden.sh update

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

# Reachable Go advisories.
vuln:
    govulncheck ./...

# Run go mod tidy
tidy:
    go mod tidy

# Clean build artifacts
clean:
    rm -rf bin/ dist/ coverage.out

# Everything CI runs on a pull request.
check: build lint test leak-canary vuln

# Build a snapshot release locally (no push, no tag)
snapshot:
    goreleaser release --snapshot --clean

# Package the chart locally (the release workflow stamps the version from the tag).
helm-package:
    helm package charts/nats-auth-callout --destination dist/
