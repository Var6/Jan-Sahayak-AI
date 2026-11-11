import type { NextConfig } from "next";

// Pure static export, served by central-api at /admin/ (JS_ADMIN_UI_DIR).
// No server code: no route handlers, Server Functions, middleware or
// image optimisation. Inline hydration scripts are allowed by central-api
// through per-page sha256 hashes written by scripts/csp-hashes.mjs.
const config: NextConfig = {
  output: "export",
  basePath: "/admin",
  trailingSlash: true,
  reactStrictMode: true,
  poweredByHeader: false,
  images: { unoptimized: true },
  typedRoutes: false,
};

export default config;
