import assert from "node:assert/strict";
import { lstatSync } from "node:fs";
import {
  copyFile,
  lstat,
  mkdir,
  mkdtemp,
  readFile,
  rename,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { fileURLToPath, pathToFileURL } from "node:url";

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const PREPARATION_PATH = join(REPOSITORY_ROOT, "runtime", "prepare-pi-runtime.mjs");
const FAILURE_PREFIX = "Steward Pi runtime preparation failed:";

/**
 * @typedef {{
 *   root: string,
 *   runtimePath: string,
 *   codingRoot: string,
 *   rootAi: string,
 *   rootTui: string,
 *   nestedAi: string,
 *   nestedTui: string,
 *   nestedNeighbor: string,
 *   rootNeighbor: string,
 * }} RuntimeFixture
 */

/** @param {string} packageRoot @param {string} name @param {string} version */
async function writeManifest(packageRoot, name, version) {
  await mkdir(packageRoot, { recursive: true });
  await writeFile(join(packageRoot, "package.json"), `${JSON.stringify({ name, version })}\n`, "utf8");
}

/** @returns {Promise<RuntimeFixture>} */
async function createRuntimeFixture() {
  const root = await mkdtemp(join(tmpdir(), "steward-prepare-pi-runtime-"));
  const runtimePath = join(root, "runtime", "prepare-pi-runtime.mjs");
  const packageScope = join(root, "node_modules", "@earendil-works");
  const codingRoot = join(packageScope, "pi-coding-agent");
  const nestedScope = join(codingRoot, "node_modules", "@earendil-works");
  const rootAi = join(packageScope, "pi-ai");
  const rootTui = join(packageScope, "pi-tui");
  const nestedAi = join(nestedScope, "pi-ai");
  const nestedTui = join(nestedScope, "pi-tui");
  const nestedNeighbor = join(nestedScope, "pi-client", "keep.txt");
  const rootNeighbor = join(packageScope, "pi-server", "keep.txt");

  await mkdir(dirname(runtimePath), { recursive: true });
  await copyFile(PREPARATION_PATH, runtimePath);
  await Promise.all([
    writeManifest(codingRoot, "@earendil-works/pi-coding-agent", "0.85.1"),
    writeManifest(rootAi, "@earendil-works/pi-ai", "0.85.1"),
    writeManifest(rootTui, "@earendil-works/pi-tui", "0.85.1"),
    writeManifest(nestedAi, "@earendil-works/pi-ai", "0.85.1"),
    writeManifest(nestedTui, "@earendil-works/pi-tui", "0.85.1"),
    mkdir(dirname(nestedNeighbor), { recursive: true }).then(() =>
      writeFile(nestedNeighbor, "nested neighbor\n", "utf8"),
    ),
    mkdir(dirname(rootNeighbor), { recursive: true }).then(() =>
      writeFile(rootNeighbor, "root neighbor\n", "utf8"),
    ),
  ]);

  return {
    root,
    runtimePath,
    codingRoot,
    rootAi,
    rootTui,
    nestedAi,
    nestedTui,
    nestedNeighbor,
    rootNeighbor,
  };
}

/** @param {string} path @returns {Promise<boolean>} */
async function exists(path) {
  try {
    await lstat(path);
    return true;
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT") {
      return false;
    }
    throw error;
  }
}

/** @param {RuntimeFixture} fixture */
function runPreparation(fixture) {
  return spawnSync(process.execPath, [fixture.runtimePath], {
    cwd: fixture.root,
    encoding: "utf8",
  });
}

/**
 * @param {RuntimeFixture} fixture
 * @param {string} expectedDetail
 */
function assertPreparationFails(fixture, expectedDetail) {
  const result = runPreparation(fixture);
  assert.equal(result.error, undefined);
  assert.equal(result.signal, null);
  assert.equal(result.status, 1);
  assert.match(result.stderr, new RegExp(`^${FAILURE_PREFIX} `));
  assert.match(result.stderr, new RegExp(expectedDetail));
  assert.doesNotMatch(result.stderr, /\n\s+at /, "failure must not emit an implementation stack");
  assert.equal(result.stdout, "");
}

/** @param {RuntimeFixture} fixture */
async function assertGeneratedCopiesRemain(fixture) {
  assert.equal(await exists(fixture.nestedAi), true, "nested pi-ai must remain");
  assert.equal(await exists(fixture.nestedTui), true, "nested pi-tui must remain");
}

