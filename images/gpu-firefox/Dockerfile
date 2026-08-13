FROM debian:bookworm-slim AS webgl-cts

ARG WEBGL_CTS_COMMIT=f15a73f727d8ee66a3ec6d0bed9f02f08cacc2c1
ARG WEBGL_CTS_SHA256=23372bd002751674143b18c0615f861f6e91e377f9fa586ecf0ec3b0a0d5ff2b

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
    && curl --fail --location --silent --show-error \
        "https://github.com/KhronosGroup/WebGL/archive/${WEBGL_CTS_COMMIT}.tar.gz" \
        --output /tmp/webgl-cts.tar.gz \
    && printf '%s  %s\n' "${WEBGL_CTS_SHA256}" /tmp/webgl-cts.tar.gz \
        | sha256sum --check --strict - \
    && mkdir -p /src/WebGL \
    && tar --extract --gzip --file /tmp/webgl-cts.tar.gz \
        --strip-components=1 --directory /src/WebGL

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
COPY --from=webgl-cts /src/WebGL/sdk/tests/ /usr/share/vmsh-webgl/cts/
COPY patch-webgl-cts.py /usr/local/lib/vmsh-webgl/patch-webgl-cts.py
RUN python3 /usr/local/lib/vmsh-webgl/patch-webgl-cts.py \
        /usr/share/vmsh-webgl/cts/webgl-conformance-tests.html \
    && rm /usr/local/lib/vmsh-webgl/patch-webgl-cts.py
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
