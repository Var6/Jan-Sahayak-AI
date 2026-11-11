import react from "@vitejs/plugin-react";
import type { Plugin } from "vite";
import { defineConfig } from "vitest/config";
import { deviceApiMock } from "./mock/deviceApiMock.ts";

/** Device agent local API (docs/API_DOCUMENTATION.md §1). */
const AGENT = "http://127.0.0.1:8730";

// Strict CSP for the built kiosk: everything is same-origin, nothing external.
// Only injected at build time (the dev server needs inline HMR scripts).
const CSP = [
  "default-src 'self'",
  "script-src 'self'",
  "style-src 'self'",
  "img-src 'self' data:",
  "font-src 'self'",
  "media-src 'self'",
  "connect-src 'self'",
  "worker-src 'self'",
  "object-src 'none'",
  "base-uri 'none'",
  "form-action 'none'",
].join("; ");

function cspMeta(): Plugin {
  return {
    name: "jan-sahayak-csp",
    apply: "build",
    transformIndexHtml: () => [
      {
        tag: "meta",
        attrs: { "http-equiv": "Content-Security-Policy", content: CSP },
        injectTo: "head-prepend",
      },
    ],
  };
}

export default defineConfig(({ mode }) => ({
  // Relative asset paths: the Go agent embeds dist/ and serves it at `/`.
  base: "./",
  plugins: [react(), cspMeta(), ...(mode === "mock" ? [deviceApiMock()] : [])],
  server: {
    host: "127.0.0.1",
    port: 5173,
    strictPort: true,
    proxy:
      mode === "mock"
        ? undefined
        : {
            "/api": {
              target: AGENT,
              // The agent checks Host (DNS-rebinding protection) and rejects
              // cross-site Origin on mutating calls.
              changeOrigin: true,
              headers: { Origin: AGENT },
            },
          },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    target: "chrome120",
    assetsInlineLimit: 0, // keep fonts/worklet as files (CSP: no data: fonts)
    sourcemap: false,
    reportCompressedSize: true,
  },
  test: {
    environment: "happy-dom",
    include: ["src/**/*.test.{ts,tsx}"],
    setupFiles: ["src/test/setup.ts"],
    pool: "threads",
    maxWorkers: 2,
    testTimeout: 10_000,
  },
}));
