#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TARGET_SUFFIX=""
if [[ "$(go env GOOS)" == "windows" ]]; then
  TARGET_SUFFIX=".exe"
fi

"${ROOT_DIR}/tools/build_vmsh.sh" >/dev/null

CCVM_OUTPUT="${ROOT_DIR}/build/vmsh/ccvm${TARGET_SUFFIX}"
VMSH_OUTPUT="${ROOT_DIR}/build/vmsh/vmsh${TARGET_SUFFIX}"

exec "${VMSH_OUTPUT}" -ccvm "${CCVM_OUTPUT}" "$@"
