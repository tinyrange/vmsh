# syntax=docker/dockerfile:1.7

ARG KALI_BASE=docker.io/kalilinux/kali-rolling@sha256:dea2bdf0e8c0ca1deb51b7a6253f481acae3ca9c2f1e2371077e6af55e5b2721

FROM ${KALI_BASE} AS bridge-build

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        build-essential \
        libx11-dev \
    && rm -rf /var/lib/apt/lists/*

COPY glass-clipboard.c glass-resize.c /src/
RUN cc -O2 -Wall -Wextra -o /glass-clipboard /src/glass-clipboard.c -lX11 \
    && cc -O2 -Wall -Wextra -o /glass-resize /src/glass-resize.c

FROM ${KALI_BASE} AS binwalk-build

ARG BINWALK_VERSION=v3.1.0

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        cargo \
        git \
        libfontconfig1-dev \
        pkg-config \
        rustc \
    && git clone --branch "${BINWALK_VERSION}" --depth 1 \
        https://github.com/ReFirmLabs/binwalk.git /src/binwalk \
    && cd /src/binwalk \
    && cargo build --release --locked

FROM ${KALI_BASE} AS pwninit-build

ARG PWNINIT_COMMIT=0c123dd79c2ebfc1dca68aeec0dc2b840c066c32

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        cargo \
        git \
        liblzma-dev \
        libssl-dev \
        pkg-config \
        rustc \
    && git clone --filter=blob:none https://github.com/io12/pwninit.git /src/pwninit \
    && cd /src/pwninit \
    && git checkout "${PWNINIT_COMMIT}" \
    && cargo build --release --locked

FROM ${KALI_BASE}

ARG TARGETARCH
ARG SQUADVM_USER=squad
ARG SQUADVM_UID=1000
ARG PWNDBG_COMMIT=73c6ec27f91828408fa28e5604445bc3293d2d9b
ARG ZSTEG_VERSION=0.2.14
ARG CYBER_SQUAD_LOGO_SHA256=6d0285eb4427f1ddf430fca774c31a3e3a39aeb3c3148eabd2b58a0445c19724

ENV DEBIAN_FRONTEND=noninteractive

COPY packages.txt /tmp/squadvm-packages.txt
RUN echo "wireshark-common wireshark-common/install-setuid boolean true" | debconf-set-selections \
    && if [ "${TARGETARCH}" = "arm64" ]; then \
        dpkg --add-architecture amd64; \
    fi \
    && apt-get update \
    && sed -e '/^[[:space:]]*#/d' -e '/^[[:space:]]*$/d' /tmp/squadvm-packages.txt \
        | xargs apt-get install -y --no-install-recommends \
    && if [ "${TARGETARCH}" = "arm64" ]; then \
        apt-get install -y --no-install-recommends \
            qemu-user-binfmt \
            gdb-multiarch \
            libc6:amd64 \
            libgcc-s1:amd64 \
            libstdc++6:amd64 \
            zlib1g:amd64 \
        && ln -sfn /usr/bin/gdb-multiarch /usr/local/bin/gdb; \
    fi \
    && rm -f /tmp/squadvm-packages.txt \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/*

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        libglib2.0-bin \
        python3.14 \
        python3.14-dev \
        python3.14-venv \
    && git clone --filter=blob:none https://github.com/pwndbg/pwndbg.git /opt/pwndbg \
    && cd /opt/pwndbg \
    && git checkout "${PWNDBG_COMMIT}" \
    && PWNDBG_VENV_PATH=/opt/pwndbg/.venv PWNDBG_NO_AUTOUPDATE=1 ./setup.sh \
    && sha256sum uv.lock | cut -d ' ' -f 1 > /opt/pwndbg/.venv/uv.lock.hash \
    && rm -f /root/.gdbinit \
    && gem install --no-document --version "${ZSTEG_VERSION}" zsteg \
    && chown -R "${SQUADVM_UID}:${SQUADVM_UID}" /opt/pwndbg \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/*

ENV PWNDBG_NO_AUTOUPDATE=1

RUN useradd --create-home --uid "${SQUADVM_UID}" --shell /bin/bash "${SQUADVM_USER}" \
    && usermod -aG sudo,wireshark "${SQUADVM_USER}" \
    && printf '%s ALL=(ALL:ALL) NOPASSWD: ALL\n' "${SQUADVM_USER}" > "/etc/sudoers.d/${SQUADVM_USER}" \
    && chmod 0440 "/etc/sudoers.d/${SQUADVM_USER}" \
    && if [ -x /usr/bin/dumpcap ]; then \
        setcap cap_net_raw,cap_net_admin=eip /usr/bin/dumpcap; \
    fi \
    && : > /etc/machine-id

COPY --from=bridge-build /glass-clipboard /usr/local/bin/glass-clipboard
COPY --from=bridge-build /glass-resize /usr/local/bin/glass-resize
COPY --from=binwalk-build /src/binwalk/target/release/binwalk /usr/local/bin/binwalk
COPY --from=pwninit-build /src/pwninit/target/release/pwninit /usr/local/bin/pwninit
COPY squadvm-burpsuite /usr/local/bin/squadvm-burpsuite
COPY qemu-gdb /tmp/qemu-gdb
COPY squadvm-session /usr/local/sbin/squadvm-session
COPY squadvm-sshd-prepare /usr/local/sbin/squadvm-sshd-prepare
COPY squadvm-binfmt.service /etc/systemd/system/squadvm-binfmt.service
COPY squadvm-desktop.service /etc/systemd/system/squadvm-desktop.service
COPY squadvm-systemd.conf /etc/systemd/system.conf.d/squadvm-console.conf
COPY squadvm-sshd.service /etc/systemd/system/squadvm-sshd.service
COPY squadvm-sshd_config /etc/ssh/squadvm-sshd_config
COPY squadvm.target /etc/systemd/system/squadvm.target
COPY xorg.conf /etc/X11/xorg.conf
COPY squadvm-wallpaper.svg squadvm-icon.svg /usr/share/squadvm/
COPY cyber-squad-special.png /usr/share/squadvm/
COPY skel/ /usr/share/squadvm/skel/
COPY firefox-policies.json /usr/lib/firefox-esr/distribution/policies.json

RUN curl --fail --location --silent --show-error \
        https://cybersquad.uqcloud.net/logo.svg \
        --output /tmp/cyber-squad-logo.svg \
    && printf '%s  %s\n' "${CYBER_SQUAD_LOGO_SHA256}" /tmp/cyber-squad-logo.svg \
        | sha256sum --check --strict - \
    && sed 's/#5F17EE/#FFFFFF/g' /tmp/cyber-squad-logo.svg \
        > /usr/share/squadvm/cyber-squad-logo.svg \
    && logo_data="$(base64 -w 0 /usr/share/squadvm/cyber-squad-logo.svg)" \
    && sed "s|cyber-squad-logo.svg|data:image/svg+xml;base64,${logo_data}|g" \
        /usr/share/squadvm/squadvm-wallpaper.svg \
        > /tmp/squadvm-wallpaper.svg \
    && mv /tmp/squadvm-wallpaper.svg /usr/share/squadvm/squadvm-wallpaper.svg \
    && rsvg-convert --width 1920 --height 1080 \
        --output /usr/share/squadvm/squadvm-wallpaper.png \
        /usr/share/squadvm/squadvm-wallpaper.svg \
    && install -d /usr/share/icons/hicolor/256x256/apps \
        /usr/share/icons/hicolor/128x128/apps \
        /usr/share/icons/hicolor/64x64/apps \
    && install -m 0644 /usr/share/squadvm/cyber-squad-special.png \
        /usr/share/icons/hicolor/128x128/apps/cyber-squad.png \
    && rsvg-convert --width 256 --height 256 \
        --output /usr/share/icons/hicolor/256x256/apps/squadvm.png \
        /usr/share/squadvm/squadvm-icon.svg \
    && unzip -p /usr/share/burpsuite/burpsuite.jar \
        resources/Browser/NewTab/images/icon-128.png \
        > /usr/share/icons/hicolor/128x128/apps/squadvm-burpsuite.png \
    && install -m 0644 \
        /usr/share/doc/ghidra/GhidraClass/AdvancedDevelopment/Images/GhidraLogo64.png \
        /usr/share/icons/hicolor/64x64/apps/squadvm-ghidra.png \
    && gtk-update-icon-cache --force /usr/share/icons/hicolor \
    && update-desktop-database /usr/share/applications \
    && rm -f /tmp/cyber-squad-logo.svg \
    && chmod 0755 \
        /usr/local/bin/glass-clipboard \
        /usr/local/bin/glass-resize \
        /usr/local/bin/squadvm-burpsuite \
        /usr/local/bin/binwalk \
        /usr/local/bin/pwninit \
        /usr/local/sbin/squadvm-sshd-prepare \
        /usr/local/sbin/squadvm-session \
    && if [ "${TARGETARCH}" = "arm64" ]; then \
        install -m 0755 /tmp/qemu-gdb /usr/local/bin/qemu-gdb; \
    fi \
    && rm -f /tmp/qemu-gdb \
    && chmod 0775 \
        /var/lib/AccountsService \
        /var/lib/AccountsService/icons \
    && chmod 0700 /var/lib/AccountsService/users \
    && install -d -m 0755 /etc/systemd/system \
    && rm -f /etc/ssh/ssh_host_* \
    && (systemctl disable ssh.service ssh.socket 2>/dev/null || true)

RUN systemctl enable squadvm-desktop.service squadvm-sshd.service \
    && ln -sfn /etc/systemd/system/squadvm.target /etc/systemd/system/default.target \
    && systemctl mask \
        apt-daily-upgrade.timer \
        apt-daily.timer \
        systemd-networkd-wait-online.service \
    && install -d -o "${SQUADVM_UID}" -g "${SQUADVM_UID}" -m 0700 "/home/${SQUADVM_USER}/.config" \
    && cp -a /usr/share/squadvm/skel/. "/home/${SQUADVM_USER}/" \
    && chmod 0755 "/home/${SQUADVM_USER}/Desktop/"*.desktop /usr/share/squadvm/skel/Desktop/*.desktop \
    && desktop-file-validate "/home/${SQUADVM_USER}/Desktop/"*.desktop \
    && chown -R "${SQUADVM_UID}:${SQUADVM_UID}" "/home/${SQUADVM_USER}" \
    && test -z "$(find /opt/pwndbg ! -user "${SQUADVM_USER}" -print -quit)" \
    && runuser -u "${SQUADVM_USER}" -- test -r /opt/pwndbg/gdbinit.py \
    && runuser -u "${SQUADVM_USER}" -- test -x /opt/pwndbg/.venv/bin/python \
    && test "$(sha256sum /opt/pwndbg/uv.lock | cut -d ' ' -f 1)" = "$(cat /opt/pwndbg/.venv/uv.lock.hash)" \
    && runuser -u "${SQUADVM_USER}" -- /usr/local/bin/pwninit --version \
    && if [ "${TARGETARCH}" = "arm64" ]; then \
        dpkg --print-foreign-architectures | grep -Fx amd64 \
        && test -x /usr/bin/qemu-x86_64 \
        && test "$(readlink -f /usr/local/bin/gdb)" = /usr/bin/gdb-multiarch \
        && test -x /usr/local/bin/qemu-gdb \
        && bash -n /usr/local/bin/qemu-gdb \
        && /usr/local/bin/gdb --batch --quiet -ex 'set architecture i386:x86-64' -ex quit \
        && test -f /usr/lib/binfmt.d/qemu-x86_64.conf \
        && test -x /usr/lib/systemd/systemd-binfmt \
        && test -x /lib/x86_64-linux-gnu/ld-linux-x86-64.so.2 \
        && test "$(dpkg-query -W -f='${Architecture}' libc6:amd64)" = amd64 \
        && test "$(dpkg-query -W -f='${Architecture}' libgcc-s1:amd64)" = amd64 \
        && test "$(dpkg-query -W -f='${Architecture}' libstdc++6:amd64)" = amd64 \
        && test "$(dpkg-query -W -f='${Architecture}' zlib1g:amd64)" = amd64; \
    fi

ENV USER=${SQUADVM_USER}
ENV HOME=/home/${SQUADVM_USER}
WORKDIR /home/${SQUADVM_USER}

CMD ["/sbin/init"]
