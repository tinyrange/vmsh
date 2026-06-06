#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD_DIR="${ROOT_DIR}/build/vmsh"
TIMEOUT_SECONDS="${VMSH_SMOKE_TIMEOUT:-60}"

if [[ "${VMSH_SMOKE_SKIP_BUILD:-0}" != "1" ]]; then
  "${ROOT_DIR}/tools/build_vmsh.sh" >/dev/null
fi

tmpdir="$(mktemp -d "/tmp/vmsh-multi.XXXXXX")"
cleanup() {
  rm -rf "${tmpdir}"
}
trap cleanup EXIT

cache_dir="${tmpdir}/cache"
script_path="${tmpdir}/multi.vmsh"
stdout_path="${tmpdir}/stdout"
stderr_path="${tmpdir}/stderr"

cat >"${script_path}" <<'EOF'
@alpine --vm one --memory 256 --network
@start --vm one
@alpine --vm one sh -lc 'echo one:$(id -u):$(hostname):$(uname -m)'
@alpine --vm two --memory 256 --network
@start --vm two
@alpine --vm two sh -lc 'echo two:$(id -u):$(hostname):$(uname -m)'
@ps
@stop --vm two
@stop --vm one
EOF

"${BUILD_DIR}/cc" \
  -cache-dir "${cache_dir}" \
  -ccvm "${BUILD_DIR}/ccvm" \
  pull alpine "${ROOT_DIR}/cc/fixtures/alpine.simg"

(
  "${BUILD_DIR}/vmsh" \
    -cache-dir "${cache_dir}" \
    -ccvm "${BUILD_DIR}/ccvm" \
    -script "${script_path}"
) >"${stdout_path}" 2>"${stderr_path}" &
vmsh_pid=$!

(
  sleep "${TIMEOUT_SECONDS}"
  if kill -0 "${vmsh_pid}" 2>/dev/null; then
    echo "vmsh multi-VM smoke timed out after ${TIMEOUT_SECONDS}s" >>"${stderr_path}"
    kill -QUIT "${vmsh_pid}" 2>/dev/null || true
    sleep 2
    kill -TERM "${vmsh_pid}" 2>/dev/null || true
  fi
) &
watchdog_pid=$!

set +e
wait "${vmsh_pid}"
rc=$?
set -e
kill "${watchdog_pid}" 2>/dev/null || true
wait "${watchdog_pid}" 2>/dev/null || true
if [[ "${rc}" -ne 0 ]]; then
  echo "--- vmsh stdout ---"
  cat "${stdout_path}"
  echo "--- vmsh stderr ---"
  cat "${stderr_path}"
  exit "${rc}"
fi

for expected in \
  "one:1000:ccx3:" \
  "two:1000:ccx3:" \
  "one running addr=10.42.0.2" \
  "two running addr=10.42.0.3"; do
  if ! grep -Fq "${expected}" "${stdout_path}"; then
    echo "missing expected output: ${expected}" >&2
    echo "--- vmsh stdout ---"
    cat "${stdout_path}"
    echo "--- vmsh stderr ---"
    cat "${stderr_path}"
    exit 1
  fi
done

boot_count="$(grep -Fc "Boot: ready" "${stderr_path}" || true)"
if [[ "${boot_count}" -lt 2 ]]; then
  echo "expected at least two VM boots, saw ${boot_count}" >&2
  echo "--- vmsh stdout ---"
  cat "${stdout_path}"
  echo "--- vmsh stderr ---"
  cat "${stderr_path}"
  exit 1
fi

echo "--- vmsh stdout ---"
cat "${stdout_path}"
echo "--- vmsh stderr ---"
cat "${stderr_path}"
echo "multi-VM vmsh smoke passed"
