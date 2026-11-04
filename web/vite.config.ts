/// <reference types="vitest/config" />
import { fileURLToPath } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

const devServer = process.env.ATHENA_DEV_SERVER ?? "http://127.0.0.1:8443";
const origin = new URL(devServer).origin;

// The dev server forwards the server's own routes, with the Origin the server expects.
const proxied = { target: devServer, changeOrigin: true, headers: { Origin: origin } };

export default defineConfig({
  root: fileURLToPath(new URL(".", import.meta.url)),
  plugins: [react(), tailwindcss()],
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
    globals: false,
  },
});
