import { mkdir, writeFile } from "node:fs/promises";

const repository = "tinyrange/vmsh";
const apiUrl = `https://api.github.com/repos/${repository}/releases/latest`;
const outputUrl = new URL("../app/release-data.json", import.meta.url);
const token = process.env.GITHUB_TOKEN;

function safeReleasePath(tag) {
  return String(tag).replace(/[^A-Za-z0-9._-]/g, "-");
}

const headers = {
  Accept: "application/vnd.github+json",
  "User-Agent": "vmsh-website-build",
  "X-GitHub-Api-Version": "2022-11-28",
};

if (token) {
  headers.Authorization = `Bearer ${token}`;
}

const response = await fetch(apiUrl, { headers });

if (!response.ok) {
  throw new Error(
    `Could not load the latest vmsh release: ${response.status} ${response.statusText}`,
  );
}

const latest = await response.json();
const releaseDirectory = new URL(`../public/tours/${safeReleasePath(latest.tag_name)}/`, import.meta.url);
await mkdir(releaseDirectory, { recursive: true });
const platforms = {
  darwin: "macOS",
  linux: "Linux",
  windows: "Windows",
};
const architectures = {
  amd64: "x64",
  arm64: "ARM64",
};
const productOrder = {
  NeurodeskAppX: 0,
  SquadVM: 1,
  vmsh: 2,
};
const platformOrder = {
  macOS: 0,
  Windows: 1,
  Linux: 2,
};
const architectureOrder = {
  "Apple silicon": 0,
  x64: 0,
  ARM64: 1,
};

function parseAsset(asset) {
  const match = asset.name.match(
    /^(NeurodeskAppX|SquadVM|vmsh)_.+_(darwin|linux|windows)_(amd64|arm64)(?:\.zip|\.exe)?$/,
  );

  if (!match) return null;

  const [, product, os, architecture] = match;
  const arch =
    os === "darwin" && architecture === "arm64"
      ? "Apple silicon"
      : architectures[architecture];

  return {
    product,
    name: asset.name,
    platform: platforms[os],
    arch,
    size: asset.size,
    url: asset.browser_download_url,
  };
}

const assets = latest.assets
  .map(parseAsset)
  .filter(Boolean)
  .sort(
    (a, b) =>
      productOrder[a.product] - productOrder[b.product] ||
      platformOrder[a.platform] - platformOrder[b.platform] ||
      architectureOrder[a.arch] - architectureOrder[b.arch],
  );

async function parseTourAsset(asset) {
  if (!asset.name.endsWith(".cast")) return null;
  const castResponse = await fetch(asset.browser_download_url, { headers });
  if (!castResponse.ok) {
    throw new Error(`Could not load released tour ${asset.name}: ${castResponse.status}`);
  }
  const cast = await castResponse.text();
  const firstLine = cast.split(/\r?\n/, 1)[0];
  const header = JSON.parse(firstLine);
  if (header.version !== 2 || header.vmsh_tour?.schema !== 1) {
    throw new Error(`Released tour ${asset.name} has an unsupported header.`);
  }
  const filename = `${header.vmsh_tour.id}.cast`;
  await writeFile(new URL(filename, releaseDirectory), cast);
  return {
    id: header.vmsh_tour.id,
    title: header.vmsh_tour.title,
    url: `/tours/${safeReleasePath(latest.tag_name)}/${filename}`,
  };
}

const tours = (await Promise.all(latest.assets.map(parseTourAsset)))
  .filter(Boolean)
  .sort((a, b) => a.id.localeCompare(b.id));

if (!assets.some((asset) => asset.product === "NeurodeskAppX")) {
  throw new Error("The latest release does not contain NeurodeskAppX.");
}

if (!assets.some((asset) => asset.product === "SquadVM")) {
  throw new Error("The latest release does not contain SquadVM.");
}

if (!assets.some((asset) => asset.product === "vmsh")) {
  throw new Error("The latest release does not contain vmsh.");
}

const checksumAsset = latest.assets.find(
  (asset) => asset.name === "checksums.txt",
);

const releaseData = {
  tag: latest.tag_name,
  releaseUrl: latest.html_url,
  checksums:
    checksumAsset?.browser_download_url ??
    `https://github.com/${repository}/releases/tag/${latest.tag_name}`,
  tours,
  assets,
};

await writeFile(outputUrl, `${JSON.stringify(releaseData, null, 2)}\n`);
console.log(`Prepared ${assets.length} downloads and ${tours.length} tours from vmsh ${latest.tag_name}.`);
