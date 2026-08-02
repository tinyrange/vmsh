FROM debian:bookworm-slim

ARG FIREFOX_VERSION=140.13.0esr-1~deb12u1

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        dbus \
        firefox-esr="${FIREFOX_VERSION}" \
        fonts-dejavu-core \
        libgl1-mesa-dri \
        libglx-mesa0 \
        mesa-utils \
        openbox \
        procps \
        python3 \
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

RUN useradd --create-home --uid 1000 --shell /bin/sh webgl \
    && usermod -aG audio,input,video webgl \
    && install -d -o webgl -g webgl -m 0700 \
        /home/webgl/.config/openbox \
        /home/webgl/.mozilla/firefox-vmsh

COPY xorg.conf /etc/X11/xorg.conf
COPY firefox-policies.json /usr/lib/firefox-esr/distribution/policies.json
COPY firefox-user.js /home/webgl/.mozilla/firefox-vmsh/user.js
COPY openbox-rc.xml /home/webgl/.config/openbox/rc.xml
COPY index.html demo.js /usr/share/vmsh-webgl/
COPY telemetry-server.py /usr/local/lib/vmsh-webgl/telemetry-server.py
COPY start-firefox-webgl.sh /usr/local/bin/start-firefox-webgl
COPY vmsh-gpu-firefox.service /etc/systemd/system/vmsh-gpu-firefox.service

RUN chown -R webgl:webgl /home/webgl \
    && chmod 0755 \
        /usr/local/bin/start-firefox-webgl \
        /usr/local/lib/vmsh-webgl/telemetry-server.py \
    && systemctl enable vmsh-gpu-firefox.service \
    && ln -sfn /etc/systemd/system/multi-user.target /etc/systemd/system/default.target

CMD ["/sbin/init"]
