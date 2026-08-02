"use strict";

const canvas = document.getElementById("gl");
const title = document.getElementById("title");
const metrics = document.getElementById("metrics");
const progress = document.getElementById("progress");
const errorPanel = document.getElementById("error");
const sceneSeconds = 10;
const scenes = ["Indexed cube field", "Framebuffer prism", "Dynamic particles", "Procedural aurora"];

let gl;
let frame = 0;
let framesThisSample = 0;
let fps = 0;
let sampleStarted = performance.now();
let lastTelemetry = 0;
let lastSceneIndex = -1;
let completedCycles = 0;
let renderer = "unavailable";
let vendor = "unavailable";
let version = "unavailable";
let shadingLanguage = "unavailable";
const visitedScenes = new Set();
const capturedScenes = new Set();
const captureSamples = {};
const captureCanvas = document.createElement("canvas");
const captureContext = captureCanvas.getContext("2d");

function fail(message) {
  errorPanel.style.display = "grid";
  errorPanel.textContent = `WebGL initialization failed\n\n${message}`;
  postTelemetry({ status: "error", error: String(message) });
  throw new Error(message);
}

function postTelemetry(extra = {}) {
  const payload = Object.assign({
    status: "running",
    renderer,
    vendor,
    version,
    shadingLanguage,
    width: canvas.width,
    height: canvas.height,
    frame,
    fps: Number(fps.toFixed(1)),
    uptimeSeconds: Number((performance.now() / 1000).toFixed(1)),
    completedCycles,
    visitedScenes: Array.from(visitedScenes).sort(),
    captureSamples,
  }, extra);
  fetch("/telemetry", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  }).catch(() => {});
}

function shader(type, source) {
  const result = gl.createShader(type);
  gl.shaderSource(result, source);
  gl.compileShader(result);
  if (!gl.getShaderParameter(result, gl.COMPILE_STATUS)) {
    fail(gl.getShaderInfoLog(result));
  }
  return result;
}

function program(vertexSource, fragmentSource) {
  const result = gl.createProgram();
  gl.attachShader(result, shader(gl.VERTEX_SHADER, vertexSource));
  gl.attachShader(result, shader(gl.FRAGMENT_SHADER, fragmentSource));
  gl.linkProgram(result);
  if (!gl.getProgramParameter(result, gl.LINK_STATUS)) {
    fail(gl.getProgramInfoLog(result));
  }
  return result;
}

function buffer(target, data, usage = gl.STATIC_DRAW) {
  const result = gl.createBuffer();
  gl.bindBuffer(target, result);
  gl.bufferData(target, data, usage);
  return result;
}

function multiply(a, b) {
  const out = new Float32Array(16);
  for (let column = 0; column < 4; column++) {
    for (let row = 0; row < 4; row++) {
      out[column * 4 + row] =
        a[row] * b[column * 4] +
        a[4 + row] * b[column * 4 + 1] +
        a[8 + row] * b[column * 4 + 2] +
        a[12 + row] * b[column * 4 + 3];
    }
  }
  return out;
}

function perspective(aspect, near, far) {
  const f = 1 / Math.tan(Math.PI / 6);
  const range = 1 / (near - far);
  return new Float32Array([
    f / aspect, 0, 0, 0,
    0, f, 0, 0,
    0, 0, (far + near) * range, -1,
    0, 0, 2 * far * near * range, 0,
  ]);
}

function normalize(v) {
  const length = Math.hypot(v[0], v[1], v[2]) || 1;
  return [v[0] / length, v[1] / length, v[2] / length];
}

function cross(a, b) {
  return [a[1] * b[2] - a[2] * b[1], a[2] * b[0] - a[0] * b[2], a[0] * b[1] - a[1] * b[0]];
}

function lookAt(eye, center, up) {
  const z = normalize([eye[0] - center[0], eye[1] - center[1], eye[2] - center[2]]);
  const x = normalize(cross(up, z));
  const y = cross(z, x);
  return new Float32Array([
    x[0], y[0], z[0], 0,
    x[1], y[1], z[1], 0,
    x[2], y[2], z[2], 0,
    -(x[0] * eye[0] + x[1] * eye[1] + x[2] * eye[2]),
    -(y[0] * eye[0] + y[1] * eye[1] + y[2] * eye[2]),
    -(z[0] * eye[0] + z[1] * eye[1] + z[2] * eye[2]),
    1,
  ]);
}

