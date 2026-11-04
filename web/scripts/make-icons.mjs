// Draws the app's icons once into public/icons/ (the PNGs are committed): an orange (--accent) square with a black
// (--accent-fg) "A" in IBM Plex Sans 600, rendered by Playwright's Chromium. Run it again only to change the icon:
//   node web/scripts/make-icons.mjs
// The maskable icon keeps the letter inside the central 80 % circle that a launcher's mask always shows.
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { chromium } from "@playwright/test";

const web = join(dirname(fileURLToPath(import.meta.url)), "..");
const font = readFileSync(join(web, "node_modules/@fontsource/ibm-plex-sans/files/ibm-plex-sans-latin-600-normal.woff2"));
const out = join(web, "public/icons");

// name, size in px, and the letter's size as a share of the icon.
const icons = [
  ["icon-192.png", 192, 0.62],
  ["icon-512.png", 512, 0.62],
  ["icon-maskable-512.png", 512, 0.44],
  ["apple-touch-icon.png", 180, 0.62],
];

const browser = await chromium.launch();
try {
  const page = await browser.newPage({ deviceScaleFactor: 1 });
  for (const [name, size, letter] of icons) {
    await page.setViewportSize({ width: size, height: size });
    await page.setContent(`<!doctype html><html><head><style>
      @font-face { font-family: "Plex"; font-weight: 600; src: url(data:font/woff2;base64,${font.toString("base64")}) format("woff2"); }
      html, body { margin: 0; }
      div { width: ${size}px; height: ${size}px; background: #FF5A1F; color: #0A0A0A; display: grid; place-items: center;
        font: 600 ${Math.round(size * letter)}px/1 "Plex"; }
      /* Optical centre: the cap height sits a little high in its line box. */
      span { transform: translateY(${(size * letter * 0.04).toFixed(1)}px); }
    </style></head><body><div><span>A</span></div></body></html>`);
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({ path: join(out, name), clip: { x: 0, y: 0, width: size, height: size } });
    console.log(`wrote public/icons/${name}`);
  }
} finally {
  await browser.close();
}
