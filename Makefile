.PHONY: assets generate build test run tidy

# Resolved lazily from go.mod so it cannot drift from the pinned dependency.
TEMPL_VERSION = $(shell go list -m -f '{{.Version}}' github.com/a-h/templ)

assets:
	bun install --frozen-lockfile
	bun run build:editor
	bun run build:terminal
generate:
	go run github.com/a-h/templ/cmd/templ@$(TEMPL_VERSION) generate
tidy: generate
	go mod tidy
build: generate
	go build -o bin/nori ./cmd/nori
test: generate
	go test ./...
run: build
	./bin/nori
