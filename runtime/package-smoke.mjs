import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import process from "node:process";
import { fileURLToPath, pathToFileURL } from "node:url";

const sourceRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const fixture = join(sourceRoot, "runtime", "package-smoke-model.mjs");
const quotaFixture = join(sourceRoot, "runtime", "pi-quota-model.mjs");
const quotaCredentialSentinel = "SYNTHETIC-QUOTA-CREDENTIAL-SENTINEL";
const entryObserver = join(sourceRoot, "runtime", "package-smoke-entry.mjs");
const request = JSON.stringify({
  version: 1,
  operation: "compose",
  model: { provider: "openai-codex", id: "gpt-5.6-luna", thinking: "low" },
  input: { user: "Synthetic package user", assistant: "Synthetic package assistant" },
  label: { current: "", refresh: true },
});
const quotaRequest = JSON.stringify({
  version: 1,
  operation: "quota",
  model: {
    provider: "openai-codex",
    id: "gpt-5.6-luna",
    base_url: "https://chatgpt.com/backend-api",
  },
});

/** @param {string} name */
function option(name) {
  const index = process.argv.indexOf(name);
  if (index === -1 || process.argv[index + 1] === undefined) throw new Error(`${name.slice(2)} is required`);
  return resolve(process.argv[index + 1]);
}

/** @param {string} path @param {string} label */
function requireDirectory(path, label) {
  if (!existsSync(path)) throw new Error(`${label} is unavailable`);
}

/** @param {string} path @param {string} label */
function requireFile(path, label) {
  if (!existsSync(path)) throw new Error(`${label} is unavailable`);
}

/** @param {string} command @param {string[]} args @param {NodeJS.ProcessEnv} env @param {string} cwd */
function run(command, args, env, cwd) {
  const result = spawnSync(command, args, { cwd, encoding: "utf8", env, timeout: 15_000, maxBuffer: 1024 * 1024 });
  if (result.error !== undefined) throw result.error;
  assert.equal(result.status, 0, result.stderr);
  return result.stdout;
}

/** @param {string} marker */
function readEntryMarker(marker) {
  if (statSync(marker).size > 1024) throw new Error("Pi entry marker exceeds its bounded size");
  /** @type {unknown} */
  const value = JSON.parse(readFileSync(marker, "utf8"));
  if (typeof value !== "object" || value === null || Array.isArray(value)) throw new Error("Pi entry marker is invalid");
  const record = /** @type {Record<string, unknown>} */ (value);
  if (Object.keys(record).length !== 2 || typeof record.entry !== "string" || typeof record.node !== "string") {
    throw new Error("Pi entry marker is invalid");
  }
  return { entry: record.entry, node: record.node };
}

/** @param {string} defaultPath @param {string} runtimePath @param {string} label */
function requirePairedAsset(defaultPath, runtimePath, label) {
  requireFile(defaultPath, `default packaged ${label}`);
  requireFile(runtimePath, `runtime ${label}`);
  assert.equal(realpathSync(defaultPath), realpathSync(runtimePath), `default packaged ${label} is not paired with runtime`);
}

