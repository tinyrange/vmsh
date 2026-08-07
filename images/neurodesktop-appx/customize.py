#!/usr/bin/env python3
"""Remove nested remote-desktop integration from the AppX image."""

import ast
from pathlib import Path
import sys


def remove_section(text: str, start_marker: str, end_marker: str) -> str:
    start = text.find(start_marker)
    if start < 0:
        return text
    end = text.find(end_marker, start)
    if end < 0:
        raise RuntimeError(f"could not find {end_marker!r} after {start_marker!r}")
    return text[:start] + text[end:]


def customize_jupyter_config(path: Path) -> None:
    text = path.read_text()

    credential_start = text.find("# Per-user Guacamole web credentials.")
    if credential_start >= 0:
        credential_end = text.find("\n\n", text.find("_guac_basic =", credential_start))
        if credential_end < 0:
            raise RuntimeError("could not locate the end of the Guacamole credential block")
        text = text[:credential_start] + text[credential_end + 2 :]

    tree = ast.parse(text, filename=str(path))
    remote_names = {"neurodesktop", "neurodesktop-rdp", "neurodesktop-vnc"}
    unused_helpers = {"_is_apptainer_runtime", "_rdp_launcher_enabled", "_neurodesktop_server"}

    for node in tree.body:
        if not isinstance(node, ast.Assign) or not isinstance(node.value, ast.Dict):
            continue
        if not any(
            isinstance(target, ast.Attribute)
            and target.attr == "servers"
            and isinstance(target.value, ast.Attribute)
            and target.value.attr == "ServerProxy"
            for target in node.targets
        ):
            continue
        kept = [
            (key, value)
            for key, value in zip(node.value.keys, node.value.values)
            if not (isinstance(key, ast.Constant) and key.value in remote_names)
        ]
        node.value.keys = [key for key, _ in kept]
        node.value.values = [value for _, value in kept]

    tree.body = [
        node
        for node in tree.body
        if not (
            isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
            and node.name in unused_helpers
        )
    ]
    ast.fix_missing_locations(tree)
    updated = ast.unparse(tree) + "\n"
    compile(updated, str(path), "exec")
    path.write_text(updated)


def customize_startup(path: Path) -> None:
    text = path.read_text()
    text = remove_section(
        text,
        "# Initialize per-user Guacamole config",
        "is_apptainer_runtime() {",
    )
    text = remove_section(
        text,
        "# Pre-generate the SSH keypairs guacamole.sh needs",
        "# Create a symlink in home if /data is mounted",
    )
    text = remove_section(
        text,
        "# Setup VNC directory and ensure files exist",
        'touch "$STARTUP_DONE_FILE"',
    )
    text = text.replace(
        "# SSH key generation, guacamole mapping injection, and SSH/SFTP daemon startup\n"
        "# are handled on-demand by guacamole.sh when the desktop is opened.\n",
        "",
    )
    path.write_text(text)


def customize_before_notebook(path: Path) -> None:
    if not path.exists():
        return
    text = path.read_text()
    text = text.replace(
        "# RDP backend is started on-demand by guacamole.sh when the desktop is opened.\n\n",
        "",
    )
    text = remove_section(
        text,
        "# Ensure the VNC password file has the correct permissions",
        "apply_chown_if_needed() {",
    )
    text = text.replace('apply_chown_if_needed "/etc/guacamole"\n', "")
    text = text.replace('apply_chown_if_needed "/usr/local/tomcat"\n', "")
    path.write_text(text)


def prevent_vnc_restore(path: Path) -> None:
    text = path.read_text()
    marker = '        rel_path="${src_file#${DEFAULTS_DIR}/}"\n'
    replacement = marker + '        case "$rel_path" in .vnc/*) continue ;; esac\n'
    if replacement in text:
        return
    if marker not in text:
        raise RuntimeError("could not locate the restored-home relative path")
    path.write_text(text.replace(marker, replacement, 1))


root = Path(sys.argv[1]) if len(sys.argv) > 1 else Path("/")


def rooted(path: str) -> Path:
    return root / path.removeprefix("/")


customize_jupyter_config(rooted("/etc/jupyter/jupyter_notebook_config.py"))
customize_startup(rooted("/opt/neurodesktop/jupyterlab_startup.sh"))
customize_before_notebook(rooted("/usr/local/bin/before-notebook.d/before_notebook.sh"))
prevent_vnc_restore(rooted("/opt/neurodesktop/restore_home_defaults.sh"))
