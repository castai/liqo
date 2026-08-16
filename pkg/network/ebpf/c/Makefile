# SPDX-License-Identifier: Apache-2.0
# Copyright 2019-2026 The Liqo Authors

# Self-contained Makefile for the PoC eBPF TC programs.
# It can be invoked directly from this directory or through the root
# `make ebpf` target.

CLANG ?= clang
LLC ?= llc
ARCH ?= $(shell uname -m | sed -e 's/x86_64/x86/' -e 's/aarch64/arm64/' -e 's/arm64/arm64/')

# Try to locate usable kernel headers.  Distro packages sometimes split the
# architecture-independent source tree and the generated architecture headers
# into two directories (e.g., Ubuntu's linux-headers-<version> and
# linux-headers-<version>-generic).  Allow explicit override, then fall back
# to the running kernel's build directory, then scan /usr/src.
KERNEL_HEADERS ?= /lib/modules/$(shell uname -r)/build
KERNEL_HEADERS_COMMON ?= $(shell \
	if [ -n "$(KERNEL_HEADERS)" ] && [ -d "$(KERNEL_HEADERS)/include" ]; then \
		echo "$(KERNEL_HEADERS)"; \
	else \
		ls -d /usr/src/linux-headers-[0-9]* 2>/dev/null | grep -v generic | head -n1; \
	fi)
KERNEL_HEADERS_GENERATED ?= $(shell \
	if [ -d "$(KERNEL_HEADERS)/arch/$(ARCH)/include/generated" ]; then \
		echo "$(KERNEL_HEADERS)/arch/$(ARCH)/include/generated"; \
	else \
		ls -d /usr/src/linux-headers-[0-9]*-generic 2>/dev/null | head -n1 | xargs -I{} echo {}/arch/$(ARCH)/include/generated; \
	fi)

# Common verifier-friendly flags.  -O2 keeps the verifier happy, -g emits BTF.
# KERNEL_HEADERS may point to either a full kernel tree or a distro split tree
# (e.g., /usr/src/linux-headers-<version> with generated headers in a
# separate -generic directory).  Provide sensible defaults and allow override.

BPF_CFLAGS := -O2 -g -target bpf -D__TARGET_ARCH_$(ARCH) -D__KERNEL__ -DKBUILD_MODNAME=\"liqo_poc\"
# Use the full kernel headers (UAPI + generated + internal).  Defining
# __KERNEL__ lets the sanitized UAPI headers use the generated autoconf.h
# CONFIG_* values and compiler_types.h shipped with the distro headers.
BPF_CFLAGS += -I$(KERNEL_HEADERS_COMMON)/include/uapi
BPF_CFLAGS += -I$(KERNEL_HEADERS_COMMON)/arch/$(ARCH)/include/uapi
BPF_CFLAGS += -I$(KERNEL_HEADERS_GENERATED)/uapi
BPF_CFLAGS += -I$(KERNEL_HEADERS_GENERATED)
BPF_CFLAGS += -I$(KERNEL_HEADERS_COMMON)/include
BPF_CFLAGS += -I$(KERNEL_HEADERS_COMMON)/arch/$(ARCH)/include
BPF_CFLAGS += -I.
BPF_CFLAGS += -Wall -Werror

TARGETS := tc_pod_encap.o tc_gw_return.o tc_gw_forward.o tc_geneve_rx.o

.PHONY: all clean

all: $(TARGETS)

%.o: %.c common.h
	$(CLANG) $(BPF_CFLAGS) -c $< -o $@

clean:
	rm -f $(TARGETS)
