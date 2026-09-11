import { lstatSync, readFileSync, realpathSync, rmSync } from "node:fs";
import { dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const PI_VERSION = "0.85.1";
const FAILURE_PREFIX = "Steward Pi runtime preparation failed:";
const REPOSITORY_ROOT = realpathSync(resolve(dirname(fileURLToPath(import.meta.url)), ".."));
const NODE_MODULES = join(REPOSITORY_ROOT, "node_modules");
const PI_SCOPE = join(NODE_MODULES, "@earendil-works");
const CODING_AGENT_ROOT = join(PI_SCOPE, "pi-coding-agent");
const NESTED_PI_SCOPE = join(CODING_AGENT_ROOT, "node_modules", "@earendil-works");

const requiredPackages = [
  {
    label: "@earendil-works/pi-coding-agent",
    name: "@earendil-works/pi-coding-agent",
    path: CODING_AGENT_ROOT,
  },
  {
    label: "root @earendil-works/pi-ai",
    name: "@earendil-works/pi-ai",
    path: join(PI_SCOPE, "pi-ai"),
  },
  {
    label: "root @earendil-works/pi-tui",
    name: "@earendil-works/pi-tui",
    path: join(PI_SCOPE, "pi-tui"),
  },
];

const generatedPackages = [
  {
    label: "generated @earendil-works/pi-ai",
    name: "@earendil-works/pi-ai",
    path: join(NESTED_PI_SCOPE, "pi-ai"),
  },
  {
    label: "generated @earendil-works/pi-tui",
    name: "@earendil-works/pi-tui",
    path: join(NESTED_PI_SCOPE, "pi-tui"),
  },
];

/** @param {unknown} value @returns {value is Record<string, unknown>} */
function isRecord(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** @param {unknown} error @returns {error is NodeJS.ErrnoException} */
function isErrnoException(error) {
  return error instanceof Error && "code" in error;
}

/** @param {string} detail @returns {never} */
function fail(detail) {
  throw new Error(detail);
}

/**
 * @param {string} parent
 * @param {string} target
 * @param {string} label
 */
function assertContained(parent, target, label) {
  const pathFromParent = relative(parent, target);
  if (
    pathFromParent === "" ||
    pathFromParent === ".." ||
    pathFromParent.startsWith(`..${sep}`) ||
    isAbsolute(pathFromParent)
  ) {
    fail(`${label} is outside its expected package directory`);
  }
}

/** @param {string} path @param {string} label @returns {import("node:fs").Stats | undefined} */
function optionalStatus(path, label) {
  try {
    return lstatSync(path);
  } catch (error) {
    if (isErrnoException(error) && error.code === "ENOENT") {
      return undefined;
    }
    fail(`${label} cannot be inspected`);
  }
}

/**
 * Validate each existing component without following a symbolic link.
 * @param {string} base
 * @param {string[]} components
 * @param {string} label
 * @param {boolean} required
 * @returns {boolean}
 */
function validateDirectoryChain(base, components, label, required) {
  let current = base;
  for (const component of components) {
    const candidate = join(current, component);
    assertContained(REPOSITORY_ROOT, candidate, label);
    const status = optionalStatus(candidate, label);
    if (status === undefined) {
      if (required) {
        fail(`${label} is missing`);
      }
      return false;
    }
    if (status.isSymbolicLink()) {
      fail(`${label} path contains a symbolic link at ${candidate}`);
    }
    if (!status.isDirectory()) {
      fail(`${label} path contains a non-directory at ${candidate}`);
    }
    current = candidate;
  }
  return true;
}

/**
 * @param {string} packageRoot
 * @param {string} expectedName
 * @param {string} label
 */
function validateManifest(packageRoot, expectedName, label) {
  const manifestPath = join(packageRoot, "package.json");
  assertContained(packageRoot, manifestPath, label);
  const status = optionalStatus(manifestPath, `${label} manifest`);
  if (status === undefined) {
    fail(`${label} manifest is missing`);
  }
  if (status.isSymbolicLink()) {
    fail(`${label} manifest is a symbolic link`);
  }
  if (!status.isFile()) {
    fail(`${label} manifest is not a regular file`);
  }

  /** @type {unknown} */
  let manifest;
  try {
    manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
  } catch {
    fail(`${label} manifest is corrupt`);
  }
  if (!isRecord(manifest) || manifest.name !== expectedName || manifest.version !== PI_VERSION) {
    fail(`${label} must be ${expectedName} at exactly ${PI_VERSION}`);
  }
}

/**
 * Align the generated coding-agent shrinkwrap copies with Steward's validated root Pi peers.
 * This function intentionally accepts no path: it can mutate only this package's exact graph.
 */
export function preparePiRuntime() {
  validateDirectoryChain(REPOSITORY_ROOT, ["node_modules"], "node_modules", true);
  validateDirectoryChain(
    REPOSITORY_ROOT,
    ["node_modules", "@earendil-works"],
    "root @earendil-works scope",
    true,
  );

  for (const packageInfo of requiredPackages) {
    const components = relative(REPOSITORY_ROOT, packageInfo.path).split(sep);
    validateDirectoryChain(REPOSITORY_ROOT, components, packageInfo.label, true);
    validateManifest(packageInfo.path, packageInfo.name, packageInfo.label);
  }

  const hasNestedScope = validateDirectoryChain(
    CODING_AGENT_ROOT,
    ["node_modules", "@earendil-works"],
    "generated Pi package ancestry",
    false,
  );
  if (!hasNestedScope) {
    return;
  }

  /** @type {typeof generatedPackages} */
  const copiesToRemove = [];
  for (const packageInfo of generatedPackages) {
    assertContained(CODING_AGENT_ROOT, packageInfo.path, packageInfo.label);
    const status = optionalStatus(packageInfo.path, packageInfo.label);
    if (status === undefined) {
      continue;
    }
    if (status.isSymbolicLink()) {
      fail(`${packageInfo.label} target is a symbolic link`);
    }
    if (!status.isDirectory()) {
      fail(`${packageInfo.label} target is not a directory`);
    }
    validateManifest(packageInfo.path, packageInfo.name, packageInfo.label);
    copiesToRemove.push(packageInfo);
  }

  for (const packageInfo of copiesToRemove) {
    rmSync(packageInfo.path, { recursive: true });
  }
}

const invokedPath = process.argv[1];
if (invokedPath !== undefined && resolve(invokedPath) === fileURLToPath(import.meta.url)) {
  try {
    preparePiRuntime();
  } catch (error) {
    const detail = error instanceof Error ? error.message : "unknown preparation error";
    process.stderr.write(`${FAILURE_PREFIX} ${detail}\n`);
    process.exitCode = 1;
  }
}
