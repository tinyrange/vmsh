FROM debian:bookworm-slim

ARG OPENARENA_VERSION=0.8.8+dfsg-6
ARG BENCHMARK_WIDTH=1440
ARG BENCHMARK_HEIGHT=900
ARG OPENARENA_DEMO_BYTES=389544

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        dbus \
        libgl1-mesa-dri \
        libglx-mesa0 \
        mesa-utils \
        openarena="${OPENARENA_VERSION}" \
        openarena-data \
        procps \
        systemd \
        systemd-sysv \
        util-linux \
        x11-xserver-utils \
        xserver-xorg-core \
        xserver-xorg-input-evdev \
        xserver-xorg-input-libinput \
    && rm -rf /var/lib/apt/lists/* \
    && systemctl mask \
        apt-daily-upgrade.timer \
        apt-daily.timer \
        systemd-networkd-wait-online.service

RUN apt-get update \
    && apt-get install -y --no-install-recommends unzip \
    && rm -rf /var/lib/apt/lists/*

RUN useradd --create-home --uid 1000 --shell /bin/sh arena \
    && usermod -aG audio,input,video arena \
    && install -d -o arena -g arena -m 0700 \
        /home/arena/.openarena/baseoa \
        /home/arena/.openarena/baseoa/demos \
        /home/arena/.config

RUN unzip -p \
        /usr/share/games/openarena/baseoa/pak6-patch088.pk3 \
        demos/demo088-test1.dm_71 \
        | head -c "${OPENARENA_DEMO_BYTES}" \
        > /home/arena/.openarena/baseoa/demos/openarena-benchmark.dm_71 \
    && printf '\377\377\377\377\377\377\377\377' \
        >> /home/arena/.openarena/baseoa/demos/openarena-benchmark.dm_71 \
    && test "$(wc -c \
        < /home/arena/.openarena/baseoa/demos/openarena-benchmark.dm_71)" \
        -eq "$((OPENARENA_DEMO_BYTES + 8))" \
    && chown arena:arena \
        /home/arena/.openarena/baseoa/demos/openarena-benchmark.dm_71

COPY xorg.conf /etc/X11/xorg.conf
COPY autoexec.cfg /home/arena/.openarena/baseoa/autoexec.cfg
COPY start-openarena.sh /usr/local/bin/start-openarena
COPY vmsh-gpu-openarena.service /etc/systemd/system/vmsh-gpu-openarena.service

RUN sed -i \
        -e "s/@BENCHMARK_WIDTH@/${BENCHMARK_WIDTH}/g" \
        -e "s/@BENCHMARK_HEIGHT@/${BENCHMARK_HEIGHT}/g" \
        /home/arena/.openarena/baseoa/autoexec.cfg \
    && chown arena:arena /home/arena/.openarena/baseoa/autoexec.cfg \
    && chmod 0755 /usr/local/bin/start-openarena \
    && systemctl enable vmsh-gpu-openarena.service \
    && ln -sfn /etc/systemd/system/multi-user.target /etc/systemd/system/default.target

CMD ["/sbin/init"]
