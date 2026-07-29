"use client";

import { useEffect, useMemo, useState } from "react";
import release from "./release-data.json";

type Platform = "macOS" | "Windows" | "Linux";

type Download = {
  name: string;
  url: string;
  size: number;
  platform: Platform;
  arch: string;
};

type Product = {
  key: "NeurodeskAppX" | "SquadVM";
  name: string;
  eyebrow: string;
  description: string;
  image: string;
  imageClass: string;
  accent: "green" | "purple";
};

const products: Product[] = [
  {
    key: "NeurodeskAppX",
    name: "NeurodeskAppX",
    eyebrow: "RESEARCH DESKTOP",
    description:
      "A ready-to-use desktop for reproducible neuroimaging. Open it and get to work.",
    image: "neurodesk-wordmark.png",
    imageClass: "neurodesk-wordmark",
    accent: "green",
  },
  {
    key: "SquadVM",
    name: "SquadVM",
    eyebrow: "CYBER LAB",
    description:
      "A complete cyber security workspace for UQ Cyber Squad. One download, no setup.",
    image: "squadvm-brand.png",
    imageClass: "squadvm-brand",
    accent: "purple",
  },
];

function prettySize(bytes: number) {
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}

function detectPlatform(): Platform {
  const platform = navigator.platform.toLowerCase();
  const userAgent = navigator.userAgent.toLowerCase();

  if (platform.includes("mac") || userAgent.includes("mac os")) return "macOS";
  if (platform.includes("win") || userAgent.includes("windows")) return "Windows";
  return "Linux";
}

function productDownloads(product: Product["key"]): Download[] {
  return release.assets.filter((asset) => asset.product === product) as Download[];
}

function DownloadCard({ product }: { product: Product }) {
  const downloads = useMemo(() => productDownloads(product.key), [product.key]);
  const [platform, setPlatform] = useState<Platform>("macOS");

  useEffect(() => {
    const detected = detectPlatform();
    if (downloads.some((download) => download.platform === detected)) {
      setPlatform(detected);
    }
  }, [downloads]);

  const selected =
    downloads.find((download) => download.platform === platform) ?? downloads[0];

  return (
    <article className={`product-card ${product.accent}`}>
      <div className="product-visual">
        <span className="product-eyebrow">{product.eyebrow}</span>
        <img
          className={product.imageClass}
          src={product.image}
          alt={`${product.name} logo`}
        />
      </div>

      <div className="product-content">
        <h2>{product.name}</h2>
        <p>{product.description}</p>

        <div className="platform-switcher" aria-label={`${product.name} platform`}>
          {downloads.map((download) => (
            <button
              className={download.platform === platform ? "active" : ""}
              key={download.name}
              onClick={() => setPlatform(download.platform)}
              type="button"
            >
              {download.platform}
            </button>
          ))}
        </div>

        <a className="download-button" href={selected.url}>
          <span>Download for {selected.platform}</span>
          <span className="button-meta">
            {selected.arch} · {prettySize(selected.size)}
          </span>
        </a>
      </div>
    </article>
  );
}

export default function Home() {
  const vmshDownloads = release.assets.filter(
    (asset) => asset.product === "vmsh",
  );

  return (
    <main>
      <header className="site-header">
        <a className="vmsh-logo" href="#" aria-label="vmsh home">
          <span className="prompt-mark">&gt;_</span>
          <span>vmsh</span>
        </a>
        <nav aria-label="Main navigation">
          <a href="#apps">Apps</a>
          <a href="#vmsh">vmsh</a>
          <a
            className="github-link"
            href="https://github.com/tinyrange/vmsh"
          >
            GitHub <span aria-hidden="true">↗</span>
          </a>
        </nav>
      </header>

      <div className="landing-fold">
        <section className="hero" id="apps">
          <div className="hero-title">
            <p className="kicker">
              <span />
              DESKTOP VIRTUAL MACHINES
            </p>
            <h1>
              Your lab,
              <br />
              <em>ready to run.</em>
            </h1>
          </div>
          <div className="hero-details">
            <p className="hero-description">
              Purpose-built desktops powered by vmsh. Download, launch, and start
              working.
            </p>
            <div className="release-line">
              <span className="status-dot" />
              Latest release {release.tag}
            </div>
          </div>
        </section>

        <section className="products" aria-label="Desktop applications">
          {products.map((product) => (
            <DownloadCard key={product.key} product={product} />
          ))}
        </section>
      </div>

      <section className="vmsh-section" id="vmsh">
        <div className="vmsh-intro">
          <p className="kicker">
            <span />
            FOR DEVELOPERS
          </p>
          <h2>
            One shell.
            <br />
            <em>Any system.</em>
          </h2>
          <p>
            vmsh makes local virtual machines feel like part of your shell.
            Switch systems, keep your context, and run ordinary commands.
          </p>
          <a
            className="text-link"
            href="https://github.com/tinyrange/vmsh#readme"
          >
            Read the documentation <span aria-hidden="true">→</span>
          </a>
        </div>

        <div className="cli-panel">
          <div className="cli-command">
            <span>$</span>
            <code>vmsh</code>
          </div>
          <div className="cli-downloads">
            {vmshDownloads.map((download) => (
              <a href={download.url} key={download.name}>
                <span>
                  <strong>{download.platform}</strong>
                  <small>{download.arch}</small>
                </span>
                <span className="asset-size">{prettySize(download.size)}</span>
                <span aria-hidden="true">↓</span>
              </a>
            ))}
          </div>
          <a className="checksums" href={release.checksums}>
            SHA256 checksums <span aria-hidden="true">↗</span>
          </a>
        </div>
      </section>

      <footer>
        <a className="vmsh-logo" href="#">
          <span className="prompt-mark">&gt;_</span>
          <span>vmsh</span>
        </a>
        <p>Virtual machines that belong in your shell.</p>
        <span>Open source · {release.tag}</span>
      </footer>
    </main>
  );
}
