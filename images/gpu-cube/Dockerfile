FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        kmscube \
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

COPY start-cube.sh /usr/local/bin/start-cube
COPY vmsh-gpu-cube.service /etc/systemd/system/vmsh-gpu-cube.service

RUN chmod 0755 /usr/local/bin/start-cube \
    && systemctl enable vmsh-gpu-cube.service

CMD ["/sbin/init"]
