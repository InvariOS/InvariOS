GO       := go
GOOS     ?= linux
GOARCH   ?= amd64
PKGS      = $(or $(PKG),$(shell $(GO) list ./...))
BIN       = $(CURDIR)/bin
V         = 0
Q         = $(if $(filter 1,$V),,@)
M         = $(shell printf "\033[34;1m▶\033[0m")

IMAGE := ghcr.io/invarios/builder:main
PLATFORM := $(GOOS)/$(GOARCH)
KERNEL_IMAGE := ghcr.io/invarios/kernel:6.18.49-amd64
SYSTEMD_BOOT_IMAGE := ghcr.io/invarios/systemd-boot:261.2-amd64
OPENBAO_VERSION := 2.6.2
OVMF_DIR := .ovmf
OVMF_CODE := $(OVMF_DIR)/OVMF_CODE_4M.fd
OVMF_VARS := $(OVMF_DIR)/OVMF_VARS_4M.fd

# The build depends on Linux-only tooling (makefs.VFAT shells out to
# mkfs.vfat/mcopy). On Linux it can therefore run directly on the host;
# everywhere else it needs the builder container. DOCKER=true forces
# the container path on Linux too.
UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Linux)
DOCKER ?= false
else
DOCKER ?= true
endif

# Install pulls its boot artifact from a registry (see
# internal/bootimage) instead of copying it from whatever booted the
# install media, so `make build` needs somewhere to push it to and
# `make boot`'s QEMU guest needs somewhere to pull it back from. Both
# point at the throwaway registry in docker-compose.yml, not at
# ghcr.io/invarios/esp: local/dev builds shouldn't push dirty-tree
# artifacts into the real registry.
#
# The push side runs from wherever the build tool itself runs (the
# host on DOCKER=false, the builder container on DOCKER=true), so it
# needs a different address than the pull side, which always runs
# inside the QEMU guest: 10.0.2.2 is QEMU user-mode networking's alias
# for the host, reachable regardless of where the build ran.
#
# REGISTRY_PORT is the container's own listening port, reachable
# directly on the invarios-dev network without going through the host
# port mapping below. REGISTRY_HOST_PORT is the host-published port
# (see docker-compose.yml), used by anything reaching the registry
# from outside that network. They're kept separate because a fixed
# port on the host can collide with other software; the container
# port never does.
REGISTRY_PORT := 5000
REGISTRY_HOST_PORT := 5050
BOOT_IMAGE_REPO := invarios/esp
ifeq ($(DOCKER),true)
BOOT_IMAGE_PUSH_REPO := registry:$(REGISTRY_PORT)/$(BOOT_IMAGE_REPO)
else
BOOT_IMAGE_PUSH_REPO := localhost:$(REGISTRY_HOST_PORT)/$(BOOT_IMAGE_REPO)
endif
BOOT_IMAGE_PULL_REPO := 10.0.2.2:$(REGISTRY_HOST_PORT)/$(BOOT_IMAGE_REPO)

# Go module and build caches live in named Docker volumes so they
# survive `docker run --rm`. They are not bind-mounted from the
# working tree: the caches are thousands of small files, and a named
# volume stays inside Docker's Linux VM rather than crossing the host
# filesystem.
GO_MOD_VOLUME   := invarios-go-mod
GO_CACHE_VOLUME := invarios-go-cache

docker-mounts = \
	-v "$(CURDIR):/work" \
	-v "$(GO_MOD_VOLUME):/go/pkg/mod" \
	-v "$(GO_CACHE_VOLUME):/root/.cache/go-build" \
	-e GOMODCACHE=/go/pkg/mod \
	-e GOCACHE=/root/.cache/go-build

# --network invarios-dev (docker-compose.yml's network) lets the
# builder container resolve the "registry" compose service by name,
# for BOOT_IMAGE_PUSH_REPO above.
ifeq ($(DOCKER),true)
docker-run = docker run --rm --platform $(PLATFORM) --network invarios-dev $(docker-mounts) $(IMAGE)
else
docker-run =
endif

.PHONY: image build shell clean ovmf boot registry-up registry-down fmt lint vulncheck

image:
	docker pull --platform $(PLATFORM) $(IMAGE)

# Starts the local registry (see docker-compose.yml) that Install's
# boot artifact gets pushed to and pulled back from. --wait blocks
# until the registry's healthcheck passes, so it's actually accepting
# connections by the time this returns.
registry-up:
	REGISTRY_HOST_PORT=$(REGISTRY_HOST_PORT) docker compose up -d --wait registry

registry-down:
	docker compose down -v

