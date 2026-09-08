GO       := go
GOOS     ?= linux
GOARCH   ?= amd64
PKGS      = $(or $(PKG),$(shell $(GO) list ./...))
BIN       = $(CURDIR)/bin
V         = 0
Q         = $(if $(filter 1,$V),,@)
M         = $(shell printf "\033[34;1m▶\033[0m")

IMAGE := ghcr.io/invarios/pkgs/builder:main
PLATFORM := $(GOOS)/$(GOARCH)
KERNEL_IMAGE := ghcr.io/invarios/pkgs/kernel:6.18.49-amd64
SYSTEMD_BOOT_IMAGE := ghcr.io/invarios/pkgs/systemd-boot:261.2-amd64
OPENBAO_VERSION := 2.6.2
OVMF_DIR := .ovmf
OVMF_CODE := $(OVMF_DIR)/OVMF_CODE_4M.fd
OVMF_VARS := $(OVMF_DIR)/OVMF_VARS_4M.fd

# The build depends on Linux-only tooling (cmd/build.go shells out to
# mkfs.fat/mcopy). On Linux it can therefore run directly on the host;
# everywhere else it needs the builder container. DOCKER=true forces
# the container path on Linux too.
UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Linux)
DOCKER ?= false
else
DOCKER ?= true
endif

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

ifeq ($(DOCKER),true)
docker-run = docker run --rm --platform $(PLATFORM) $(docker-mounts) $(IMAGE)
else
docker-run =
endif

.PHONY: image build shell clean ovmf boot fmt lint vulncheck

image:
	docker pull --platform $(PLATFORM) $(IMAGE)

build: $(if $(filter true,$(DOCKER)),image)
	$(docker-run) go run . build \
		--kernel-image=$(KERNEL_IMAGE) \
		--systemd-boot-image=$(SYSTEMD_BOOT_IMAGE) \
		--openbao-version=$(OPENBAO_VERSION)

shell: image
	docker run --rm -it \
		--platform $(PLATFORM) \
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

# Size `make boot`'s throwaway test disk is grown to before booting it.
# out/invarios-efi.img itself (espImageSize in cmd/build.go) only needs
# to hold systemd-boot + one UKI + loader.conf (~128 MiB) -- real
# installer media (a USB stick, an ISO's El Torito image) installs onto
# a *separate* target disk, so it never needs to be bigger than that.
# `make boot`'s self-install-in-place loop is the one case where the
# boot media and the install target are the same file, so it alone
# needs headroom for the post-install GPT layout (ESP+META+STATE+DATA,
# sized in internal/disk) to fit once Install repartitions it.
BOOT_DISK_SIZE := 2G
BOOT_DISK := /tmp/invarios-boot-disk.img

# Quick local boot test: serial-only, no Proxmox/USB copy required.
# Re-copies OVMF_VARS and the boot disk each run so NVRAM state (boot
# attempts, the Boot#### entry Install creates, etc.) and any previous
# install never carry over between separate `make boot` invocations --
# within a single run, the same disk and OVMF vars persist across
# invarios's own internal reboot (install, then straight into boot),
# since that reboot restarts the guest kernel without qemu itself
# exiting.
boot: $(OVMF_CODE)
	cp $(OVMF_VARS) /tmp/invarios-ovmf-vars.fd
	cp out/invarios-efi.img $(BOOT_DISK)
	truncate -s $(BOOT_DISK_SIZE) $(BOOT_DISK)
	qemu-system-x86_64 \
		-machine q35,accel=tcg \
		-m 1G \
		-drive if=pflash,format=raw,readonly=on,file=$(OVMF_CODE) \
		-drive if=pflash,format=raw,file=/tmp/invarios-ovmf-vars.fd \
		-drive if=none,format=raw,file=$(BOOT_DISK),id=bootdisk \
		-device virtio-blk-pci,drive=bootdisk,bootindex=1 \
		-device virtio-rng-pci \
		-netdev user,id=net0,hostfwd=tcp::8200-:8200 \
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
