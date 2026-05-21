.PHONY: help build console-build test vet lint preflight drift-audit check-mirror install-hooks clean dev

help:
	@echo "Harbor — make targets"
	@echo "  build           Build the harbor binary (skipped until Phase 1 lands)"
	@echo "  console-build   Build the SvelteKit Console + stage it into cmd/harbor/consoledist"
	@echo "  test            go test -race ./..."
	@echo "  vet             go vet ./..."
	@echo "  lint            golangci-lint run"
	@echo "  preflight       Build + boot + run smoke checks + drift-audit + tear down"
	@echo "  drift-audit     Verify design coherence (RFC, plans, briefs, mirror)"
	@echo "  check-mirror    Verify AGENTS.md == CLAUDE.md"
	@echo "  install-hooks   Install the pre-commit hook (one-time per clone)"
	@echo "  dev             Run ./bin/harbor dev (skipped until Phase 1 lands)"
	@echo "  clean           Remove build artifacts"

# console-build builds the SvelteKit Console static bundle and stages it
# into cmd/harbor/consoledist/ — the directory cmd/harbor/console_embed.go
# bakes into the binary via //go:embed (Phase 73m / D-129). The Console
# build (web/console/build/) is gitignored (CLAUDE.md §13); consoledist/
# ships a committed placeholder so `go build` always works, and this
# target overwrites it with the real bundle before a release build.
console-build:
	@if [ -d web/console ]; then \
		cd web/console && npm ci && npm run build; \
		find ../../cmd/harbor/consoledist -mindepth 1 ! -name .gitkeep -exec rm -rf {} +; \
		cp -R build/. ../../cmd/harbor/consoledist/; \
		echo "console-build: staged web/console/build -> cmd/harbor/consoledist"; \
	else \
		echo "skip console-build: web/console absent"; \
	fi

build:
	@if [ -f cmd/harbor/main.go ]; then \
		CGO_ENABLED=0 go build -ldflags='-s -w' -o bin/harbor ./cmd/harbor; \
	else \
		echo "skip build: cmd/harbor/main.go absent (Phase 1 hasn't landed)"; \
	fi

test:
	@if [ -n "$$(find . -name '*.go' -not -path './vendor/*' 2>/dev/null | head -1)" ]; then \
		go test -race ./...; \
	else \
		echo "skip test: no Go sources yet"; \
	fi

vet:
	@if [ -n "$$(find . -name '*.go' -not -path './vendor/*' 2>/dev/null | head -1)" ]; then \
		go vet ./...; \
	else \
		echo "skip vet: no Go sources yet"; \
	fi

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		if [ -n "$$(find . -name '*.go' -not -path './vendor/*' 2>/dev/null | head -1)" ]; then \
			golangci-lint run; \
		else \
			echo "skip lint: no Go sources yet"; \
		fi; \
	else \
		echo "golangci-lint not installed; skipping"; \
	fi

preflight:
	@bash scripts/preflight.sh

drift-audit:
	@bash scripts/drift-audit.sh

check-mirror:
	@diff -q AGENTS.md CLAUDE.md && echo "OK: AGENTS.md == CLAUDE.md" || (echo "DRIFT: AGENTS.md != CLAUDE.md"; exit 1)

install-hooks:
	@bash scripts/install-hooks.sh

dev:
	@if [ -x bin/harbor ]; then \
		./bin/harbor dev; \
	else \
		echo "bin/harbor not built. Run 'make build' first (Phase 1+)."; \
		exit 1; \
	fi

clean:
	@rm -rf bin/ dist/ build/
	@find . -name '*.test' -delete
	@find . -name 'coverage.out' -delete
	@find . -name 'coverage.html' -delete
