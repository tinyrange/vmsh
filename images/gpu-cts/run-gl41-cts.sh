#!/bin/sh
set -u

display=:0
results=${1:-/shared/gpu-cts/gl41}
modules=/opt/vk-gl-cts/modules
shards="$results/shards"
export LD_LIBRARY_PATH=/opt/mesa/lib
export LIBGL_DRIVERS_PATH=/opt/mesa/lib/dri

install -d -m 0755 /run/user/1000 /tmp/.X11-unix "$results" "$shards"
rm -f /tmp/.X0-lock /tmp/.X11-unix/X0

setsid /usr/lib/xorg/Xorg "$display" \
    -noreset \
    -nolisten tcp \
    -novtswitch \
    -logfile "$results/Xorg.log" \
    </dev/null >"$results/Xorg.stdout" 2>&1 &
xorg_pid=$!
cleanup() {
    kill "$xorg_pid" 2>/dev/null || true
    wait "$xorg_pid" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

attempt=0
while [ ! -S /tmp/.X11-unix/X0 ]; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 400 ]; then
        echo "GL 4.1 CTS: Xorg did not create display $display" >&2
        exit 1
    fi
    sleep 0.05
done

export DISPLAY="$display"
cd "$modules"

./glcts \
    --deqp-runmode=txt-caselist \
    --deqp-caselist-export-file="$results/cases.txt" \
    --deqp-case='KHR-GL41.*' \
    >"$results/caselist.stdout" 2>&1

run_shard() {
    pattern=$1
    name=$2
    qpa="$shards/$name.qpa"
    stdout="$shards/$name.stdout"
    status="$shards/$name.status"
    if [ -f "$status" ] && jq -e \
        '.complete == true and .exit_code == 0 and .begun == .ended' \
        "$status" >/dev/null 2>&1
    then
        printf '%-43s already complete\n' "$name"
        return
    fi

    started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    result=0
    ./glcts \
        --deqp-gl-context-type=egl \
        --deqp-terminate-on-device-lost=disable \
        --deqp-case="$pattern" \
        --deqp-log-filename="$qpa" \
        --deqp-surface-width=256 \
        --deqp-surface-height=256 \
        >"$stdout" 2>&1 || result=$?
    finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    begun=$(grep -c '^#beginTestCaseResult ' "$qpa" 2>/dev/null || true)
    ended=$(grep -c '^#endTestCaseResult$' "$qpa" 2>/dev/null || true)
    pass=$(grep -c 'StatusCode="Pass"' "$qpa" 2>/dev/null || true)
    fail=$(grep -c 'StatusCode="Fail"' "$qpa" 2>/dev/null || true)
    not_supported=$(grep -c 'StatusCode="NotSupported"' "$qpa" 2>/dev/null || true)
    complete=false
    if tail -n 8 "$qpa" 2>/dev/null | grep -q '^#endSession$'; then
        complete=true
    fi

    jq -n \
        --arg pattern "$pattern" \
        --arg started "$started" \
        --arg finished "$finished" \
        --argjson exit_code "$result" \
        --argjson begun "$begun" \
        --argjson ended "$ended" \
        --argjson pass "$pass" \
        --argjson fail "$fail" \
        --argjson not_supported "$not_supported" \
        --arg complete "$complete" \
        '{pattern:$pattern,started:$started,finished:$finished,exit_code:$exit_code,begun:$begun,ended:$ended,pass:$pass,fail:$fail,not_supported:$not_supported,complete:($complete == "true")}' \
        >"$status.new"
    mv "$status.new" "$status"
    printf '%-43s cases=%-5s pass=%-5s ns=%-5s fail=%-3s rc=%-3s complete=%s\n' \
        "$name" "$begun" "$pass" "$not_supported" "$fail" "$result" "$complete"
}

# A monolithic GL 4.1 CTS process grows beyond an 8 GiB guest. Keep ordinary
# top-level groups in fresh processes and bound texture_swizzle more tightly.
run_shard 'KHR-GL41.texture_swizzle.api_errors' texture_swizzle-api_errors
run_shard 'KHR-GL41.texture_swizzle.intial_state' texture_swizzle-intial_state
index=0
while [ "$index" -le 13 ]; do
    run_shard \
        "KHR-GL41.texture_swizzle.smoke_access_idx_${index}_*" \
        "texture_swizzle-smoke-${index}"
    index=$((index + 1))
