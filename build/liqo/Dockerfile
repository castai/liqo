
# syntax=docker/dockerfile:1
FROM alpine:3.20
ARG COMPONENT
ARG TARGETARCH

RUN set -e; \
    if [ "$COMPONENT" = "geneve" ] || [ "$COMPONENT" = "wireguard" ] || [ "$COMPONENT" = "gateway" ] || [ "$COMPONENT" = "fabric" ]; then \
        set -x; \
        apk add --no-cache iproute2 nftables bash tcpdump conntrack-tools curl iputils; \
    fi; \
    if [ "$COMPONENT" = "wireguard" ]; then \
        set -x; \
        apk add --no-cache wireguard-tools; \
    fi

# Copy the correct binary for the architecture
COPY ./bin/${TARGETARCH}/${COMPONENT}_linux_${TARGETARCH} /usr/bin/${COMPONENT}
RUN chmod +x /usr/bin/${COMPONENT}
RUN ln -s /usr/bin/${COMPONENT} /usr/bin/liqo-component

WORKDIR /workspace

ENTRYPOINT ["/usr/bin/liqo-component"]
