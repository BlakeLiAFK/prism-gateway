GO ?= go
BINARY := bin/prism-gateway
.PHONY: build test race vet check run demo clean
build:
	mkdir -p bin
	CGO_ENABLED=1 $(GO) build -trimpath -o $(BINARY) ./cmd/gateway
test:
	CGO_ENABLED=1 $(GO) test -count=1 -timeout 60s ./...
race:
	CGO_ENABLED=1 $(GO) test -race -count=1 -timeout 90s ./...
vet:
	CGO_ENABLED=1 $(GO) vet ./...
check: test vet
run: build
	./$(BINARY)
demo: build
	./$(BINARY) --demo
clean:
	rm -rf bin
