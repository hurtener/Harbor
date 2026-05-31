.PHONY: help build console-build test vet lint lint-revive preflight drift-audit markdownlint check-mirror install-hooks clean dev wave13-coverage-check bench bench-check release-build release-dryrun

help:
	@echo "Harbor — make targets"
	@echo "  build           Build the harbor binary (skipped until Phase 1 lands)"
	@echo "  console-build   Build the SvelteKit Console + stage it into cmd/harbor/consoledist"
	@echo "  test            go test -race ./..."
	@echo "  vet             go vet ./..."
	@echo "  lint            golangci-lint run (all linters)"
	@echo "  lint-revive     golangci-lint run --enable-only revive (Phase 80 doc-hygiene gate)"
	@echo "  preflight       Build + boot + run smoke checks + drift-audit + tear down"
	@echo "  drift-audit     Verify design coherence (RFC, plans, briefs, mirror)"
	@echo "  markdownlint    Lint Markdown with the pinned cli2 version CI uses (@v15 → 0.12.1)"
	@echo "  wave13-coverage-check  Assert every Console page has a Playwright spec"
	@echo "  release-build   Build the version-stamped static release artifact into dist/"
	@echo "  release-dryrun  Exercise the release build end-to-end without a tag (Phase 81)"
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

# build rebuilds the Console bundle first (so the produced binary
# embeds a fresh Console) and then builds the static CGo-free `harbor`
# binary. Phase 83k (D-157): the pre-83k `make build` skipped the
# Console rebuild, so a fresh `git clone` + `make build` produced a
# binary that served the synthesized placeholder page — the same
# failure mode `go install github.com/.../cmd/harbor@latest` still
# hits today (operators can't tell `go install` to run a Make target).
# Local iterative dev that knows it didn't touch `web/console/`
# should use `make build-fast` to skip the npm step.
build: console-build
	@if [ -f cmd/harbor/main.go ]; then \
		CGO_ENABLED=0 go build -ldflags='-s -w' -o bin/harbor ./cmd/harbor; \
	else \
		echo "skip build: cmd/harbor/main.go absent (Phase 1 hasn't landed)"; \
	fi

# build-fast skips the Console rebuild (Phase 83k / D-157). The
# iterative-dev shortcut for changes that don't touch `web/console/`.
# The produced binary embeds whatever `cmd/harbor/consoledist/` holds
# on disk — stale relative to `web/console/src/` if you edited the
# Console without re-running `make console-build` yourself. CI's
# Console-staleness gate (`scripts/check-console-bundle.sh`) catches
# the drift before merge.
build-fast:
	@if [ -f cmd/harbor/main.go ]; then \
		CGO_ENABLED=0 go build -ldflags='-s -w' -o bin/harbor ./cmd/harbor; \
	else \
		echo "skip build-fast: cmd/harbor/main.go absent (Phase 1 hasn't landed)"; \
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

# lint runs the full golangci-lint gate (every linter in .golangci.yml).
# It fails LOUDLY when golangci-lint is absent rather than skipping —
# a silent skip is exactly the no-op bug the Wave 14 lint hardening
# (D-141) closed. CI installs golangci-lint before invoking this.
lint:
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "ERROR: golangci-lint not installed — the lint gate cannot run." >&2; \
		echo "  install: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2" >&2; \
		exit 1; \
	fi
	golangci-lint run

# lint-revive is the Phase 80 (D-138) documentation-hygiene gate. It
# runs ONLY the `revive` linter via the dedicated `.golangci-revive.yml`
# config — whose `exported` rule enforces a godoc comment on every
# exported identifier and whose `package-comments` rule enforces a
# package-level doc comment. This is the narrow, binding acceptance
# check the master plan names for Phase 80; CI's `lint` job runs it.
# The broader `make lint` (all linters) carries a pre-existing backlog
# predating enforcement and is a separate release-hardening effort.
lint-revive:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		if [ -n "$$(find . -name '*.go' -not -path './vendor/*' 2>/dev/null | head -1)" ]; then \
			golangci-lint run -c .golangci-revive.yml; \
		else \
			echo "skip lint-revive: no Go sources yet"; \
		fi; \
	else \
		echo "golangci-lint not installed; skipping"; \
	fi

