/// <reference types="vitest/config" />
import { fileURLToPath } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig, type Plugin } from "vite";
import { type BundleLike, precacheBuild } from "./src/swBuild";

const devServer = process.env.ATHENA_DEV_SERVER ?? "http://localhost:8443";
const origin = new URL(devServer).origin;

// The dev server forwards the server's own routes, with the Origin the server expects.
const proxied = { target: devServer, changeOrigin: true, headers: { Origin: origin } };

const src = (p: string) => fileURLToPath(new URL(p, import.meta.url));

/**
 * Writes the precache list into the service worker (src/sw.ts reads it as __ATHENA_BUILD__): every hashed file of
 * this build under assets/, the lazy chunks included, and a version made from that list and index.html, which names
 * the worker's cache (src/swBuild.ts). A new build therefore changes sw.js, and the browser installs the new worker
 * beside the old one. It runs last, once index.html is in the bundle.
 */
function precachePlugin(): Plugin {
  const token = "__ATHENA_BUILD__";
  return {
    name: "athena-precache",
    apply: "build",
    enforce: "post",
    generateBundle(_, bundle) {
      const sw = bundle["sw.js"];
      if (!sw || sw.type !== "chunk") return this.error("precache: the build has no sw.js chunk");
      if (!sw.code.includes(token)) return this.error(`precache: sw.js does not read ${token}`);
      if (sw.imports.length > 0 || sw.exports.length > 0) return this.error("precache: sw.js must not import or export");
      const build = precacheBuild(bundle as unknown as BundleLike);
      sw.code = sw.code.replaceAll(token, JSON.stringify(build));
    },
  };
}

export default defineConfig({
  root: fileURLToPath(new URL(".", import.meta.url)),
  plugins: [react(), tailwindcss(), precachePlugin()],
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  build: {
    outDir: "../internal/web/ui/dist",
    emptyOutDir: true,
    manifest: true,
    assetsDir: "assets",
    // No inline script or style: the CSP forbids both.
    assetsInlineLimit: 0,
    cssCodeSplit: true,
    modulePreload: { polyfill: false },
    rolldownOptions: {
      // The app, and the service worker as /sw.js (not hashed: its URL is its identity, and the server revalidates it).
      input: { index: src("./index.html"), sw: src("./src/sw.ts") },
      output: { entryFileNames: (chunk) => (chunk.name === "sw" ? "sw.js" : "assets/[name]-[hash].js") },
    },
  },
  server: {
    proxy: {
      "/ws": { ...proxied, ws: true },
      "/auth": proxied,
      "/push": proxied,
      "/api": proxied,
    },
  },
  test: {
    environment: "jsdom",
    include: ["src/**/*.test.{ts,tsx}"],
    setupFiles: ["src/test/setup.ts"],
    globals: false,
  },
});
