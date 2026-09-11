import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { realpathSync } from "node:fs";
import { copyFile, mkdir, mkdtemp, readFile, rm, symlink, writeFile } from "node:fs/promises";
import { createRequire, findPackageJSON } from "node:module";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { fileURLToPath, pathToFileURL } from "node:url";

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const CHILD_CONTEXT_PATH = join(REPOSITORY_ROOT, "runtime", "pi-child-context.mjs");
const CHILD_PROBE_PATH = join(REPOSITORY_ROOT, "runtime", "pi-child-probe.mjs");
const OWNED_ADAPTER_PATH = join(REPOSITORY_ROOT, "runtime", "pi-extension.mjs");
const SUBAGENTS_WRAPPER_PATH = join(REPOSITORY_ROOT, "runtime", "pi-subagents.mjs");
const COUPLING_ERROR = "Steward cannot verify the pinned Pi child-context coupling";
const EXPECTED_INDEX_SHA256 = "ee8cdc6e9b0dd95c47b5416be2f48089252db98edfcc6669f417c16a58b79ec5";
const EXPECTED_CHILD_CONTEXT_SHA256 = "318617894e4994a3052bfcf5b42a071503de8e12a3378976dfdb6bbcba94ef2d";
const nativeRequire = createRequire(import.meta.url);

