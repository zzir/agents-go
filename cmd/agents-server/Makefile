BINARY   := agents-server
FRONTEND := internal/web/frontend
DIST     := dist

VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE     := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS  := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: all build frontend dev run clean test release openapi

all: build

# Regenerate the OpenAPI 3.1 spec (internal/docs/swagger.yaml) from swag annotations.
openapi:
	go tool swag fmt -g main.go -d .,internal/handler
	go tool swag init --v3.1 -g main.go --parseDependency --parseInternal -o internal/docs --outputTypes yaml

frontend:
	cd $(FRONTEND) && npm install && npm run build

build: frontend
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) .

run: build
	./$(BINARY)

release: frontend
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; \
		arch=$${platform#*/}; \
		ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		out=$(DIST)/$(BINARY)-$$os-$$arch$$ext; \
		echo "  build $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags '$(LDFLAGS)' -o $$out . || exit 1; \
	done
	@echo "done → $(DIST)/"

dev:
	cd $(FRONTEND) && npm run dev

test:
	go test ./...

clean:
	rm -f $(BINARY)
	rm -rf $(DIST)
	rm -rf $(FRONTEND)/dist
	rm -rf $(FRONTEND)/node_modules/.vite
