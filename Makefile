.PHONY: help test test-kit-toolchain lint fmt

# Default target prints help.
help:
	@echo "donmai-smokes — OSS-canonical smoke harness for the donmai binary"
	@echo ""
	@echo "Targets:"
	@echo "  make test    Run go test -race ./... with GOWORK=off"
	@echo "  make test-kit-toolchain  Validate kit manifests/plans without cloud access"
	@echo "  make lint    Run golangci-lint run ./..."
	@echo "  make fmt     Run gofumpt -w ."
	@echo ""
	@echo "Status: Wave 10 Phase 10 — donmai-only smoke tests live. See AGENTS.md for the boundary."

# GOWORK=off keeps this harness's module resolution decoupled from any sibling
# go.work at the org root. Mirrors rensei-smokes' Wave 9 fix (commit a2a4a4b).
test:
	GOWORK=off go test -race ./...

test-kit-toolchain:
	bash -n kit-toolchain-e2b/run.sh
	python3 -m unittest discover -s kit-toolchain-e2b -p 'test_*.py' -v
	KIT_E2B_DRY_RUN=1 KIT_TOOLCHAIN_KIT=ts-next kit-toolchain-e2b/run.sh
	KIT_E2B_DRY_RUN=1 KIT_TOOLCHAIN_KIT=swift kit-toolchain-e2b/run.sh

lint:
	GOWORK=off golangci-lint run ./...

fmt:
	gofumpt -w .