function modelMatrix(x, y, z, ax, ay, scale) {
  const sx = Math.sin(ax), cx = Math.cos(ax);
  const sy = Math.sin(ay), cy = Math.cos(ay);
  return new Float32Array([
    cy * scale, sx * sy * scale, -cx * sy * scale, 0,
    0, cx * scale, sx * scale, 0,
    sy * scale, -sx * cy * scale, cx * cy * scale, 0,
    x, y, z, 1,
  ]);
}

function resize() {
  const width = Math.max(1, window.innerWidth);
  const height = Math.max(1, window.innerHeight);
  if (canvas.width !== width || canvas.height !== height) {
    canvas.width = width;
    canvas.height = height;
  }
}

try {
  gl = canvas.getContext("webgl", {
    alpha: false,
    antialias: true,
    depth: true,
    stencil: false,
    preserveDrawingBuffer: true,
  });
} catch (error) {
  fail(error);
}
if (!gl) fail("Firefox did not create a WebGL context");

const debugRenderer = gl.getExtension("WEBGL_debug_renderer_info");
renderer = debugRenderer ? gl.getParameter(debugRenderer.UNMASKED_RENDERER_WEBGL) : gl.getParameter(gl.RENDERER);
vendor = debugRenderer ? gl.getParameter(debugRenderer.UNMASKED_VENDOR_WEBGL) : gl.getParameter(gl.VENDOR);
version = gl.getParameter(gl.VERSION);
shadingLanguage = gl.getParameter(gl.SHADING_LANGUAGE_VERSION);

const cubeProgram = program(`
attribute vec3 aPosition;
attribute vec3 aNormal;
attribute vec2 aUV;
uniform mat4 uMVP;
uniform mat4 uModel;
varying vec3 vNormal;
varying vec2 vUV;
varying vec3 vWorld;
void main() {
  vec4 world = uModel * vec4(aPosition, 1.0);
  gl_Position = uMVP * vec4(aPosition, 1.0);
  vNormal = mat3(uModel) * aNormal;
  vUV = aUV;
  vWorld = world.xyz;
}`,
`precision mediump float;
uniform sampler2D uTexture;
uniform float uHue;
varying vec3 vNormal;
varying vec2 vUV;
varying vec3 vWorld;
void main() {
  vec3 normal = normalize(vNormal);
  vec3 light = normalize(vec3(0.5, 0.85, 0.35));
  float diffuse = max(dot(normal, light), 0.0);
  float rim = pow(1.0 - max(abs(normal.z), 0.0), 2.0);
  vec3 texel = texture2D(uTexture, vUV * 2.0).rgb;
  vec3 tint = 0.55 + 0.45 * cos(vec3(0.0, 2.1, 4.2) + uHue);
  gl_FragColor = vec4(texel * tint * (0.22 + 0.78 * diffuse) + rim * vec3(0.15, 0.35, 0.7), 1.0);
}`);

const postProgram = program(`
attribute vec2 aPosition;
varying vec2 vUV;
void main() {
  vUV = aPosition * 0.5 + 0.5;
  gl_Position = vec4(aPosition, 0.0, 1.0);
}`,
`precision mediump float;
uniform sampler2D uTexture;
uniform float uTime;
varying vec2 vUV;
void main() {
  vec2 centered = vUV - 0.5;
  float radius = length(centered);
  vec2 warped = vUV + centered * (0.035 * sin(uTime * 1.7 + radius * 18.0));
  float shift = 0.003 + radius * 0.004;
  vec3 color = vec3(
    texture2D(uTexture, warped + vec2(shift, 0.0)).r,
    texture2D(uTexture, warped).g,
    texture2D(uTexture, warped - vec2(shift, 0.0)).b
  );
  float vignette = smoothstep(0.78, 0.25, radius);
  gl_FragColor = vec4(color * vignette + vec3(0.02, 0.01, 0.05), 1.0);
}`);