done
index=0
while [ "$index" -le 70 ]; do
    run_shard \
        "KHR-GL41.texture_swizzle.functional_format_idx_${index}_*" \
        "texture_swizzle-functional-${index}"
    index=$((index + 1))
done

# Mesa 26 submits enough distinct packed-pixel shader variants for a single
# packed_pixels process to wedge Apple's native GL command queue. Each format
# family is an independent CTS unit, so keep those units in fresh processes.
run_shard \
    'KHR-GL41.packed_pixels.rectangle.initial_values' \
    packed_pixels-rectangle-initial_values
for family in rectangle varied_rectangle pbo_rectangle; do
    LC_ALL=C awk -v prefix="TEST: KHR-GL41.packed_pixels.${family}." '
        index($0, prefix) == 1 && /_format_/ {
            name = substr($0, length(prefix) + 1)
            sub(/_format_.*/, "", name)
            print name
        }
    ' "$results/cases.txt" | LC_ALL=C sort -u |
    while IFS= read -r format; do
        [ -n "$format" ] || continue
        run_shard \
            "KHR-GL41.packed_pixels.${family}.${format}_format_*" \
            "packed_pixels-${family}-${format}"
    done
done

for group in \
    info clip_distance glsl_noperspective transform_feedback \
    transform_feedback3 texture_repeat_mode texture_lod_basic shaders30 \
    ext_texture_shadow_lod framebuffer_blit texture_lod_bias buffer_objects \
    transform_feedback2 api get_uniform_tests frag_coord_conventions \
    texture_storage draw_buffers CommonBugs texture_size_promotion \
    primitive_restart gpu_shader5_gl transform_feedback_overflow_query_ARB \
    packed_depth_stencil shaders \
    pipeline_statistics_query_tests_ARB cull_distance nearest_edge \
    pixelstoragemodes texture_filter_minmax_tests \
    draw_elements_base_vertex_tests internalformat gpu_shader_fp64 \
    texture_gather draw_indirect clip_control_ARB shader_subroutine \
    texture_barrier_ARB exposed_extensions fragment_shading_rate \
    vertex_attrib_64bit viewport_array
do
    run_shard "KHR-GL41.${group}.*" "group-${group}"
done

LC_ALL=C awk '/^TEST: KHR-GL41[.]/{print $2}' "$results/cases.txt" \
    | LC_ALL=C sort -u >"$results/expected-cases.txt"
for qpa in "$shards"/*.qpa; do
    LC_ALL=C awk '
        /^#beginTestCaseResult / { name=$2 }
        /StatusCode="/ { print name }
    ' "$qpa"
done | LC_ALL=C sort -u >"$results/observed-cases.txt"
LC_ALL=C comm -23 "$results/expected-cases.txt" "$results/observed-cases.txt" \
    >"$results/missing-cases.txt"
LC_ALL=C comm -13 "$results/expected-cases.txt" "$results/observed-cases.txt" \
    >"$results/extra-cases.txt"

expected=$(wc -l <"$results/expected-cases.txt" | tr -d ' ')
observed=$(wc -l <"$results/observed-cases.txt" | tr -d ' ')
missing=$(wc -l <"$results/missing-cases.txt" | tr -d ' ')
extra=$(wc -l <"$results/extra-cases.txt" | tr -d ' ')
jq -s \
    --argjson expected "$expected" \
    --argjson observed "$observed" \
    --argjson missing "$missing" \
    --argjson extra "$extra" \
    '{shards:length,complete_sessions:(map(select(.complete != true or .exit_code != 0 or .begun != .ended))|length == 0),expected:$expected,observed:$observed,missing:$missing,extra:$extra,begun:(map(.begun)|add),ended:(map(.ended)|add),pass:(map(.pass)|add),not_supported:(map(.not_supported)|add),fail:(map(.fail)|add)} | .complete = (.complete_sessions and .expected == .observed and .missing == 0 and .extra == 0 and .begun == .ended and .fail == 0)' \
    "$shards"/*.status >"$results/summary.json.new"
mv "$results/summary.json.new" "$results/summary.json"
jq . "$results/summary.json"
jq -e '.complete == true' "$results/summary.json" >/dev/null
