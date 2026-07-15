.PHONY: generate build test run tidy
generate:
	go run github.com/a-h/templ/cmd/templ@latest generate
tidy: generate
	go mod tidy
build: generate
	go build -o bin/deploybot ./cmd/deploybot
test: generate
	go test ./...
run: build
	./bin/deploybot
