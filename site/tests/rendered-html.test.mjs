import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const release = JSON.parse(
  await readFile(new URL("../app/release-data.json", import.meta.url), "utf8"),
);

async function render(path = "/") {
  const workerUrl = new URL("../dist/server/index.js", import.meta.url);
  workerUrl.searchParams.set("test", `${process.pid}-${Date.now()}`);
  const { default: worker } = await import(workerUrl.href);

  return worker.fetch(
    new Request(`http://localhost${path}`, {
      headers: { accept: "text/html" },
    }),
    {
      ASSETS: {
        fetch: async () => new Response("Not found", { status: 404 }),
      },
    },
    {
      waitUntil() {},
      passThroughOnException() {},
    },
  );
}

test("server renders the default app downloads and every vmsh download", async () => {
  const response = await render();
  assert.equal(response.status, 200);
  assert.match(response.headers.get("content-type") ?? "", /^text\/html\b/i);

  const html = await response.text();
  const renderedDownloads = release.assets.filter(
    (asset) => asset.product === "vmsh" || asset.platform === "macOS",
  );
  for (const asset of renderedDownloads) {
    assert.match(html, new RegExp(asset.url.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
  }
  assert.match(
    html,
    new RegExp(release.checksums.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")),
  );
  assert.equal(
    html.split("Requires macOS 15 or newer.").length - 1,
    2,
    "both desktop applications should document the macOS requirement",
  );
  assert.doesNotMatch(html, /codex-preview|react-loading-skeleton/);
});

test("the guided tour route and enhanced cast are published together", async () => {
  const response = await render("/tours/context-switching/");
  assert.equal(response.status, 200);
  const html = await response.text();
  assert.match(html, /Move between host and VM contexts/);
  assert.match(html, /TESTED DOCUMENTATION/);

  const cast = await readFile(
    new URL("../public/tours/context-switching.cast", import.meta.url),
    "utf8",
  );
  const lines = cast.trim().split(/\r?\n/);
  const header = JSON.parse(lines[0]);
  assert.equal(header.version, 2);
  assert.equal(header.vmsh_tour.schema, 1);
  assert.equal(header.vmsh_tour.id, "context-switching");
  const sections = lines
    .slice(1)
    .map((line) => JSON.parse(line))
    .filter((event) => event[1] === "vmsh" && event[2]?.name === "vmsh.tour.section");
  assert.ok(sections.length > 0, "the cast should contain guided sections");
  assert.ok(
    lines.slice(1).map((line) => JSON.parse(line)).filter((event) => event[1] === "m")
      .every((event) => typeof event[2] === "string"),
    "ordinary asciinema markers should use string labels",
  );
});

test("the tour catalog links every available guided tour", async () => {
  const response = await render("/tours/");
  assert.equal(response.status, 200);
  const html = await response.text();
  assert.match(html, /Choose a workflow/);
  assert.match(html, /\/tours\/context-switching/);
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
