import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const release = JSON.parse(
  await readFile(new URL("../app/release-data.json", import.meta.url), "utf8"),
);
const html = await readFile(new URL("../out/index.html", import.meta.url), "utf8");

test("static site renders the default app downloads and every vmsh download", () => {
  const renderedDownloads = release.assets.filter(
    (asset) => asset.product === "vmsh" || asset.platform === "macOS",
  );

  for (const asset of renderedDownloads) {
    assert.match(
      html,
      new RegExp(asset.url.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")),
    );
  }

  assert.match(
    html,
    new RegExp(release.checksums.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")),
  );
  assert.doesNotMatch(html, /codex-preview|react-loading-skeleton/);
});

test("release data contains the three downloadable products", () => {
  const products = new Set(release.assets.map((asset) => asset.product));
  assert.deepEqual(
    [...products].sort(),
    ["NeurodeskAppX", "SquadVM", "vmsh"].sort(),
  );

  for (const asset of release.assets) {
    assert.ok(asset.url.startsWith("https://github.com/tinyrange/vmsh/releases/"));
    assert.ok(asset.size > 0);
  }
});
