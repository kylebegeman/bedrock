FROM ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55
LABEL io.bedrock.lane.provider="qemu-ubuntu-24.04"
ARG TARGETARCH
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      qemu-system-arm qemu-system-x86 qemu-efi-aarch64 qemu-utils cloud-image-utils curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN set -eu; image="ubuntu-24.04-server-cloudimg-${TARGETARCH}.img"; \
    curl --fail --location --retry 3 "https://cloud-images.ubuntu.com/releases/noble/release/$image" -o /opt/ubuntu.img; \
    curl --fail --location --retry 3 'https://cloud-images.ubuntu.com/releases/noble/release/SHA256SUMS' -o /opt/SHA256SUMS; \
    awk -v image="$image" '$2 == image || $2 == "*"image {print $1 "  /opt/ubuntu.img"}' /opt/SHA256SUMS > /opt/ubuntu.sha256; \
    test -s /opt/ubuntu.sha256; sha256sum -c /opt/ubuntu.sha256; \
    printf '%s\n' "$TARGETARCH" > /opt/guest-arch
WORKDIR /work
COPY boot.sh /opt/boot.sh
ENTRYPOINT ["/bin/sh", "/opt/boot.sh"]
