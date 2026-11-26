.PHONY: build run test fuzz-ledger fmt vet lint check ci-check tidy clean

# Dev-loop targets. Assumes `go`, `golangci-lint`, etc. are on PATH -
# `nix develop` (or direnv) provides them. For release builds use `nix build`.

BIN := sergeant
PKG := ./cmd/sergeant
FUZZTIME ?= 15s

build:
	go build -o $(BIN) $(PKG)

run: build
	./$(BIN)

test:
	go test ./...

fuzz-ledger:
	go test ./internal/parser -run '^$$' -fuzz '^FuzzDecimalAdjustment$$' -fuzztime $(FUZZTIME) -parallel 4
	go test ./internal/shadow -run '^$$' -fuzz '^FuzzDebtHistory$$' -fuzztime $(FUZZTIME) -parallel 4

fmt:
	go fmt ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

check: fmt vet lint test

ci-check: vet lint test

tidy:
	go mod tidy

clean:
	rm -f $(BIN)
