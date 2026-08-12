FROM ubuntu:24.04

ARG DEBIAN_FRONTEND=noninteractive
ENV TZ=UTC

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        make clang libbpf-dev linux-headers-generic linux-tools-generic && \
    rm -rf /var/lib/apt/lists/*

# The Ubuntu bpftool wrapper at /usr/sbin/bpftool refuses to run when the
# running kernel version does not match an installed linux-tools package.
# For reproducible local verification inside containers (and CI runners that
# may have a different kernel), expose the matching package binary directly.
RUN set -e; \
    bpftool_bin="$(find /usr/lib/linux-tools -name bpftool \( -type f -o -type l \) -print -quit 2>/dev/null)"; \
    [ -n "$bpftool_bin" ] || { echo 'bpftool binary not found'; exit 1; }; \
    ln -sf "$bpftool_bin" /usr/local/bin/bpftool; \
    /usr/local/bin/bpftool --version

# The eBPF Makefile auto-detects the installed kernel header layout (Ubuntu
# splits architecture-independent source and generated architecture headers
# into two /usr/src/linux-headers-* directories), so no explicit ENV is needed.

WORKDIR /src/pkg/network/ebpf/c
ENTRYPOINT ["make"]
