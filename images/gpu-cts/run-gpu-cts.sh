#!/bin/sh
set -eu

display=:0
results=/shared/gpu-cts
modules=/opt/vk-gl-cts/modules
gles2=/opt/vk-gl-cts/gles2
gles3=/opt/vk-gl-cts/gles3
gles31=/opt/vk-gl-cts/gles31

write_summary() {
    total_groups=0
    completed_groups=0
    if [ -f "$results/es2-groups.txt" ]; then
        total_groups=$(grep -c . "$results/es2-groups.txt" || true)
    fi
    completed_groups=$(find "$results/es2-groups" -type f -name '*.status' | wc -l | tr -d ' ')
    case_counts=$(for status in "$results"/es2-groups/*.status; do
        [ -f "$status" ] || continue
        qpa=${status%.status}.qpa
        [ -f "$qpa" ] || continue
        grep -h -o 'StatusCode="[A-Za-z]*"' "$qpa" || true
    done \
        | sed 's/StatusCode="//;s/"//' \
        | jq -R -s 'split("\n") | map(select(length > 0)) | group_by(.) | map({(.[0]): length}) | add // {}')
    jq -n \
        --argjson total_groups "$total_groups" \
        --argjson completed_groups "$completed_groups" \
        --argjson case_counts "$case_counts" \
        --arg complete "$([ -f "$results/complete" ] && printf true || printf false)" \
        '{total_groups:$total_groups,completed_groups:$completed_groups,case_results:$case_counts,complete:($complete == "true")}' \
        > "$results/summary.json.new"
    mv "$results/summary.json.new" "$results/summary.json"
}

install -d -m 0755 \
    /run/user/1000 \
    /tmp/.X11-unix \
    "$results/es2-cts" \
    "$results/es2-groups" \
    "$results/es3-probes" \
    "$results/es31-probes"
for gl_version in 30 31 32 33 40 41; do
    install -d -m 0755 "$results/gl${gl_version}-probes"
done
rm -f /tmp/.X0-lock /tmp/.X11-unix/X0 "$results/startup-error.log"

setsid /usr/lib/xorg/Xorg "$display" \
    -noreset \
    -nolisten tcp \
    -novtswitch \
    -logfile "$results/Xorg.log" </dev/null &
xorg_pid=$!
trap 'kill "$xorg_pid" 2>/dev/null || true' EXIT INT TERM

attempt=0
while [ ! -S /tmp/.X11-unix/X0 ]; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 400 ]; then
        echo "GPU CTS fixture: Xorg did not create display :0" \
            | tee "$results/startup-error.log" >&2
        exit 1
    fi
    sleep 0.05
done

export DISPLAY="$display"
glxinfo -B > "$results/glxinfo.log" 2>&1
if ! grep -Eq '^OpenGL renderer string: .*virgl' "$results/glxinfo.log"; then
    echo "GPU CTS fixture: Mesa did not select the virgl renderer" \
        | tee "$results/startup-error.log" >&2
    cat "$results/glxinfo.log" >&2
    exit 1
fi

touch /run/user/1000/squadvm-desktop-ready

tag=$(cat /opt/vk-gl-cts/tag)
commit=$(cat /opt/vk-gl-cts/commit)
renderer=$(sed -n 's/^OpenGL renderer string: //p' "$results/glxinfo.log")
version=$(sed -n 's/^OpenGL version string: //p' "$results/glxinfo.log")
sl_version=$(sed -n 's/^OpenGL shading language version string: //p' "$results/glxinfo.log")
jq -n \
    --arg suite "VK-GL-CTS" \
    --arg tag "$tag" \
    --arg commit "$commit" \
    --arg renderer "$renderer" \
    --arg version "$version" \
    --arg shading_language "$sl_version" \
    '{suite:$suite,tag:$tag,commit:$commit,renderer:$renderer,version:$version,shading_language:$shading_language,mesa_gles1_enabled:true}' \
    > "$results/manifest.json.new"
mv "$results/manifest.json.new" "$results/manifest.json"

# These configuration-discovery probes are deliberately run before the long
# suite. The manifest above, not a passing config enumeration, records the
# guest-visible API ceiling.
"$modules/glcts" \
    --deqp-gl-context-type=egl \
    --deqp-case='CTS-Configs.es2' \
    --deqp-log-filename="$results/es2-info.qpa" \
    --deqp-surface-width=256 \
    --deqp-surface-height=256 \
    > "$results/es2-info.log" 2>&1 || true
for gl_version in 30 31 32 33 40 41; do
    "$modules/glcts" \
        --deqp-gl-context-type=egl \
        --deqp-case="KHR-GL${gl_version}.info.*" \
        --deqp-log-filename="$results/gl${gl_version}-probes/info.qpa" \
        --deqp-surface-width=256 \
        --deqp-surface-height=256 \
        > "$results/gl${gl_version}-probes/info.log" 2>&1 || true
done

cd "$gles3"
./deqp-gles3 \
    --deqp-case='dEQP-GLES3.info.*' \
    --deqp-gl-config-name=rgba8888d24s8ms0 \
    --deqp-log-filename="$results/es3-probes/info.qpa" \
    --deqp-surface-width=256 \
    --deqp-surface-height=256 \
    > "$results/es3-probes/info.log" 2>&1 || true

cd "$gles31"
./deqp-gles31 \
    --deqp-case='dEQP-GLES31.info.*' \
    --deqp-gl-config-name=rgba8888d24s8ms0 \
    --deqp-log-filename="$results/es31-probes/info.qpa" \
    --deqp-surface-width=256 \
    --deqp-surface-height=256 \
    > "$results/es31-probes/info.log" 2>&1 || true

# A completed ES2 result directory is immutable evidence. A service restart or
# VM reboot may refresh capability probes, but must not execute the official
# suite again over those logs.
if [ -f "$results/complete" ]; then
    write_summary
    while :; do
        sleep 3600
    done
fi

# Run dEQP-GLES2 in stable top-level groups. A completed status file is the
# resume boundary, so a VM restart never discards already collected groups.
cd "$gles2"
./deqp-gles2 \
    --deqp-runmode=txt-caselist \
    --deqp-caselist-export-file="$results/dEQP-GLES2-cases.txt" \
    > "$results/es2-caselist.log" 2>&1

# dEQP performance workloads produce timing data rather than conformance
# verdicts. Keep this baseline to correctness cases; cts-runner below executes
# the pinned Khronos ES2 must-pass configuration.
awk '
    /^TEST: dEQP-GLES2\.performance\./ { next }
    /^TEST: / {
        sub(/^TEST: /, "")
        count = split($0, field, ".")
        if (count >= 3)
            print field[1] "." field[2] "." field[3] ".*"
    }
' "$results/dEQP-GLES2-cases.txt" | sort -u > "$results/es2-groups.txt.new"
mv "$results/es2-groups.txt.new" "$results/es2-groups.txt"
write_summary

while IFS= read -r pattern; do
    [ -n "$pattern" ] || continue
    group=$(printf '%s' "$pattern" | tr -c 'A-Za-z0-9._-' '_')
    status="$results/es2-groups/$group.status"
    [ -f "$status" ] && continue

    started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    result=0
    ./deqp-gles2 \
        --deqp-case="$pattern" \
        --deqp-gl-config-name=rgba8888d24s8ms0 \
        --deqp-log-filename="$results/es2-groups/$group.qpa" \
        --deqp-surface-width=256 \
        --deqp-surface-height=256 \
        > "$results/es2-groups/$group.log" 2>&1 || result=$?
    finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    jq -n \
        --arg pattern "$pattern" \
        --arg started "$started" \
        --arg finished "$finished" \
        --argjson exit_code "$result" \
        '{pattern:$pattern,started:$started,finished:$finished,exit_code:$exit_code}' \
        > "$status.new"
    mv "$status.new" "$status"
    write_summary
done < "$results/es2-groups.txt"

# Preserve the Khronos runner's exact ES2 configuration and execute it after
# the resumable dEQP baseline. Its .qpa/XML artifacts are the conformance record.
# --summary only reads an existing run, so it must follow the actual execution.
cd "$modules"
if find "$results/es2-cts" -type f -print -quit | grep -q .; then
    interrupted="$results/es2-cts-interrupted-$(date -u +%Y%m%dT%H%M%SZ)"
    mv "$results/es2-cts" "$interrupted"
    for artifact in es2-cts.log es2-cts-summary.log es2-cts.status; do
        if [ -f "$results/$artifact" ]; then
            mv "$results/$artifact" "$interrupted/$artifact"
        fi
    done
    install -d -m 0755 "$results/es2-cts"
fi
runner_result=0
./cts-runner --type=es2 --logdir="$results/es2-cts" \
    > "$results/es2-cts.log" 2>&1 || runner_result=$?
./cts-runner --type=es2 --summary --logdir="$results/es2-cts" \
    > "$results/es2-cts-summary.log" 2>&1 || true
runner_complete=true
found_qpa=false
for qpa in "$results"/es2-cts/*.qpa; do
    [ -f "$qpa" ] || continue
    found_qpa=true
    if ! tail -n 8 "$qpa" | grep -q '^#endSession$'; then
        runner_complete=false
    fi
done
[ "$found_qpa" = true ] || runner_complete=false
jq -n --argjson exit_code "$runner_result" --arg complete "$runner_complete" \
    '{exit_code:$exit_code,complete:($complete == "true")}' \
    > "$results/es2-cts.status.new"
mv "$results/es2-cts.status.new" "$results/es2-cts.status"
[ "$runner_complete" = true ] && touch "$results/complete"
write_summary

while :; do
    sleep 3600
done
