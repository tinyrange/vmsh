#!/usr/bin/env bash
set -euo pipefail

assets=${1:?asset directory is required}
service_name=neurodeskappx-jupyter.service

python3 "$assets/customize.py"

install -D -m 0644 "$assets/$service_name" "/etc/systemd/system/$service_name"
install -D -m 0755 "$assets/neurodeskappx-start-jupyter" /usr/local/bin/neurodeskappx-start-jupyter
install -D -m 0755 "$assets/neurodeskappx-prepare" /usr/local/sbin/neurodeskappx-prepare
install -D -m 0755 "$assets/jupyterlab.desktop" /usr/share/neurodeskappx/jupyterlab.desktop

cat > /etc/sudoers.d/neurodeskappx <<'EOF'
jovyan ALL=(ALL) NOPASSWD: ALL
EOF
chmod 0440 /etc/sudoers.d/neurodeskappx
if command -v visudo >/dev/null 2>&1; then
    visudo -cf /etc/sudoers.d/neurodeskappx >/dev/null
fi

install -d -m 0755 /etc/systemd/system/neurodesktop-glass.service.d
cat > /etc/systemd/system/neurodesktop-glass.service.d/neurodeskappx.conf <<'EOF'
[Service]
ExecStartPre=/usr/local/sbin/neurodeskappx-prepare
EOF

for unit in xrdp.service xrdp-sesman.service tigervncserver@.service guacd.service; do
    find /etc/systemd/system -type l -name "$unit" -delete
    ln -sfn /dev/null "/etc/systemd/system/$unit"
done

if [ -d /opt/jovyan_defaults/.vnc ]; then
    find /opt/jovyan_defaults/.vnc -depth -delete
fi

bash -n /usr/local/bin/neurodeskappx-start-jupyter
bash -n /usr/local/sbin/neurodeskappx-prepare
python3 -c 'compile(open("/etc/jupyter/jupyter_notebook_config.py").read(), "/etc/jupyter/jupyter_notebook_config.py", "exec")'
if command -v desktop-file-validate >/dev/null 2>&1; then
    desktop-file-validate /usr/share/neurodeskappx/jupyterlab.desktop
fi
if command -v systemd-analyze >/dev/null 2>&1; then
    systemd-analyze verify "/etc/systemd/system/$service_name"
fi

touch /usr/share/neurodeskappx/image-ready
