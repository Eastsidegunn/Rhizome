.PHONY: ci fmt-check test test-race vet

fmt-check:
	@test -z "$$(gofmt -l . | grep '\.go$$')" || { echo 'gofmt required:'; gofmt -l .; exit 1; }

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

ci: fmt-check test test-race vet