function main() {
  const packageRoot = option("--package-root");
  const runtimeRoot = option("--runtime-root");
  requireDirectory(packageRoot, "package root");
  requireDirectory(runtimeRoot, "runtime root");

  const steward = join(packageRoot, "bin", "steward");
  const defaultHelper = join(packageRoot, "bin", "steward-pi-helper");
  const defaultPi = join(packageRoot, "bin", "pi");
  const defaultRuntime = join(packageRoot, "lib", "steward", "runtime");
  const defaultModules = join(packageRoot, "lib", "steward", "node_modules");
  const runtimeHelper = join(runtimeRoot, "bin", "steward-pi-helper");
  const runtimePi = join(runtimeRoot, "bin", "pi");
  const runtimeRuntime = join(runtimeRoot, "lib", "steward", "runtime");
  const runtimeModules = join(runtimeRoot, "lib", "steward", "node_modules");
  const packageNames = ["@earendil-works/pi-coding-agent", "@earendil-works/pi-ai", "@earendil-works/pi-tui", "@tintinweb/pi-subagents"];

  requireFile(steward, "packaged steward");
  requireFile(defaultHelper, "default packaged helper");
  requireFile(runtimeHelper, "runtime helper");
  requireFile(defaultPi, "default packaged Pi");
  requireFile(runtimePi, "runtime Pi");
  requirePairedAsset(join(defaultRuntime, "pi-child-probe.mjs"), join(runtimeRuntime, "pi-child-probe.mjs"), "runtime probe");
  for (const name of packageNames) {
    requirePairedAsset(join(defaultModules, name, "package.json"), join(runtimeModules, name, "package.json"), `${name} manifest`);
  }
  const defaultTuiEntry = join(defaultModules, "@earendil-works", "pi-coding-agent", "dist", "cli.js");
  const runtimeTuiEntry = join(runtimeModules, "@earendil-works", "pi-coding-agent", "dist", "cli.js");
  requirePairedAsset(defaultTuiEntry, runtimeTuiEntry, "unbundled Pi entry");

  const root = mkdtempSync(join(tmpdir(), "steward-package-smoke-"));
  try {
    const home = join(root, "home");
    const marker = join(root, "model.jsonl");
    const quotaMarker = join(root, "quota.jsonl");
    const entryMarker = join(root, "entry.json");
    const environment = {
      HOME: home,
      XDG_CACHE_HOME: join(home, ".cache"),
      XDG_CONFIG_HOME: join(home, ".config"),
      XDG_DATA_HOME: join(home, ".local", "share"),
      XDG_RUNTIME_DIR: join(root, "runtime"),
      PI_CODING_AGENT_DIR: join(home, ".pi", "agent"),
      PATH: join(packageRoot, "bin"),
      PI_OFFLINE: "1",
      PI_SKIP_VERSION_CHECK: "1",
      PI_TELEMETRY: "0",
    };
    const piEnvironment = { ...environment, STEWARD_PACKAGE_SMOKE_ENTRY_MARKER: entryMarker, NODE_OPTIONS: `--import ${entryObserver}` };
    assert.match(run(defaultPi, ["--version"], piEnvironment, root), /0\.85\.1/);
    const executed = readEntryMarker(entryMarker);
    assert.notEqual(realpathSync(executed.entry), realpathSync(join(runtimeModules, "@earendil-works", "pi-coding-agent", "dist", "bundle", "cli.js")), "Pi wrapper executed the bundled Pi entry");
    assert.equal(realpathSync(executed.entry), realpathSync(runtimeTuiEntry), "Pi wrapper did not execute the unbundled Pi entry");
    assert.match(run(executed.node, ["--version"], environment, root), /^v24\./, "Pi wrapper did not use Node 24");

    const identity = run(executed.node, ["--input-type=module", "--eval", `
      import { createRequire, findPackageJSON } from "node:module";
      import { realpathSync } from "node:fs";
      const entry = ${JSON.stringify(pathToFileURL(executed.entry).href)};
      const require = createRequire(entry);
      const names = ${JSON.stringify(packageNames)};
      const result = Object.fromEntries(names.map((name) => [name, realpathSync(findPackageJSON(name, entry))]));
      const subagents = require.resolve("@tintinweb/pi-subagents/dist/index.js");
      for (const name of names.slice(0, 3)) if (realpathSync(findPackageJSON(name, subagents)) !== result[name]) throw new Error(name + " has a duplicate physical graph");
      console.log(JSON.stringify({ entry: realpathSync(new URL(entry)), manifests: result }));
    `], environment, root);
    assert.match(identity, /pi-coding-agent/);

    const extensions = ["pi-subagents.mjs", "pi-extension.mjs", "pi-child-probe.mjs"].map((name) => join(defaultRuntime, name));
    mkdirSync(environment.PI_CODING_AGENT_DIR, { recursive: true });
    writeFileSync(join(environment.PI_CODING_AGENT_DIR, "settings.json"), `${JSON.stringify({ extensions })}\n`);
    const probe = run(executed.node, ["--input-type=module", "--eval", `
      import { runProbe } from ${JSON.stringify(pathToFileURL(join(defaultRuntime, "pi-child-probe.mjs")).href)};
      await runProbe();
    `], environment, root);
    const proof = JSON.parse(probe);
    assert.equal(proof.proofMet, true);
    assert.equal(proof.assertions.activePiSdkAiTuiGraphShared, true);
    assert.equal(proof.assertions.zeroNetworkAttempts, true);

    const helperEnvironment = { ...environment, STEWARD_PACKAGE_SMOKE_MARKER: marker, NODE_OPTIONS: `--import ${fixture}` };
    const helperResult = spawnSync(defaultHelper, [], { cwd: root, input: request, encoding: "utf8", env: helperEnvironment, timeout: 15_000 });
    assert.equal(helperResult.status, 0, helperResult.stderr);
    assert.equal(helperResult.stderr, "");
    assert.deepEqual(JSON.parse(helperResult.stdout), { version: 1, ok: true, body: "Synthetic package outcome", label: "Synthetic Package Label" });
    assert.equal(readFileSync(marker, "utf8").trim().split("\n").length, 1);

    const quotaEnvironment = {
      ...environment,
      STEWARD_QUOTA_FIXTURE_MARKER: quotaMarker,
      NODE_OPTIONS: `--import ${quotaFixture}`,
    };
    const quotaResult = spawnSync(defaultHelper, [], {
      cwd: root,
      input: quotaRequest,
      encoding: "utf8",
      env: quotaEnvironment,
      timeout: 15_000,
    });
    assert.equal(quotaResult.status, 0, quotaResult.stderr);
    assert.equal(quotaResult.stderr, "");
    const quota = JSON.parse(quotaResult.stdout);
    assert.doesNotMatch(quotaResult.stdout, new RegExp(quotaCredentialSentinel));
    assert.equal(quota.version, 1);
    assert.equal(quota.ok, true);
    assert.equal(quota.provider, "openai-codex");
    assert.equal(quota.base_url, "https://chatgpt.com/backend-api");
    assert.match(quota.account_key, /^[a-f0-9]{64}$/);
    assert.deepEqual({
      five_hour: quota.windows.five_hour?.remaining_percent,
      weekly: quota.windows.weekly?.remaining_percent,
    }, { five_hour: 75, weekly: 20 });
    const quotaObservation = JSON.parse(readFileSync(quotaMarker, "utf8"));
    assert.deepEqual(quotaObservation, {
      phase: "complete",
      getAuthCalls: 1,
      fetchCalls: 1,
      authorizationMatches: true,
      accountMatches: true,
    });

    const invalid = spawnSync(defaultHelper, [], { cwd: root, input: "{}", encoding: "utf8", env: helperEnvironment, timeout: 15_000 });
    assert.equal(invalid.status, 1);
    assert.equal(invalid.stderr, "");
    assert.deepEqual(JSON.parse(invalid.stdout), { version: 1, ok: false, error: "invalid_request" });
  } finally {
    rmSync(root, { force: true, recursive: true });
  }
}

try {
  main();
} catch (error) {
  process.stderr.write(`${error instanceof Error ? error.message : "package smoke failed"}\n`);
  process.exitCode = 1;
}
