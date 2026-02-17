# Tool versions - override on the command line to upgrade:
#   make install-tools KUBEBUILDER_VERSION=4.12.0 KIND_VERSION=0.31.0
KUBEBUILDER_VERSION ?= 4.12.0
KIND_VERSION ?= 0.31.0

# Platform detection
OS ?= $(shell go env GOOS 2>/dev/null || echo linux)
ARCH ?= $(shell go env GOARCH 2>/dev/null || echo amd64)

# Local bin directory (kubebuilder convention)
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

# Tool binaries
KUBEBUILDER ?= $(LOCALBIN)/kubebuilder
KIND ?= $(LOCALBIN)/kind

# Download URLs
KUBEBUILDER_DOWNLOAD_URL ?= https://github.com/kubernetes-sigs/kubebuilder/releases/download/v$(KUBEBUILDER_VERSION)/kubebuilder_$(OS)_$(ARCH)
KIND_DOWNLOAD_URL ?= https://kind.sigs.k8s.io/dl/v$(KIND_VERSION)/kind-$(OS)-$(ARCH)

## Targets

.PHONY: help
help: ## Show available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: install-tools
install-tools: install-kubebuilder install-kind ## Install all development tools

.PHONY: install-kubebuilder
install-kubebuilder: $(KUBEBUILDER) ## Install or upgrade kubebuilder
$(KUBEBUILDER): $(LOCALBIN)
	$(call install-binary,$(KUBEBUILDER),$(KUBEBUILDER_DOWNLOAD_URL),$(KUBEBUILDER_VERSION))

.PHONY: install-kind
install-kind: $(KIND) ## Install or upgrade kind
$(KIND): $(LOCALBIN)
	$(call install-binary,$(KIND),$(KIND_DOWNLOAD_URL),$(KIND_VERSION))

## Helper functions

# install-binary downloads a binary if the desired version is not already present.
# $(1) = target binary path  $(2) = download URL  $(3) = version string
define install-binary
@if [ -f "$(1)" ]; then \
  echo "Current: $$($(1) version 2>/dev/null || echo unknown)" ; \
else \
  echo "$(notdir $(1)) is not installed" ; \
fi ; \
if [ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ]; then \
  echo "$(notdir $(1)) v$(3) is already installed" ; \
else \
  echo "Downloading $(notdir $(1)) v$(3)..." ; \
  curl -fsSL -o "$(1)-$(3)" "$(2)" && \
  chmod +x "$(1)-$(3)" && \
  rm -f "$(1)" && \
  ln -sf "$(1)-$(3)" "$(1)" && \
  echo "Installed: $$($(1) version 2>/dev/null || echo v$(3))" ; \
fi
endef