const particleProgram = program(`
attribute vec3 aParticle;
uniform float uTime;
varying float vEnergy;
void main() {
  float depth = 0.55 + 0.45 * sin(aParticle.z * 4.0 + uTime);
  vec2 orbit = vec2(
    aParticle.x * cos(uTime * 0.17) - aParticle.y * sin(uTime * 0.17),
    aParticle.x * sin(uTime * 0.17) + aParticle.y * cos(uTime * 0.17)
  );
  gl_Position = vec4(orbit, 0.0, 1.0);
  gl_PointSize = 2.0 + depth * 5.0;
  vEnergy = depth;
}`,
`precision mediump float;
varying float vEnergy;
void main() {
  vec2 point = gl_PointCoord * 2.0 - 1.0;
  float glow = max(0.0, 1.0 - dot(point, point));
  vec3 cold = vec3(0.05, 0.35, 1.0);
  vec3 hot = vec3(1.0, 0.22, 0.75);
  gl_FragColor = vec4(mix(cold, hot, vEnergy) * glow, glow * 0.78);
}`);

const plasmaProgram = program(`
attribute vec2 aPosition;
varying vec2 vUV;
void main() {
  vUV = aPosition * 0.5 + 0.5;
  gl_Position = vec4(aPosition, 0.0, 1.0);
}`,
`precision highp float;
uniform float uTime;
uniform vec2 uResolution;
varying vec2 vUV;
void main() {
  vec2 p = (gl_FragCoord.xy * 2.0 - uResolution) / min(uResolution.x, uResolution.y);
  float t = uTime * 0.55;
  float wave = sin(p.x * 5.0 + t);
  wave += sin((p.x + p.y) * 4.0 - t * 1.3);
  wave += sin(length(p + vec2(sin(t), cos(t))) * 9.0 - t * 2.0);
  wave += sin(atan(p.y, p.x) * 5.0 + t);
  wave *= 0.25;
  vec3 color = 0.52 + 0.48 * cos(vec3(0.0, 1.8, 3.7) + wave * 4.5 + t);
  float stars = step(0.996, fract(sin(dot(floor(gl_FragCoord.xy / 3.0), vec2(12.9898, 78.233))) * 43758.5453));
  gl_FragColor = vec4(color * (0.55 + 0.45 * smoothstep(1.4, 0.0, length(p))) + stars, 1.0);
}`);

const cubeVertices = new Float32Array([
  -1,-1, 1, 0,0,1, 0,0,  1,-1, 1, 0,0,1, 1,0,  1,1,1, 0,0,1, 1,1,  -1,1,1, 0,0,1, 0,1,
   1,-1,-1, 0,0,-1, 0,0, -1,-1,-1, 0,0,-1, 1,0, -1,1,-1, 0,0,-1, 1,1, 1,1,-1, 0,0,-1, 0,1,
  -1,-1,-1, -1,0,0, 0,0, -1,-1,1, -1,0,0, 1,0, -1,1,1, -1,0,0, 1,1, -1,1,-1, -1,0,0, 0,1,
   1,-1,1, 1,0,0, 0,0, 1,-1,-1, 1,0,0, 1,0, 1,1,-1, 1,0,0, 1,1, 1,1,1, 1,0,0, 0,1,
  -1,1,1, 0,1,0, 0,0, 1,1,1, 0,1,0, 1,0, 1,1,-1, 0,1,0, 1,1, -1,1,-1, 0,1,0, 0,1,
  -1,-1,-1, 0,-1,0, 0,0, 1,-1,-1, 0,-1,0, 1,0, 1,-1,1, 0,-1,0, 1,1, -1,-1,1, 0,-1,0, 0,1,
]);
const cubeIndices = new Uint16Array([
  0,1,2, 0,2,3, 4,5,6, 4,6,7, 8,9,10, 8,10,11,
  12,13,14, 12,14,15, 16,17,18, 16,18,19, 20,21,22, 20,22,23,
]);
const cubeVertexBuffer = buffer(gl.ARRAY_BUFFER, cubeVertices);
const cubeIndexBuffer = buffer(gl.ELEMENT_ARRAY_BUFFER, cubeIndices);
const quadBuffer = buffer(gl.ARRAY_BUFFER, new Float32Array([-1,-1, 1,-1, -1,1, -1,1, 1,-1, 1,1]));

