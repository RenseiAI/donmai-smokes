.PHONY: help test lint fmt

# Default target prints help.
help:
	@echo "agentfactory-smokes — OSS-canonical smoke harness for the af binary"
	@echo ""
	@echo "Targets:"
	@echo "  make test    Run go test ./... with GOWORK=off (matches rensei-smokes Wave 9 fix)"
	@echo "  make lint    Run golangci-lint run ./..."
	@echo "  make fmt     Run gofumpt -w ."
	@echo ""
	@echo "Status: Wave 10 Phase 7 scaffolding. Phase 9 lands the harness package;"
	@echo "Phase 10 lands the af-only smoke tests. See AGENTS.md for the boundary."

# GOWORK=off keeps this harness's module resolution decoupled from any sibling
# go.work at the org root. Mirrors rensei-smokes' Wave 9 fix (commit a2a4a4b).
test:
	GOWORK=off go test -race ./...

lint:
	golangci-lint run ./...

fmt:
	gofumpt -w .
