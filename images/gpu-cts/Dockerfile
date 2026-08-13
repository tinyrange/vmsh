FROM debian:bookworm-slim AS vk-gl-cts-builder

ARG VK_GL_CTS_TAG=opengl-cts-4.6.8.1

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        cmake \
        g++ \
        git \
        libegl1-mesa-dev \
        libgl1-mesa-dev \
        libx11-dev \
        libxext-dev \
        libxrender-dev \
        libxrandr-dev \
        libxss-dev \
        libxxf86vm-dev \
        ninja-build \
        pkg-config \
        python3 \
    && rm -rf /var/lib/apt/lists/*

RUN git clone --depth 1 --branch "${VK_GL_CTS_TAG}" \
        https://github.com/KhronosGroup/VK-GL-CTS.git /src/vk-gl-cts \
    && git -C /src/vk-gl-cts rev-parse HEAD > /src/vk-gl-cts.commit

COPY cts-gl41-harness.patch /tmp/cts-gl41-harness.patch
RUN git -C /src/vk-gl-cts apply /tmp/cts-gl41-harness.patch

RUN python3 /src/vk-gl-cts/external/fetch_sources.py

RUN cmake -S /src/vk-gl-cts -B /build/vk-gl-cts -G Ninja \
        -DCMAKE_BUILD_TYPE=Release \
        -DDE_CPU=DE_CPU_ARM_64 \
        -DDEQP_TARGET=x11_egl \
        -DSELECTED_BUILD_TARGETS="deqp-gles2 deqp-gles3 deqp-gles31 glcts cts-runner" \
    && cmake --build /build/vk-gl-cts --parallel 4

FROM debian:trixie-slim AS mesa-builder

ARG MESA_VERSION=26.1.6
ARG MESA_SHA256=5296b88a0f1e012e2cb9ada150a2bbadf728ca81e5a4fb2ab43c83a4d2158606

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bison \
        build-essential \
        ca-certificates \
        curl \
        flex \
        libdrm-dev \
        libexpat1-dev \
        libx11-dev \
        libx11-xcb-dev \
        libxcb-dri2-0-dev \
        libxcb-dri3-dev \
        libxcb-glx0-dev \
        libxcb-present-dev \
        libxcb-randr0-dev \
        libxcb-shm0-dev \
        libxcb-sync-dev \
        libxcb-xfixes0-dev \
        libxext-dev \
        libxfixes-dev \
        libxrandr-dev \
        libxshmfence-dev \
        libxxf86vm-dev \
        meson \
        ninja-build \
        pkg-config \
        python3-mako \
        python3-packaging \
        python3-yaml \
        xz-utils \
        zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*

RUN install -d -m 0755 /src \
    && curl -fL "https://archive.mesa3d.org/mesa-${MESA_VERSION}.tar.xz" \
        -o /tmp/mesa.tar.xz \
    && echo "${MESA_SHA256}  /tmp/mesa.tar.xz" | sha256sum -c - \
    && tar -C /src -xf /tmp/mesa.tar.xz \
    && mv "/src/mesa-${MESA_VERSION}" /src/mesa \
    && rm /tmp/mesa.tar.xz

RUN meson setup /build/mesa /src/mesa \
        --buildtype=release \
        --prefix=/opt/mesa \
        --libdir=lib \
        -Dbuild-tests=false \
        -Degl=enabled \
        -Dgallium-drivers=virgl \
        -Dgbm=enabled \
        -Dgles1=enabled \
        -Dgles2=enabled \
        -Dglvnd=disabled \
        -Dglx=dri \
        -Dlibunwind=disabled \
        -Dllvm=disabled \
        -Dlmsensors=disabled \
        -Dplatforms=x11 \
        -Dvalgrind=disabled \
        -Dvideo-codecs= \
        -Dvulkan-drivers= \
    && meson compile -C /build/mesa \
    && meson install -C /build/mesa

FROM debian:trixie-slim

ARG VK_GL_CTS_TAG=opengl-cts-4.6.8.1

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        jq \
        libegl1 \
        libegl1-mesa-dev \
        libgl1 \
        libgl1-mesa-dev \
        libgl1-mesa-dri \
        libglx-mesa0 \
        libx11-6 \
        libxext6 \
        libxrandr2 \
        libxrender1 \
        libxss1 \
        libxxf86vm1 \
        mesa-utils \
        procps \
        systemd \
        systemd-sysv \
        util-linux \
        xserver-xorg-core \
        xserver-xorg-input-evdev \
        xserver-xorg-input-libinput \
    && rm -rf /var/lib/apt/lists/* \
    && systemctl mask \
        apt-daily-upgrade.timer \
        apt-daily.timer \
        systemd-networkd-wait-online.service

COPY --from=vk-gl-cts-builder /build/vk-gl-cts/external/openglcts/modules/ /opt/vk-gl-cts/modules/
COPY --from=vk-gl-cts-builder /build/vk-gl-cts/modules/gles2/ /opt/vk-gl-cts/gles2/
COPY --from=vk-gl-cts-builder /build/vk-gl-cts/modules/gles3/ /opt/vk-gl-cts/gles3/
COPY --from=vk-gl-cts-builder /build/vk-gl-cts/modules/gles31/ /opt/vk-gl-cts/gles31/
COPY --from=vk-gl-cts-builder /src/vk-gl-cts.commit /opt/vk-gl-cts/commit
COPY --from=mesa-builder /opt/mesa/ /opt/mesa/
COPY cts-gl41-harness.patch /opt/vk-gl-cts/cts-gl41-harness.patch
COPY xorg.conf /etc/X11/xorg.conf
COPY run-gpu-cts.sh /usr/local/bin/run-gpu-cts
COPY run-gl41-cts.sh /usr/local/bin/run-gl41-cts
COPY vmsh-gpu-cts.service /etc/systemd/system/vmsh-gpu-cts.service

RUN printf '%s\n' "${VK_GL_CTS_TAG}" > /opt/vk-gl-cts/tag \
    && chmod 0755 /usr/local/bin/run-gpu-cts /usr/local/bin/run-gl41-cts \
    && systemctl enable vmsh-gpu-cts.service \
    && ln -sfn /etc/systemd/system/multi-user.target /etc/systemd/system/default.target

CMD ["/sbin/init"]