const checkerTexture = gl.createTexture();
gl.bindTexture(gl.TEXTURE_2D, checkerTexture);
const checker = new Uint8Array(64 * 64 * 4);
for (let y = 0; y < 64; y++) {
  for (let x = 0; x < 64; x++) {
    const offset = (y * 64 + x) * 4;
    const value = ((x >> 3) ^ (y >> 3)) & 1;
    checker[offset] = value ? 245 : 28;
    checker[offset + 1] = value ? 250 : 50;
    checker[offset + 2] = value ? 255 : 88;
    checker[offset + 3] = 255;
  }
}
gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, 64, 64, 0, gl.RGBA, gl.UNSIGNED_BYTE, checker);
gl.generateMipmap(gl.TEXTURE_2D);
gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR_MIPMAP_LINEAR);
gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR);

const framebufferTexture = gl.createTexture();
const framebuffer = gl.createFramebuffer();
const depthBuffer = gl.createRenderbuffer();
let framebufferWidth = 0;
let framebufferHeight = 0;

function updateFramebuffer() {
  const width = Math.min(1024, canvas.width);
  const height = Math.min(1024, canvas.height);
  if (width === framebufferWidth && height === framebufferHeight) return;
  framebufferWidth = width;
  framebufferHeight = height;
  gl.bindTexture(gl.TEXTURE_2D, framebufferTexture);
  gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, width, height, 0, gl.RGBA, gl.UNSIGNED_BYTE, null);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
  gl.bindRenderbuffer(gl.RENDERBUFFER, depthBuffer);
  gl.renderbufferStorage(gl.RENDERBUFFER, gl.DEPTH_COMPONENT16, width, height);
  gl.bindFramebuffer(gl.FRAMEBUFFER, framebuffer);
  gl.framebufferTexture2D(gl.FRAMEBUFFER, gl.COLOR_ATTACHMENT0, gl.TEXTURE_2D, framebufferTexture, 0);
  gl.framebufferRenderbuffer(gl.FRAMEBUFFER, gl.DEPTH_ATTACHMENT, gl.RENDERBUFFER, depthBuffer);
  if (gl.checkFramebufferStatus(gl.FRAMEBUFFER) !== gl.FRAMEBUFFER_COMPLETE) fail("offscreen framebuffer is incomplete");
  gl.bindFramebuffer(gl.FRAMEBUFFER, null);
}

const particleCount = 6144;
const particles = new Float32Array(particleCount * 3);
const particleBuffer = buffer(gl.ARRAY_BUFFER, particles, gl.DYNAMIC_DRAW);

function attribute(programObject, name, size, stride, offset) {
  const location = gl.getAttribLocation(programObject, name);
  gl.enableVertexAttribArray(location);
  gl.vertexAttribPointer(location, size, gl.FLOAT, false, stride, offset);
}

function drawCubeField(time, width, height, count) {
  gl.viewport(0, 0, width, height);
  gl.clearColor(0.012, 0.018, 0.055, 1);
  gl.clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT);
  gl.enable(gl.DEPTH_TEST);
  gl.depthFunc(gl.LEQUAL);
  gl.enable(gl.CULL_FACE);
  gl.cullFace(gl.BACK);
  gl.disable(gl.BLEND);
  gl.useProgram(cubeProgram);
  gl.bindBuffer(gl.ARRAY_BUFFER, cubeVertexBuffer);
  gl.bindBuffer(gl.ELEMENT_ARRAY_BUFFER, cubeIndexBuffer);
  attribute(cubeProgram, "aPosition", 3, 32, 0);
  attribute(cubeProgram, "aNormal", 3, 32, 12);
  attribute(cubeProgram, "aUV", 2, 32, 24);
  gl.activeTexture(gl.TEXTURE0);
  gl.bindTexture(gl.TEXTURE_2D, checkerTexture);
  gl.uniform1i(gl.getUniformLocation(cubeProgram, "uTexture"), 0);
  const projection = perspective(width / height, 0.1, 80);
  const eye = [Math.sin(time * 0.18) * 15, 7 + Math.sin(time * 0.23) * 2, Math.cos(time * 0.18) * 15];
  const view = lookAt(eye, [0, 0, 0], [0, 1, 0]);
  const projectionView = multiply(projection, view);
  const mvpLocation = gl.getUniformLocation(cubeProgram, "uMVP");
  const modelLocation = gl.getUniformLocation(cubeProgram, "uModel");
  const hueLocation = gl.getUniformLocation(cubeProgram, "uHue");
  for (let index = 0; index < count; index++) {
    const column = index % 5;
    const row = Math.floor(index / 5) % 4;
    const layer = Math.floor(index / 20);
    const model = modelMatrix(
      (column - 2) * 3.1,
      (row - 1.5) * 3.1,
      (layer - 1) * 4.5,
      time * (0.35 + (index % 7) * 0.025) + index,
      time * (0.48 + (index % 5) * 0.03) - index * 0.4,
      0.72,
    );
    gl.uniformMatrix4fv(modelLocation, false, model);
    gl.uniformMatrix4fv(mvpLocation, false, multiply(projectionView, model));
    gl.uniform1f(hueLocation, time * 0.35 + index * 0.27);
    gl.drawElements(gl.TRIANGLES, cubeIndices.length, gl.UNSIGNED_SHORT, 0);
  }
}