/** @param {unknown} value @returns {value is Record<string, unknown>} */
function isRecord(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** @param {string} path @returns {Promise<unknown>} */
async function readJson(path) {
  return JSON.parse(await readFile(path, "utf8"));
}

/** @param {unknown} value @param {string} name @returns {Record<string, unknown>} */
function recordProperty(value, name) {
  assert.ok(isRecord(value), `${name} must be an object`);
  return value;
}

/** @param {Record<string, unknown>} value @param {string} name @returns {string} */
function stringProperty(value, name) {
  const property = value[name];
  if (typeof property !== "string") {
    throw new TypeError(`${name} must be a string`);
  }
  return property;
}

/** @param {Record<string, unknown>} value @param {string} name @returns {boolean} */
function booleanProperty(value, name) {
  const property = value[name];
  if (typeof property !== "boolean") {
    throw new TypeError(`${name} must be a boolean`);
  }
  return property;
}

/** @param {unknown} error @returns {boolean} */
function isFixedCouplingError(error) {
  return error instanceof Error && error.message === COUPLING_ERROR;
}

/** @param {unknown} value @returns {value is () => boolean} */
function isClassifier(value) {
  return typeof value === "function";
}

/**
 * @param {"missing" | "valid" | "wrong-version"} packageKind
 * @param {string | undefined} childContextSource
 * @returns {Promise<string>}
 */
async function createChildContextFixture(packageKind, childContextSource) {
  const root = await mkdtemp(join(tmpdir(), "steward-child-context-unit-"));
  const runtime = join(root, "runtime");
  await mkdir(runtime, { recursive: true });
  await copyFile(CHILD_CONTEXT_PATH, join(runtime, "pi-child-context.mjs"));

  if (packageKind !== "missing") {
    const packageRoot = join(root, "node_modules", "@tintinweb", "pi-subagents");
    await mkdir(join(packageRoot, "dist"), { recursive: true });
    await writeFile(
      join(packageRoot, "package.json"),
      `${JSON.stringify({
        name: "@tintinweb/pi-subagents",
        version: packageKind === "wrong-version" ? "0.18.0" : "0.19.0",
      })}\n`,
      "utf8",
    );
    if (childContextSource !== undefined) {
      await writeFile(join(packageRoot, "dist", "child-context.js"), childContextSource, "utf8");
    }
  }

  return join(runtime, "pi-child-context.mjs");
}

/**
 * @param {"missing" | "valid" | "wrong-version"} packageKind
 * @param {string | undefined} indexSource
 * @returns {Promise<{ root: string, wrapperPath: string }>}
 */
async function createWrapperFixture(packageKind, indexSource) {
  const root = await mkdtemp(join(tmpdir(), "steward-subagents-wrapper-unit-"));
  const runtime = join(root, "runtime");
  const wrapperPath = join(runtime, "pi-subagents.mjs");
  await mkdir(runtime, { recursive: true });
  await copyFile(SUBAGENTS_WRAPPER_PATH, wrapperPath);

  if (packageKind !== "missing") {
    const packageRoot = join(root, "node_modules", "@tintinweb", "pi-subagents");
    await mkdir(join(packageRoot, "dist"), { recursive: true });
    await writeFile(
      join(packageRoot, "package.json"),
      `${JSON.stringify({
        name: "@tintinweb/pi-subagents",
        version: packageKind === "wrong-version" ? "0.18.0" : "0.19.0",
      })}\n`,
      "utf8",
    );
    if (indexSource !== undefined) {
      await writeFile(join(packageRoot, "dist", "index.js"), indexSource, "utf8");
    }
  }

  return { root, wrapperPath };
}

/** @param {string} modulePath @returns {Promise<() => boolean>} */
async function importClassifier(modulePath) {
  /** @type {unknown} */
  const loaded = await import(pathToFileURL(modulePath).href);
  assert.ok(isRecord(loaded));
  const classifier = loaded.isPiChildSession;
  if (!isClassifier(classifier)) {
    throw new TypeError("isPiChildSession must be a function");
  }
  return classifier;
}

/** @param {string} extensionPath @returns {Promise<void>} */
async function loadSingleExtension(extensionPath) {
  const root = await mkdtemp(join(tmpdir(), "steward-wrapper-loader-"));
  try {
    const { DefaultResourceLoader, SettingsManager } = await import(
      "@earendil-works/pi-coding-agent"
    );
    const loader = new DefaultResourceLoader({
      cwd: root,
      agentDir: join(root, "agent"),
      settingsManager: SettingsManager.inMemory(),
      additionalExtensionPaths: [extensionPath],
      noSkills: true,
      noPromptTemplates: true,
      noThemes: true,
      noContextFiles: true,
    });
    await loader.reload();
    assert.deepEqual(loader.getExtensions().errors, []);
  } finally {
    await rm(root, { force: true, recursive: true });
  }
}

/** @param {string} packageName @param {string | URL} base */
function packageManifestPath(packageName, base) {
  const manifestPath = findPackageJSON(packageName, base);
  if (manifestPath === undefined) {
    throw new Error(`installed ${packageName} manifest is unavailable`);
  }
  return realpathSync(manifestPath);
}

/** @param {string} packageName @param {string | URL} base */
function packageRoot(packageName, base) {
  return dirname(packageManifestPath(packageName, base));
}

/**
 * @param {string | undefined} extensionPath
 * @returns {Promise<{ root: string, result: import("node:child_process").SpawnSyncReturns<string> }>}
 */
async function runConstructionProbe(extensionPath, adapterPath = OWNED_ADAPTER_PATH) {
  const root = await mkdtemp(join(tmpdir(), "steward-child-construction-"));
  const home = join(root, "home");
  const agentDir = join(root, "agent");
  const project = join(root, "project");
  const projectSettings = join(project, ".pi");
  const temporaryDirectory = join(root, "tmp");
  await Promise.all([
    mkdir(home, { recursive: true }),
    mkdir(agentDir, { recursive: true }),
    mkdir(projectSettings, { recursive: true }),
    mkdir(temporaryDirectory, { recursive: true }),
  ]);
  await writeFile(
    join(agentDir, "settings.json"),
    `${JSON.stringify({
      extensions: [SUBAGENTS_WRAPPER_PATH, adapterPath, extensionPath ?? CHILD_PROBE_PATH],
    })}\n`,
    "utf8",
  );
  await writeFile(join(projectSettings, "settings.json"), "{}\n", "utf8");

  const entry = `import { runProbe } from ${JSON.stringify(pathToFileURL(CHILD_PROBE_PATH).href)}; await runProbe();`;
  const startedAt = Date.now();
  /** @type {import("node:child_process").SpawnSyncReturns<string>} */
  const result = spawnSync(process.execPath, ["--input-type=module", "--eval", entry], {
    cwd: project,
    encoding: "utf8",
    env: {
      CI: "1",
      HOME: home,
      NODE_NO_WARNINGS: "1",
      PATH: "/run/current-system/sw/bin:/usr/bin:/bin",
      PI_CODING_AGENT_DIR: agentDir,
      PI_CODING_AGENT_SESSION_DIR: join(project, "sessions"),
      PI_OFFLINE: "1",
      PI_SKIP_VERSION_CHECK: "1",
      PI_TELEMETRY: "0",
      TEMP: temporaryDirectory,
      TMP: temporaryDirectory,
      TMPDIR: temporaryDirectory,
      XDG_CACHE_HOME: join(home, ".cache"),
    },
    maxBuffer: 1024 * 1024,
    timeout: 15_000,
  });
  assert.ok(Date.now() - startedAt < 15_000, "construction probe must exit without observer handles");
  return { root, result };
}

/** @param {string} root @returns {Promise<string>} */
async function createCopiedRootAdapter(root) {
  const runtime = join(root, "copied-adapter");
  await mkdir(runtime, { recursive: true });
  await Promise.all([
    copyFile(OWNED_ADAPTER_PATH, join(runtime, "pi-extension.mjs")),
    copyFile(join(REPOSITORY_ROOT, "runtime", "pi-labels.mjs"), join(runtime, "pi-labels.mjs")),
    copyFile(join(REPOSITORY_ROOT, "runtime", "pi-metadata.mjs"), join(runtime, "pi-metadata.mjs")),
    copyFile(join(REPOSITORY_ROOT, "runtime", "pi-notify.mjs"), join(runtime, "pi-notify.mjs")),
    copyFile(join(REPOSITORY_ROOT, "runtime", "pi-quota.mjs"), join(runtime, "pi-quota.mjs")),
    copyFile(join(REPOSITORY_ROOT, "runtime", "pi-quota-cache.mjs"), join(runtime, "pi-quota-cache.mjs")),
    copyFile(join(REPOSITORY_ROOT, "runtime", "pi-quota-helper.mjs"), join(runtime, "pi-quota-helper.mjs")),
    copyFile(join(REPOSITORY_ROOT, "runtime", "quota.mjs"), join(runtime, "quota.mjs")),
    writeFile(
      join(runtime, "pi-child-context.mjs"),
      "export function isPiChildSession() { return false; }\n",
      "utf8",
    ),
  ]);
  return join(runtime, "pi-extension.mjs");
}

/** @param {string} root */
async function createIndependentProbeExtension(root) {
  const runtime = join(root, "independent-runtime");
  const independentPackageRoot = join(runtime, "node_modules", "@tintinweb", "pi-subagents");
  await mkdir(join(independentPackageRoot, "dist"), { recursive: true });
  await Promise.all([
    copyFile(CHILD_PROBE_PATH, join(runtime, "pi-child-probe.mjs")),
    copyFile(CHILD_CONTEXT_PATH, join(runtime, "pi-child-context.mjs")),
  ]);

  const installedManifestPath = nativeRequire.resolve("@tintinweb/pi-subagents/package.json");
  const installedRoot = dirname(installedManifestPath);
  await Promise.all([
    copyFile(installedManifestPath, join(independentPackageRoot, "package.json")),
    copyFile(
      join(installedRoot, "dist", "child-context.js"),
      join(independentPackageRoot, "dist", "child-context.js"),
    ),
    symlink(
      packageRoot("typebox", import.meta.url),
      join(runtime, "node_modules", "typebox"),
      "dir",
    ),
  ]);
  return join(runtime, "pi-child-probe.mjs");
}

test("installed SDK and subagents resolve one physical Pi peer graph", () => {
  const codingRoot = packageRoot("@earendil-works/pi-coding-agent", import.meta.url);
  const codingEntry = join(codingRoot, "dist", "index.js");
  const subagentsRoot = packageRoot("@tintinweb/pi-subagents", import.meta.url);
  const subagentsEntry = join(subagentsRoot, "dist", "index.js");
  for (const packageName of [
    "@earendil-works/pi-coding-agent",
    "@earendil-works/pi-ai",
    "@earendil-works/pi-tui",
  ]) {
    assert.equal(
      packageManifestPath(packageName, codingEntry),
      packageManifestPath(packageName, subagentsEntry),
      `${packageName} must resolve to one physical package from the SDK and subagents`,
    );
  }
});

test("manifest and lock pin one compatible active Pi graph", async () => {
  const manifest = recordProperty(await readJson(join(REPOSITORY_ROOT, "package.json")), "manifest");
  const dependencies = recordProperty(manifest.dependencies, "dependencies");
  assert.equal(dependencies["@earendil-works/pi-coding-agent"], "0.85.1");
  assert.equal(dependencies["@earendil-works/pi-server"], "0.85.1");
  assert.equal(dependencies["@earendil-works/pi-ai"], "0.85.1");
  assert.equal(dependencies["@earendil-works/pi-tui"], "0.85.1");
  assert.equal(dependencies["@tintinweb/pi-subagents"], "0.19.0");

  const lock = recordProperty(await readJson(join(REPOSITORY_ROOT, "package-lock.json")), "lock");
  const packages = recordProperty(lock.packages, "lock packages");
  const expectedVersions = new Map([
    ["node_modules/@earendil-works/pi-ai", "0.85.1"],
    ["node_modules/@earendil-works/pi-coding-agent", "0.85.1"],
    ["node_modules/@earendil-works/pi-tui", "0.85.1"],
    ["node_modules/@tintinweb/pi-subagents", "0.19.0"],
  ]);
  for (const [packagePath, version] of expectedVersions) {
    assert.equal(recordProperty(packages[packagePath], packagePath).version, version);
  }
  assert.equal(
    packages["node_modules/@tintinweb/pi-subagents/node_modules/@earendil-works/pi-ai"],
    undefined,
  );
  assert.equal(
    packages["node_modules/@tintinweb/pi-subagents/node_modules/@earendil-works/pi-tui"],
    undefined,
  );
  assert.equal(
    packages["node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-ai"],
    undefined,
  );
  assert.equal(
    packages["node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-tui"],
    undefined,
  );

  const subagentsRoot = packageRoot("@tintinweb/pi-subagents", import.meta.url);
  /** @param {string} path */
  const sha256 = async (path) => createHash("sha256").update(await readFile(path)).digest("hex");
  assert.equal(await sha256(join(subagentsRoot, "dist", "index.js")), EXPECTED_INDEX_SHA256);
  assert.equal(
    await sha256(join(subagentsRoot, "dist", "child-context.js")),
    EXPECTED_CHILD_CONTEXT_SHA256,
  );
});

test("shipped bridge modules are relocatable and absent from sessionless helpers", async () => {
  const bridgeSources = await Promise.all([
    readFile(CHILD_CONTEXT_PATH, "utf8"),
    readFile(SUBAGENTS_WRAPPER_PATH, "utf8"),
  ]);
  for (const source of bridgeSources) {
    assert.doesNotMatch(source, /\/tmp\/|\/nix\/store\/|\.claude\/worktrees\//);
    assert.doesNotMatch(source, /pi-child-probe/);
  }
  for (const helper of ["cli.mjs", "compose.mjs", "quota.mjs"]) {
    const source = await readFile(join(REPOSITORY_ROOT, "runtime", helper), "utf8");
    assert.doesNotMatch(source, /pi-subagents|pi-child-context|pi-child-probe/);
  }
});

test("classifier returns booleans only from the pinned compiled accessor", async (t) => {
  const classifierPath = await createChildContextFixture(
    "valid",
    "export function inChildSessionContext() { return false; }\n",
  );
  t.after(() => rm(dirname(dirname(classifierPath)), { force: true, recursive: true }));
  const classifier = await importClassifier(classifierPath);
  assert.equal(classifier(), false);
});

test("classifier fails closed with one safe error for broken coupling", async (t) => {
  const fixtures = [
    ["missing package", "missing", undefined],
    ["wrong package version", "wrong-version", "export function inChildSessionContext() { return false; }\n"],
    ["missing accessor module", "valid", undefined],
    ["missing accessor export", "valid", "export const unrelated = false;\n"],
    ["wrong accessor type", "valid", "export const inChildSessionContext = false;\n"],
  ];
  for (const [name, packageKind, source] of fixtures) {
    await t.test(name, async () => {
      assert.equal(typeof packageKind, "string");
      assert.ok(packageKind === "missing" || packageKind === "valid" || packageKind === "wrong-version");
      assert.ok(source === undefined || typeof source === "string");
      const classifierPath = await createChildContextFixture(packageKind, source);
      t.after(() => rm(dirname(dirname(classifierPath)), { force: true, recursive: true }));
      await assert.rejects(() => import(pathToFileURL(classifierPath).href), isFixedCouplingError);
    });
  }

  await t.test("wrong accessor return type", async () => {
    const classifierPath = await createChildContextFixture(
      "valid",
      "export function inChildSessionContext() { return 'not-a-boolean'; }\n",
    );
    t.after(() => rm(dirname(dirname(classifierPath)), { force: true, recursive: true }));
    const classifier = await importClassifier(classifierPath);
    assert.throws(() => classifier(), isFixedCouplingError);
  });
});

test("wrapper validates the pinned compiled default factory", async (t) => {
  const fixtures = [
    ["missing package", "missing", undefined],
    ["wrong package version", "wrong-version", "export default function () {}\n"],
    ["missing compiled entry", "valid", undefined],
    ["missing default export", "valid", "export const unrelated = true;\n"],
    ["wrong default export type", "valid", "export default 42;\n"],
  ];
  for (const [name, packageKind, source] of fixtures) {
    await t.test(name, async () => {
      assert.equal(typeof packageKind, "string");
      assert.ok(packageKind === "missing" || packageKind === "valid" || packageKind === "wrong-version");
      assert.ok(source === undefined || typeof source === "string");
      const fixture = await createWrapperFixture(packageKind, source);
      t.after(() => rm(fixture.root, { force: true, recursive: true }));
      await assert.rejects(() => import(pathToFileURL(fixture.wrapperPath).href), isFixedCouplingError);
    });
  }
});

test("wrapper forwards Pi construction to the exact native default factory", async (t) => {
  const markerName = "factory-called";
  const fixture = await createWrapperFixture(
    "valid",
    `import { writeFileSync } from "node:fs";\nexport default function (pi) {\n  if (!pi || typeof pi.registerTool !== "function") throw new Error("not the Pi ExtensionAPI");\n  writeFileSync(${JSON.stringify(markerName)}, "called", { encoding: "utf8", flag: "wx" });\n}\n`,
  );
  t.after(() => rm(fixture.root, { force: true, recursive: true }));
  const markerPath = join(fixture.root, markerName);
  const previousCwd = process.cwd();
  process.chdir(fixture.root);
  try {
    await loadSingleExtension(fixture.wrapperPath);
  } finally {
    process.chdir(previousCwd);
  }
  assert.equal(await readFile(markerPath, "utf8"), "called");
});

test("real extension-enabled general-purpose child shares the pinned native context", async (t) => {
  const { root, result } = await runConstructionProbe(undefined);
  t.after(() => rm(root, { force: true, recursive: true }));
  assert.equal(result.error, undefined);
  assert.equal(result.signal, null);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stderr, "");
  const output = recordProperty(JSON.parse(result.stdout), "probe output");
  assert.equal(booleanProperty(output, "proofMet"), true);
  const assertions = recordProperty(output.assertions, "assertions");
  const requiredAssertions = [
    "rootLoadedWrapperAdapterAndProbe",
    "rootClassifierFalse",
    "childClassifierTrueBeforeAdmission",
    "ownedAdapterRootRegistered",
    "ownedAdapterChildSuppressedBeforeRegistration",
    "oneRootAndOneChildFactory",
    "extraProbeExtensionLoaded",
    "sameNativeAccessorAcrossFactories",
    "onePhysicalPinnedSubagentsPackage",
    "activePiSdkAiTuiGraphShared",
    "rootAgentToolRegistered",
    "childAgentToolAbsent",
    "stoppedAtOnSessionCreated",
    "zeroChildMessages",
    "zeroAgentStarts",
    "zeroProviderStarts",
    "zeroNetworkAttempts",
    "parentClassifierFalseAfterChild",
  ];
  for (const name of requiredAssertions) {
    assert.equal(booleanProperty(assertions, name), true, name);
  }
  assert.equal(stringProperty(recordProperty(output.package, "package"), "subagentsVersion"), "0.19.0");
});

test("a copied adapter that bypasses classification registers in the real child", async (t) => {
  const seedRoot = await mkdtemp(join(tmpdir(), "steward-copied-adapter-"));
  t.after(() => rm(seedRoot, { force: true, recursive: true }));
  const copiedAdapter = await createCopiedRootAdapter(seedRoot);
  const { root, result } = await runConstructionProbe(undefined, copiedAdapter);
  t.after(() => rm(root, { force: true, recursive: true }));
  assert.equal(result.status, 1, result.stderr);
  const output = recordProperty(JSON.parse(result.stdout), "copied adapter output");
  const assertions = recordProperty(output.assertions, "copied adapter assertions");
  assert.equal(booleanProperty(assertions, "ownedAdapterChildSuppressedBeforeRegistration"), false);
  assert.equal(booleanProperty(assertions, "ownedAdapterRootRegistered"), true);
});

test("an independent accessor copy is a failing real-construction negative control", async (t) => {
  const seedRoot = await mkdtemp(join(tmpdir(), "steward-independent-accessor-"));
  t.after(() => rm(seedRoot, { force: true, recursive: true }));
  const independentExtension = await createIndependentProbeExtension(seedRoot);
  const { root, result } = await runConstructionProbe(independentExtension);
  t.after(() => rm(root, { force: true, recursive: true }));
  assert.equal(result.error, undefined);
  assert.equal(result.signal, null);
  assert.equal(result.status, 1, result.stderr);
  assert.equal(result.stderr, "");
  const output = recordProperty(JSON.parse(result.stdout), "negative probe output");
  assert.equal(booleanProperty(output, "proofMet"), false);
  const assertions = recordProperty(output.assertions, "negative assertions");
  assert.equal(booleanProperty(assertions, "childClassifierTrueBeforeAdmission"), false);
  assert.equal(booleanProperty(assertions, "ownedAdapterChildSuppressedBeforeRegistration"), true);
  assert.equal(booleanProperty(assertions, "sameNativeAccessorAcrossFactories"), false);
  assert.equal(booleanProperty(assertions, "onePhysicalPinnedSubagentsPackage"), false);
  assert.equal(booleanProperty(assertions, "stoppedAtOnSessionCreated"), true);
  assert.equal(booleanProperty(assertions, "zeroChildMessages"), true);
});
