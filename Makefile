# GPUMon build, test, and verification targets. See README.md.
SHELL := /bin/bash
VERSION := 0.1.0
GO ?= $(shell command -v go 2>/dev/null || echo /usr/local/go/bin/go)
PYTHON ?= python3
VENV := .venv
PY := $(VENV)/bin/python
# go-nvml's headers mark some functions deprecated; the agent does not use them.
export CGO_CFLAGS := -O2 -Wno-deprecated-declarations
export GOTOOLCHAIN := local

.PHONY: all build venv test test-go test-py lint e2e verify package clean

all: build

venv: $(VENV)/.stamp
$(VENV)/.stamp: central/requirements-dev.txt central/pyproject.toml
	$(PYTHON) -m venv $(VENV)
	$(VENV)/bin/pip install -q --disable-pip-version-check -r central/requirements-dev.txt
	$(VENV)/bin/pip install -q --disable-pip-version-check --no-deps -e central
	touch $@

build: venv
	cd agent && CGO_ENABLED=1 GOOS=linux GOARCH=amd64 $(GO) build -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" -o ../bin/gpumon-agent ./cmd/gpumon-agent
	@echo "built bin/gpumon-agent and the central virtualenv $(VENV)"

# Unit and contract tests (Go and Python).
test: test-go test-py

test-go: build
	@mkdir -p verification
	cd agent && $(GO) test -json -count=1 ./... > ../verification/go-test.json || true
	$(PY) scripts/go_test_summary.py verification/go-test.json

test-py: venv
	@mkdir -p verification
	GPUMON_PYTEST_RESULTS=verification/pytest-results.json $(PY) -m pytest -q central/tests

lint: venv
	cd agent && $(GO) vet ./...
	@out="$$(cd agent && gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(VENV)/bin/ruff check central scripts
	$(VENV)/bin/shellcheck deploy/*.sh scripts/*.sh
	$(PY) scripts/ascii_check.py

e2e: build
	scripts/e2e.sh

# Runs everything, then writes verification/report.md and report.json.
# Exits non-zero if lint fails or any auto/e2e requirement is FAIL or NO-TEST.
verify: build
	@rc=0; \
	$(MAKE) --no-print-directory lint || rc=1; \
	$(MAKE) --no-print-directory test-go || true; \
	$(MAKE) --no-print-directory test-py || true; \
	$(MAKE) --no-print-directory e2e || true; \
	$(PY) scripts/verify.py --lint-status $$rc

package: build
	scripts/package.sh $(VERSION)

clean:
	rm -rf bin dist verification/*.json verification/*.md verification/e2e-work
