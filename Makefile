# Generated cookbook entrypoint. Change cookbook.toml and regenerate.
# Recipe data SHA256: f4f2842b08f7c23306aeb027c4df148482d3153a48ca239971dd2d54f99a9e81
COOKBOOK_REF := 14dd4792df910b8f72ae10bf01c7e426b97b667e
COOKBOOK_RECIPE := rust-cli
COOKBOOK_ROOT ?= .artifacts/cookbook/$(COOKBOOK_REF)
COOKBOOK_TEST_SUITES := lint unit security integration e2e
COOKBOOK_CAPABILITIES :=
COOKBOOK_EMPTY :=
COOKBOOK_SPACE := $(COOKBOOK_EMPTY) $(COOKBOOK_EMPTY)
cookbook_path = $(subst $(COOKBOOK_SPACE),\$(COOKBOOK_SPACE),$(1))
COOKBOOK_ENTRY := $(call cookbook_path,$(COOKBOOK_ROOT)/cookbook/recipes/$(COOKBOOK_RECIPE).mk)

ifeq ($(wildcard $(COOKBOOK_ENTRY)),)
.DEFAULT_GOAL := help
.PHONY: help setup
help:
	@printf '%s\n' 'make help               List implemented commands' 'make setup              Prepare pinned tools and dependencies' 'make fmt                Apply formatting' 'make build              Compile, package and validate outputs' 'make test               Run implemented test groups without building' 'make test:lint          Check formatting, code and configuration' 'make test:unit          Run isolated behavior tests' 'make test:security      Run applicable security checks' 'make test:integration   Test real component boundaries' 'make test:e2e           Run complete product journeys; requires current build' 'make clean              Remove declared outputs; preserve retained data'
	@printf '%s\n' 'Run make setup once to fetch the pinned cookbook and prepare dependencies.'
setup:
	@mkdir -p "$(COOKBOOK_ROOT)"
	@git -C "$(COOKBOOK_ROOT)" init -q
	@git -C "$(COOKBOOK_ROOT)" fetch --quiet --depth=1 https://github.com/nicodes/tools.git "$(COOKBOOK_REF)"
	@git -C "$(COOKBOOK_ROOT)" checkout --quiet --detach "$(COOKBOOK_REF)"
	@test "$$(git -C "$(COOKBOOK_ROOT)" rev-parse HEAD)" = "$(COOKBOOK_REF)"
	@$(MAKE) --no-print-directory setup
else
include $(COOKBOOK_ENTRY)
endif