function drawFramebufferScene(time) {
  updateFramebuffer();
  gl.bindFramebuffer(gl.FRAMEBUFFER, framebuffer);
  drawCubeField(time * 1.1, framebufferWidth, framebufferHeight, 32);
  gl.bindFramebuffer(gl.FRAMEBUFFER, null);
  gl.viewport(0, 0, canvas.width, canvas.height);
  gl.disable(gl.DEPTH_TEST);
  gl.disable(gl.CULL_FACE);
  gl.useProgram(postProgram);
  gl.bindBuffer(gl.ARRAY_BUFFER, quadBuffer);
  attribute(postProgram, "aPosition", 2, 0, 0);
  gl.activeTexture(gl.TEXTURE0);
  gl.bindTexture(gl.TEXTURE_2D, framebufferTexture);
  gl.uniform1i(gl.getUniformLocation(postProgram, "uTexture"), 0);
  gl.uniform1f(gl.getUniformLocation(postProgram, "uTime"), time);
  gl.drawArrays(gl.TRIANGLES, 0, 6);
}

function drawParticles(time) {
  for (let index = 0; index < particleCount; index++) {
    const phase = index * 2.399963 + time * (0.12 + (index % 19) * 0.0015);
    const ring = 0.08 + 0.84 * ((index % 257) / 256);
    const pulse = 0.86 + 0.14 * Math.sin(time * 1.7 + index * 0.071);
    particles[index * 3] = Math.cos(phase) * ring * pulse;
    particles[index * 3 + 1] = Math.sin(phase * 1.013) * ring * pulse;
    particles[index * 3 + 2] = (index % 113) / 112;
  }
  gl.bindFramebuffer(gl.FRAMEBUFFER, null);
  gl.viewport(0, 0, canvas.width, canvas.height);
  gl.clearColor(0.002, 0.004, 0.02, 1);
  gl.clear(gl.COLOR_BUFFER_BIT);
  gl.disable(gl.DEPTH_TEST);
  gl.disable(gl.CULL_FACE);
  gl.enable(gl.BLEND);
  gl.blendFunc(gl.SRC_ALPHA, gl.ONE);
  gl.useProgram(particleProgram);
  gl.bindBuffer(gl.ARRAY_BUFFER, particleBuffer);
  gl.bufferSubData(gl.ARRAY_BUFFER, 0, particles);
  attribute(particleProgram, "aParticle", 3, 0, 0);
  gl.uniform1f(gl.getUniformLocation(particleProgram, "uTime"), time);
  gl.drawArrays(gl.POINTS, 0, particleCount);
  gl.disable(gl.BLEND);
}

function drawPlasma(time) {
  gl.bindFramebuffer(gl.FRAMEBUFFER, null);
  gl.viewport(0, 0, canvas.width, canvas.height);
  gl.disable(gl.DEPTH_TEST);
  gl.disable(gl.CULL_FACE);
  gl.disable(gl.BLEND);
  gl.useProgram(plasmaProgram);
  gl.bindBuffer(gl.ARRAY_BUFFER, quadBuffer);
  attribute(plasmaProgram, "aPosition", 2, 0, 0);
  gl.uniform1f(gl.getUniformLocation(plasmaProgram, "uTime"), time);
  gl.uniform2f(gl.getUniformLocation(plasmaProgram, "uResolution"), canvas.width, canvas.height);
  gl.drawArrays(gl.TRIANGLES, 0, 6);
}

