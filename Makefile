.DEFAULT_GOAL := help
SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.NOTPARALLEL:
VERSION ?= dev
export VERSION
export PYTHONDONTWRITEBYTECODE := 1
.PHONY: help install lint test build artifact-smoke vuln check web-check browser-check platform-check dev stop clean
help:
	@echo 'make install  Install pinned tools and locked dependencies'
	@echo 'make check    Verify CLI, archived installers, embedded UI, browser and platform boundaries'
install:
	mise trust .mise.toml
	mise install
	mise exec -- go mod download
	mise exec -- npm ci --prefix ui
lint test build artifact-smoke vuln web-check browser-check platform-check:
	mise exec -- bash scripts/profile-$@.sh
check:
	$(MAKE) web-check lint build artifact-smoke browser-check test vuln platform-check
dev stop:
	@echo '$@: unsupported: interactive terminal agent requires caller-owned configuration and session'
clean:
	mise exec -- python3 -c 'import shutil; [shutil.rmtree(p, ignore_errors=True) for p in ("dist", ".artifacts", "ui/node_modules", "ui/dist")]'
