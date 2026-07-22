import { readFile, writeFile, mkdir, readdir } from "node:fs/promises";
import { stripTypeScriptTypes } from "node:module";
import { fileURLToPath } from "node:url";
import path from "node:path";
import process from "node:process";

const requiredNode = "24.11.1";
const root = path.dirname(fileURLToPath(import.meta.url));
const checkOnly = process.argv.includes("--check");
const outputs = [
  { source: "src/index.html", destination: "dist/index.html", type: "copy" },
  { source: "src/styles.css", destination: "dist/assets/styles.css", type: "copy" },
  { source: "src/state.ts", destination: "dist/assets/state.js", type: "typescript" },
  { source: "src/app.ts", destination: "dist/assets/app.js", type: "typescript" }
];

if (process.versions.node !== requiredNode) {
  throw new Error(`WEB_NODE_VERSION_MISMATCH: required ${requiredNode}, found ${process.versions.node}`);
}
if (process.argv.slice(2).some((argument) => argument !== "--check")) {
  throw new Error("WEB_BUILD_ARGUMENT_INVALID");
}

function normalize(value) {
  return value
    .replace(/\r\n/g, "\n")
    .replace(/\r/g, "\n")
    .split("\n")
    .map((line) => line.trimEnd())
    .join("\n")
    .replace(/\s*$/, "") + "\n";
}

async function expectedContents(entry) {
  const source = normalize(await readFile(path.join(root, entry.source), "utf8"));
  if (entry.type === "typescript") {
    return normalize(stripTypeScriptTypes(source, { mode: "strip" }));
  }
  return source;
}

async function listFiles(directory, prefix = "") {
  let entries;
  try {
    entries = await readdir(directory, { withFileTypes: true });
  } catch (error) {
    if (error.code === "ENOENT") return [];
    throw error;
  }
  const result = [];
  for (const entry of entries) {
    const relative = path.posix.join(prefix, entry.name);
    if (entry.isDirectory()) result.push(...await listFiles(path.join(directory, entry.name), relative));
    else if (entry.isFile()) result.push(relative);
    else throw new Error(`WEB_DIST_UNSUPPORTED_ENTRY:${relative}`);
  }
  return result.sort();
}

const expectedFiles = outputs.map((entry) => entry.destination.replace(/^dist\//, "")).sort();
const actualFiles = await listFiles(path.join(root, "dist"));
const unexpected = actualFiles.filter((file) => !expectedFiles.includes(file));
if (unexpected.length !== 0) throw new Error(`WEB_DIST_UNEXPECTED_FILE:${unexpected.join(",")}`);

for (const entry of outputs) {
  const expected = await expectedContents(entry);
  const destination = path.join(root, entry.destination);
  if (checkOnly) {
    let actual;
    try {
      actual = await readFile(destination, "utf8");
    } catch (error) {
      if (error.code === "ENOENT") throw new Error(`WEB_DIST_MISSING:${entry.destination}`);
      throw error;
    }
    if (actual !== expected) throw new Error(`WEB_DIST_STALE:${entry.destination}`);
  } else {
    await mkdir(path.dirname(destination), { recursive: true });
    await writeFile(destination, expected, "utf8");
  }
}

if (!checkOnly) process.stdout.write("web/dist\n");