test("package scripts and Node CI explicitly prepare the ignored-script install", async () => {
  /** @type {unknown} */
  const manifestValue = JSON.parse(await readFile(join(REPOSITORY_ROOT, "package.json"), "utf8"));
  assert.ok(typeof manifestValue === "object" && manifestValue !== null);
  const manifest = /** @type {Record<string, unknown>} */ (manifestValue);
  assert.ok(typeof manifest.scripts === "object" && manifest.scripts !== null);
  const scripts = /** @type {Record<string, unknown>} */ (manifest.scripts);
  assert.equal(scripts["prepare:pi-runtime"], "node runtime/prepare-pi-runtime.mjs");
  assert.equal(scripts.postinstall, "npm run prepare:pi-runtime");

  const workflow = await readFile(join(REPOSITORY_ROOT, ".github", "workflows", "test.yml"), "utf8");
  const nodeJobStart = workflow.indexOf("  node:\n");
  const nextJobStart = workflow.indexOf("\n  test:\n", nodeJobStart);
  assert.notEqual(nodeJobStart, -1, "workflow must contain the Node job");
  assert.notEqual(nextJobStart, -1, "Node job must have a bounded workflow section");
  const nodeJob = workflow.slice(nodeJobStart, nextJobStart);
  const installAt = nodeJob.indexOf("run: npm ci --ignore-scripts");
  const prepareAt = nodeJob.indexOf("run: npm run prepare:pi-runtime");
  const testAt = nodeJob.indexOf("run: npm test");
  assert.ok(installAt >= 0, "Node job must retain ignored lifecycle scripts");
  assert.ok(prepareAt > installAt, "Node job must explicitly prepare after npm ci");
  assert.ok(testAt > prepareAt, "Node tests must run only after explicit preparation");
});

test("exact generated AI and TUI copies are removed without touching neighbors", async (t) => {
  const fixture = await createRuntimeFixture();
  t.after(() => rm(fixture.root, { force: true, recursive: true }));

  const result = runPreparation(fixture);
  assert.equal(result.error, undefined);
  assert.equal(result.signal, null);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stderr, "");
  assert.equal(await exists(fixture.nestedAi), false);
  assert.equal(await exists(fixture.nestedTui), false);
  assert.equal(await readFile(fixture.nestedNeighbor, "utf8"), "nested neighbor\n");
  assert.equal(await readFile(fixture.rootNeighbor, "utf8"), "root neighbor\n");
  assert.equal(await exists(fixture.rootAi), true);
  assert.equal(await exists(fixture.rootTui), true);
});

test("preparation is idempotent when generated copies are already absent", async (t) => {
  const fixture = await createRuntimeFixture();
  t.after(() => rm(fixture.root, { force: true, recursive: true }));
  await rm(fixture.nestedAi, { recursive: true });
  await rm(fixture.nestedTui, { recursive: true });

  for (let invocation = 0; invocation < 2; invocation += 1) {
    const result = runPreparation(fixture);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stderr, "");
  }
  assert.equal(await readFile(fixture.nestedNeighbor, "utf8"), "nested neighbor\n");
  assert.equal(await readFile(fixture.rootNeighbor, "utf8"), "root neighbor\n");
});

test("a generated copy is validated and removed when its peer is already absent", async (t) => {
  const fixture = await createRuntimeFixture();
  t.after(() => rm(fixture.root, { force: true, recursive: true }));
  await rm(fixture.nestedAi, { recursive: true });

  const result = runPreparation(fixture);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(await exists(fixture.nestedAi), false);
  assert.equal(await exists(fixture.nestedTui), false);
  assert.equal(await readFile(fixture.nestedNeighbor, "utf8"), "nested neighbor\n");
});

