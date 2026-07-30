FROM debian:bookworm-slim AS builder

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        g++ \
        git \
        libdrm-dev \
        libegl1-mesa-dev \
        libgbm-dev \
        libgles2-mesa-dev \
        libjpeg-dev \
        libpng-dev \
        libudev-dev \
        meson \
        ninja-build \
        pkg-config \
    && rm -rf /var/lib/apt/lists/*

RUN git clone --depth 1 --branch 2023.01 https://github.com/glmark2/glmark2.git /src/glmark2
COPY preferred-virtio-mode.patch /src/glmark2/preferred-virtio-mode.patch
RUN cd /src/glmark2 \
    && git apply preferred-virtio-mode.patch \
    && meson setup build \
        --prefix=/usr \
        -Ddata-path=/usr/share/glmark2 \
        -Dflavors=drm-glesv2 \
    && ninja -C build

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        glmark2-es2-drm \
        libgl1-mesa-dri \
        mesa-utils \
        socat \
        systemd \
        systemd-sysv \
    && rm -rf /var/lib/apt/lists/* \
    && systemctl mask \
        apt-daily-upgrade.timer \
        apt-daily.timer \
        systemd-networkd-wait-online.service

COPY start-glmark2.sh /usr/local/bin/start-glmark2
COPY vmsh-gpu-glmark2.service /etc/systemd/system/vmsh-gpu-glmark2.service
COPY --from=builder /src/glmark2/build/src/glmark2-es2-drm /usr/local/bin/glmark2-es2-drm

RUN chmod 0755 /usr/local/bin/start-glmark2 \
    && systemctl enable vmsh-gpu-glmark2.service

CMD ["/sbin/init"]