build: $(if $(filter true,$(DOCKER)),image) registry-up
	$(docker-run) go run . build \
		--arch=$(GOARCH) \
		--kernel-image=$(KERNEL_IMAGE) \
		--systemd-boot-image=$(SYSTEMD_BOOT_IMAGE) \
		--openbao-version=$(OPENBAO_VERSION) \
		--boot-image-push-repo=$(BOOT_IMAGE_PUSH_REPO) \
		--boot-image-pull-repo=$(BOOT_IMAGE_PULL_REPO) \
		--boot-image-insecure

shell: image
	docker run --rm -it \
		--platform $(PLATFORM) \
		--network invarios-dev \
		$(docker-mounts) \
		$(IMAGE) \
		bash

# Fetches a fresh OVMF (UEFI firmware for QEMU) from Debian, cached
# under .ovmf/. Not part of the shipped appliance; this is dev-only
# tooling so local `make boot` doesn't depend on whatever OVMF vintage
# happens to be bundled with the host's qemu install.
ovmf:
	mkdir -p $(OVMF_DIR)
	docker run --rm -v "$(CURDIR)/$(OVMF_DIR):/out" debian:trixie bash -c '\
		apt-get update -qq && \
		apt-get install -y -qq --no-install-recommends ovmf >/dev/null && \
		cp /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/OVMF/OVMF_VARS_4M.fd /out/'

# TARGET_DISK is deliberately a separate device from the boot media
# (out/invarios.iso, attached below as a CD-ROM): booting the installer
# and the disk it installs onto are the same device on real hardware
# too only once installed, never before. A disk that's ever booted from
# directly while still unpartitioned gets a generic firmware-cached
# boot entry for that raw, filesystem-less state; once Install
# repartitions it, that stale entry no longer resolves but still
# outranks the fresh Boot#### entry Install creates, so the system
# falls through to firmware's own interactive shell instead of booting
# the new install. Real boot media (a USB stick, PXE) is never the
# target disk itself, so it never triggers this; keeping them separate
# here too is what makes this test representative of that.
TARGET_DISK_SIZE := 2G
TARGET_DISK := /tmp/invarios-target-disk.img

# Quick local boot test: serial-only, no Proxmox/USB copy required.
# Install pulls its boot artifact over the network from
# BOOT_IMAGE_PULL_REPO, reachable from the guest via QEMU user-mode
# networking's 10.0.2.2 host alias -- registry-up is a prerequisite so
# it's running regardless of whether `make build` already started it.
# Re-copies OVMF_VARS and recreates TARGET_DISK each run so NVRAM state
# (the Boot#### entry Install creates, etc.) and any previous install
# never carry over between separate `make boot` invocations -- within a
# single run, OVMF vars and TARGET_DISK persist across invarios's own
# internal reboot (install, then straight into boot), since that reboot
# restarts the guest kernel without qemu itself exiting.
boot: $(OVMF_CODE) registry-up
	cp $(OVMF_VARS) /tmp/invarios-ovmf-vars.fd
	rm -f $(TARGET_DISK)
	truncate -s $(TARGET_DISK_SIZE) $(TARGET_DISK)
	qemu-system-x86_64 \
		-machine q35,accel=tcg \
		-m 1G \
		-drive if=pflash,format=raw,readonly=on,file=$(OVMF_CODE) \
		-drive if=pflash,format=raw,file=/tmp/invarios-ovmf-vars.fd \
		-cdrom out/invarios.iso \
		-drive if=none,format=raw,file=$(TARGET_DISK),id=target \
		-device virtio-blk-pci,drive=target \
		-device virtio-rng-pci \
		-netdev user,id=net0,hostfwd=tcp::8200-:8200,hostfwd=tcp::8420-:8420 \
		-device virtio-net-pci,netdev=net0 \
		-nographic

$(OVMF_CODE):
	$(MAKE) ovmf

# Linting

fmt: ; $(info $(M) running gofmt…) @ ## Run gofmt on all source files
	$Q $(GO) fmt $(PKGS)

# Run fmt before any linter so parallel `-jN` doesn't change files while a linter is mid flight.
lint vulncheck: fmt

lint: ; $(info $(M) running golangci-lint…) @ ## Run golangci-lint (vet, revive, staticcheck, errcheck)
	$Q GOOS=$(GOOS) GOARCH=$(GOARCH) golangci-lint run ./...

$(BIN):
	@mkdir -p $@
$(BIN)/%: | $(BIN) ; $(info $(M) building $(PACKAGE)…)
	$Q env GOBIN=$(BIN) $(GO) install $(PACKAGE) \
		|| ret=$$?; \
	   exit $$ret

VULNCHECK = $(BIN)/govulncheck
$(BIN)/govulncheck: PACKAGE=golang.org/x/vuln/cmd/govulncheck@latest

vulncheck: | $(VULNCHECK) ; $(info $(M) running vulncheck…) @
	$Q GOOS=$(GOOS) GOARCH=$(GOARCH) $(VULNCHECK) $(PKGS)

clean:
	rm -rf bin build out