test("importing the preparation module does not mutate the package graph", async (t) => {
  const fixture = await createRuntimeFixture();
  t.after(() => rm(fixture.root, { force: true, recursive: true }));
  const entry = `await import(${JSON.stringify(pathToFileURL(fixture.runtimePath).href)});`;

  const result = spawnSync(process.execPath, ["--input-type=module", "--eval", entry], {
    cwd: fixture.root,
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stderr, "");
  await assertGeneratedCopiesRemain(fixture);
});

test("missing or wrong required root packages fail before generated copies are removed", async (t) => {
  /** @type {Array<{ name: string, detail: string, alter: (fixture: RuntimeFixture) => Promise<void> }>} */
  const cases = [
    {
      name: "missing SDK",
      detail: "@earendil-works/pi-coding-agent",
      alter: async (fixture) => rm(join(fixture.codingRoot, "package.json")),
    },
    {
      name: "wrong SDK pin",
      detail: "@earendil-works/pi-coding-agent.*0\\.85\\.1",
      alter: async (fixture) =>
        writeManifest(fixture.codingRoot, "@earendil-works/pi-coding-agent", "0.85.0"),
    },
    {
      name: "missing root AI",
      detail: "root @earendil-works/pi-ai",
      alter: async (fixture) => rm(fixture.rootAi, { recursive: true }),
    },
    {
      name: "wrong root AI pin",
      detail: "root @earendil-works/pi-ai.*0\\.85\\.1",
      alter: async (fixture) => writeManifest(fixture.rootAi, "@earendil-works/pi-ai", "0.85.0"),
    },
    {
      name: "missing root TUI",
      detail: "root @earendil-works/pi-tui",
      alter: async (fixture) => rm(fixture.rootTui, { recursive: true }),
    },
    {
      name: "wrong root TUI pin",
      detail: "root @earendil-works/pi-tui.*0\\.85\\.1",
      alter: async (fixture) => writeManifest(fixture.rootTui, "@earendil-works/pi-tui", "0.85.0"),
    },
  ];

  for (const scenario of cases) {
    await t.test(scenario.name, async (t) => {
      const fixture = await createRuntimeFixture();
      t.after(() => rm(fixture.root, { force: true, recursive: true }));
      await scenario.alter(fixture);
      assertPreparationFails(fixture, scenario.detail);
      await assertGeneratedCopiesRemain(fixture);
    });
  }
});

test("invalid generated manifests fail without partially pruning the pair", async (t) => {
  const cases = [
    ["wrong nested AI pin", "pi-ai.*0\\.85\\.1", "ai", "@earendil-works/pi-ai", "0.85.0"],
    ["wrong nested TUI pin", "pi-tui.*0\\.85\\.1", "tui", "@earendil-works/pi-tui", "0.85.0"],
    ["wrong nested AI name", "pi-ai", "ai", "@earendil-works/different-ai", "0.85.1"],
    ["wrong nested TUI name", "pi-tui", "tui", "@earendil-works/different-tui", "0.85.1"],
  ];

  for (const [scenario, detail, targetKind, manifestName, version] of cases) {
    await t.test(scenario, async (t) => {
      const fixture = await createRuntimeFixture();
      t.after(() => rm(fixture.root, { force: true, recursive: true }));
      const target = targetKind === "ai" ? fixture.nestedAi : fixture.nestedTui;
      await writeManifest(target, manifestName, version);
      assertPreparationFails(fixture, detail);
      await assertGeneratedCopiesRemain(fixture);
    });
  }
});

test("corrupt required and generated manifests fail clearly before mutation", async (t) => {
  for (const [scenario, manifestPath, detail] of [
    ["corrupt SDK manifest", "sdk", "pi-coding-agent.*manifest"],
    ["corrupt nested TUI manifest", "nested-tui", "pi-tui.*manifest"],
  ]) {
    await t.test(scenario, async (t) => {
      const fixture = await createRuntimeFixture();
      t.after(() => rm(fixture.root, { force: true, recursive: true }));
      const target = manifestPath === "sdk" ? fixture.codingRoot : fixture.nestedTui;
      await writeFile(join(target, "package.json"), "{not json\n", "utf8");
      assertPreparationFails(fixture, detail);
      await assertGeneratedCopiesRemain(fixture);
    });
  }
});

test("symlinked deletion targets and package ancestry are rejected without mutation", async (t) => {
  await t.test("target symlink", async (t) => {
    const fixture = await createRuntimeFixture();
    t.after(() => rm(fixture.root, { force: true, recursive: true }));
    const external = join(fixture.root, "external-pi-tui");
    await writeManifest(external, "@earendil-works/pi-tui", "0.85.1");
    await writeFile(join(external, "keep.txt"), "external\n", "utf8");
    await rm(fixture.nestedTui, { recursive: true });
    await symlink(external, fixture.nestedTui, "dir");

    assertPreparationFails(fixture, "pi-tui.*symbolic link");
    assert.equal(await exists(fixture.nestedAi), true);
    assert.equal(lstatSync(fixture.nestedTui).isSymbolicLink(), true);
    assert.equal(await readFile(join(external, "keep.txt"), "utf8"), "external\n");
  });

  await t.test("SDK ancestry symlink", async (t) => {
    const fixture = await createRuntimeFixture();
    t.after(() => rm(fixture.root, { force: true, recursive: true }));
    const externalSdk = join(fixture.root, "external-sdk");
    await rename(fixture.codingRoot, externalSdk);
    await symlink(externalSdk, fixture.codingRoot, "dir");

    assertPreparationFails(fixture, "pi-coding-agent.*symbolic link");
    assert.equal(lstatSync(fixture.codingRoot).isSymbolicLink(), true);
    assert.equal(await exists(join(externalSdk, "node_modules", "@earendil-works", "pi-ai")), true);
    assert.equal(await exists(join(externalSdk, "node_modules", "@earendil-works", "pi-tui")), true);
    assert.equal(await readFile(join(externalSdk, "node_modules", "@earendil-works", "pi-client", "keep.txt"), "utf8"), "nested neighbor\n");
  });
});
