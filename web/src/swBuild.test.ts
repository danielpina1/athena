import { expect, test } from "vitest";
import { precacheBuild } from "./swBuild";

const bundle = (index: string, assets: string[]) =>
  Object.fromEntries([["index.html", { source: index }], ["sw.js", {}], [".vite/manifest.json", {}], ...assets.map((a) => [a, {}])]);

test("the precache list is the build's hashed files, sorted", () => {
  const b = precacheBuild(bundle("<html>", ["assets/b-2.js", "assets/a-1.css"]));
  expect(b.assets).toEqual(["/assets/a-1.css", "/assets/b-2.js"]);
});

test("the version changes with index.html even when no asset does", () => {
  const assets = ["assets/a-1.js"];
  const one = precacheBuild(bundle('<html><meta name="theme-color" content="#FFFFFF">', assets)).version;
  const two = precacheBuild(bundle('<html><meta name="theme-color" content="#000000">', assets)).version;
  expect(one).not.toBe(two);
  expect(precacheBuild(bundle('<html><meta name="theme-color" content="#FFFFFF">', assets)).version).toBe(one);
  expect(precacheBuild(bundle('<html><meta name="theme-color" content="#FFFFFF">', ["assets/a-2.js"])).version).not.toBe(one);
});

test("a build without index.html is refused", () => {
  expect(() => precacheBuild({ "assets/a-1.js": {} })).toThrow(/index\.html/);
});