function updatePanel(sceneIndex) {
  title.textContent = scenes[sceneIndex];
  metrics.innerHTML = `
    <div class="label">renderer</div><div>${renderer}</div>
    <div class="label">WebGL</div><div>${version}</div>
    <div class="label">canvas</div><div>${canvas.width} × ${canvas.height}</div>
    <div class="label">performance</div><div>${fps.toFixed(1)} FPS · frame ${frame}</div>
    <div class="label">suite</div><div>${sceneIndex + 1} / ${scenes.length}</div>`;
}

function captureScene(sceneIndex) {
  const width = canvas.width;
  const height = canvas.height;
  const pixels = new Uint8Array(width * height * 4);
  gl.finish();
  gl.readPixels(0, 0, width, height, gl.RGBA, gl.UNSIGNED_BYTE, pixels);
  const readbackError = gl.getError();
  if (readbackError !== gl.NO_ERROR) {
    fail(`WebGL readback error 0x${readbackError.toString(16)} in scene ${sceneIndex}`);
  }

  const topDown = new Uint8ClampedArray(pixels.length);
  const rowBytes = width * 4;
  let nonzero = 0;
  let checksum = 2166136261;
  for (let row = 0; row < height; row++) {
    const source = (height - 1 - row) * rowBytes;
    const destination = row * rowBytes;
    const sourceRow = pixels.subarray(source, source + rowBytes);
    topDown.set(sourceRow, destination);
    for (let index = 0; index < sourceRow.length; index++) {
      const value = sourceRow[index];
      if (value !== 0) nonzero++;
      checksum = Math.imul(checksum ^ value, 16777619) >>> 0;
    }
  }
  captureSamples[sceneIndex] = { nonzero, checksum };
  captureCanvas.width = width;
  captureCanvas.height = height;
  captureContext.putImageData(new ImageData(topDown, width, height), 0, 0);
  captureCanvas.toBlob(blob => {
    if (blob) {
      fetch(`/capture/${sceneIndex}`, { method: "POST", body: blob }).catch(() => {});
    }
  }, "image/png");
}

canvas.addEventListener("webglcontextlost", event => {
  event.preventDefault();
  fail("WebGL context was lost");
});

function render(now) {
  resize();
  const time = now / 1000;
  const suitePosition = time % (sceneSeconds * scenes.length);
  const sceneIndex = Math.floor(suitePosition / sceneSeconds);
  if (lastSceneIndex === scenes.length - 1 && sceneIndex === 0) completedCycles++;
  lastSceneIndex = sceneIndex;
  visitedScenes.add(sceneIndex);
  progress.style.transform = `scaleX(${(suitePosition % sceneSeconds) / sceneSeconds})`;
  if (sceneIndex === 0) {
    gl.bindFramebuffer(gl.FRAMEBUFFER, null);
    drawCubeField(time, canvas.width, canvas.height, 60);
  } else if (sceneIndex === 1) {
    drawFramebufferScene(time);
  } else if (sceneIndex === 2) {
    drawParticles(time);
  } else {
    drawPlasma(time);
  }

  const secondsIntoScene = suitePosition % sceneSeconds;
  if (secondsIntoScene >= 2 && !capturedScenes.has(sceneIndex)) {
    capturedScenes.add(sceneIndex);
    captureScene(sceneIndex);
  }

  frame++;
  framesThisSample++;
  if (now - sampleStarted >= 1000) {
    fps = framesThisSample * 1000 / (now - sampleStarted);
    framesThisSample = 0;
    sampleStarted = now;
    updatePanel(sceneIndex);
  }
  if (now - lastTelemetry >= 2000) {
    lastTelemetry = now;
    postTelemetry({ scene: scenes[sceneIndex], sceneIndex, sceneCount: scenes.length });
  }
  if (frame % 120 === 0) {
    const error = gl.getError();
    if (error !== gl.NO_ERROR) fail(`WebGL error 0x${error.toString(16)} at frame ${frame}`);
  }
  requestAnimationFrame(render);
}

updatePanel(0);
postTelemetry({ status: "initialized", scene: scenes[0], sceneIndex: 0, sceneCount: scenes.length });
requestAnimationFrame(render);
