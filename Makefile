.PHONY: build run run-real mock verify clean fmt vet

BIN := bin

build:
	@mkdir -p $(BIN)
	go build -o $(BIN)/gateway ./cmd/gateway
	go build -o $(BIN)/mockupstream ./cmd/mockupstream

run: build
	$(BIN)/gateway -config configs/gateway.json

run-real: build
	$(BIN)/gateway -config configs/gateway.real.json

mock: build
	$(BIN)/mockupstream -listen :9090

verify:
	./scripts/verify.sh

fmt:
	gofmt -w ./cmd ./internal

vet:
	go vet ./...

clean:
	rm -rf $(BIN) data/prompts.json
