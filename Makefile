GO ?= go
BINARY := bin/prism-gateway
.PHONY: build test race vet cover lint ui check hooks release run clean
build:
	mkdir -p bin
	CGO_ENABLED=1 $(GO) build -trimpath -o $(BINARY) ./cmd/gateway
test:
	CGO_ENABLED=1 $(GO) test -count=1 -timeout 60s ./...
race:
	CGO_ENABLED=1 $(GO) test -race -count=1 -timeout 90s ./...
vet:
	CGO_ENABLED=1 $(GO) vet ./...
cover:
	./scripts/cover.sh
lint:
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; \
	else echo "staticcheck 未安装，跳过（go install honnef.co/go/tools/cmd/staticcheck@latest）"; fi
ui: build
	python3 scripts/ui_smoke.py --binary ./$(BINARY)
check: vet lint cover ui
hooks:
	cp scripts/hooks/pre-push scripts/hooks/pre-commit .git/hooks/
	chmod +x .git/hooks/pre-push .git/hooks/pre-commit
release: check
	mkdir -p dist
	CGO_ENABLED=1 $(GO) build -trimpath -ldflags='-s -w' -o dist/prism-gateway-$(shell go env GOOS)-$(shell go env GOARCH) ./cmd/gateway
	cd dist && shasum -a 256 prism-gateway-* > SHA256SUMS
	@echo "产物与校验和已写入 dist/"
run: build
	./$(BINARY)
clean:
	rm -rf bin
