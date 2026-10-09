.PHONY: ci fmt-check test test-race vet hygiene signer signer-test

fmt-check:
	@test -z "$$(gofmt -l . | grep '\.go$$')" || { echo 'gofmt required:'; gofmt -l .; exit 1; }

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

hygiene:
	sh tools/hygiene.sh
	sh tools/hygiene_test.sh

ci: fmt-check test test-race vet hygiene

signer:
	cd signer && swift build -c release

signer-test:
	cd signer && swift build -c release && .build/release/rhizome-signer-selftest
