#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CC_DIR="${ROOT_DIR}/cc"
BUILD_DIR="${ROOT_DIR}/build/vmsh"
TARGET_GOOS="$(go env GOOS)"
TARGET_SUFFIX=""
if [[ "${TARGET_GOOS}" == "windows" ]]; then
  TARGET_SUFFIX=".exe"
fi

CC_OUTPUT="${BUILD_DIR}/cc${TARGET_SUFFIX}"
CCVM_OUTPUT="${BUILD_DIR}/ccvm${TARGET_SUFFIX}"
VMSH_OUTPUT="${BUILD_DIR}/vmsh${TARGET_SUFFIX}"
GUESTINIT_ARM64_EMBED_PATH="${CC_DIR}/internal/guestinit/guest-init-linux-arm64"
GUESTINIT_AMD64_EMBED_PATH="${CC_DIR}/internal/guestinit/guest-init-linux-amd64"

mkdir -p "${BUILD_DIR}"

(
  cd "${CC_DIR}"
  GOOS=linux GOARCH=arm64 go build -o "${BUILD_DIR}/init-linux-arm64" ./internal/cmd/init
  install -m 644 "${BUILD_DIR}/init-linux-arm64" "${GUESTINIT_ARM64_EMBED_PATH}"

  GOOS=linux GOARCH=amd64 go build -o "${BUILD_DIR}/init-linux-amd64" ./internal/cmd/init
  install -m 644 "${BUILD_DIR}/init-linux-amd64" "${GUESTINIT_AMD64_EMBED_PATH}"

  go build -tags embed_guestinit -o "${CCVM_OUTPUT}" ./cmd/ccvm
  go build -o "${CC_OUTPUT}" ./cmd/cc
)

(
  cd "${ROOT_DIR}"
  go build -o "${VMSH_OUTPUT}" ./cmd/vmsh
)

if [[ "${TARGET_GOOS}" == "darwin" && "$(uname -s)" == "Darwin" ]]; then
  codesign -f -s - --entitlements "${ROOT_DIR}/tools/entitlements.xml" "${CCVM_OUTPUT}"
fi

printf '%s\n' "${VMSH_OUTPUT}"