preflight:
	@bash scripts/preflight.sh

drift-audit:
	@bash scripts/drift-audit.sh

# markdownlint runs the SAME markdownlint-cli2 version CI pins
# (DavidAnson/markdownlint-cli2-action@v15 bundles markdownlint-cli2
# 0.12.1) with the SAME globs, so local and CI can never drift on a
# rule like MD029 (a v0.33-vs-v0.40 ordered-list gap bit the v1.2.0
# PR). The version literal below is the pin; bump it in lockstep with
# the action tag in .github/workflows/ci.yml. Requires npx (node); a
# clone without node skips it (drift-audit degrades gracefully).
#
# File set: we feed markdownlint the git-tracked + untracked-not-ignored
# .md files (`git ls-files --cached --others --exclude-standard`) rather
# than a raw `**/*.md` glob. markdownlint-cli2 does NOT honour .gitignore,
# so a raw glob locally scans thousands of files CI never sees — the
# dependency READMEs under web/console/node_modules and the full repo
# copies under .claude/worktrees. Driving off git makes the linted set
# identical to CI's checkout (245 tracked .md) while still catching a NEW
# uncommitted plan file (untracked-but-not-ignored) — the exact v1.2.0
# MD029 failure mode this target exists to prevent.
MARKDOWNLINT_CLI2_VERSION ?= 0.12.1
markdownlint:
	@if command -v npx >/dev/null 2>&1; then \
		git ls-files -z --cached --others --exclude-standard -- '*.md' \
			| xargs -0 npx --yes markdownlint-cli2@$(MARKDOWNLINT_CLI2_VERSION); \
	else \
		echo "npx not installed; skipping markdownlint (CI still enforces it)"; \
	fi

# wave13-coverage-check asserts every Console page-spec under
# docs/design/console/page-<slug>.md has a matching Playwright spec at
# web/console/tests/<slug>-page.spec.ts (Phase 75a / D-131). Evaluations
# is excluded (post-V1, D-064). Wired into the frontend-e2e CI job.
wave13-coverage-check:
	@bash scripts/console/check-page-coverage.sh

# bench runs the Phase 79 performance-benchmark suite (engine
# throughput, bus fan-out, memory-strategy latency) against the real
# components. `-benchtime=100ms` bounds each benchmark so the suite
# completes fast and reproducibly; `-count=6` gives benchstat a
# sample to compute variance from. Pipe the output to
# docs/perf/baseline.txt to refresh the committed baseline
# (deliberately, in a reviewed PR — never auto).
bench:
	@go test -run='^$$' -bench=. -benchmem -benchtime=100ms -count=6 ./test/benchmarks/...

# bench-check is the perf-regression gate (Phase 79 / D-136): it runs
# the suite and fails when a benchmark regresses past the threshold
# versus docs/perf/baseline.txt, via benchstat. Wired into the
# `perf-regression` CI job.
bench-check:
	@bash scripts/perf/check-regression.sh

# release-build produces the CGo-free static `harbor` release artifact
# (Phase 81 / D-139). It delegates to scripts/release-build.sh — the
# SINGLE home of the `-ldflags -X 'main.HarborVersion=...'` version-
# stamping incantation. The version is derived from `git describe
# --tags` (or HARBOR_RELEASE_VERSION when set, as the release workflow
# sets it from the pushed tag); an un-tagged tree falls back to
# v0.0.0-dev. The artifact + a SHA-256 checksum land in dist/.
release-build:
	@bash scripts/release-build.sh

# release-dryrun exercises the release build end-to-end WITHOUT pushing
# a `v*` tag — the master plan's Phase 81 "release dry-run" test. It
# runs the exact scripts/release-build.sh path the release workflow
# runs, forcing a synthetic dry-run version, then verifies the artifact
# + checksum exist, the checksum verifies, and the stamped binary's
# `harbor version` reports the stamped string. Run it before tagging.
release-dryrun:
	@bash scripts/release-dryrun.sh

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
