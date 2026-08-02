import type { NextConfig } from "next";

const onGitHubPages = process.env.VMSH_GITHUB_PAGES === "1";

const nextConfig: NextConfig = {
  output: "export",
  trailingSlash: true,
  basePath: onGitHubPages ? "/vmsh" : "",
  env: {
    NEXT_PUBLIC_VMSH_BASE_PATH: onGitHubPages ? "/vmsh" : "",
  },
  images: {
    unoptimized: true,
  },
};

export default nextConfig;
