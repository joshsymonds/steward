import { realpathSync, readFileSync } from "node:fs";
import { createRequire, findPackageJSON, syncBuiltinESMExports } from "node:module";
import { dirname, join } from "node:path";
import process from "node:process";

import { Type } from "typebox";

import { isPiChildSession } from "./pi-child-context.mjs";

const PACKAGE_NAME = "@tintinweb/pi-subagents";
const PACKAGE_VERSION = "0.19.0";
const OBSERVER_KEY = Symbol.for("steward:pi-child-context-test-observer");
const STOP = new Error("STEWARD_PROBE_STOP_BEFORE_PROMPT");
const nativeRequire = createRequire(import.meta.url);

/** @typedef {import("@earendil-works/pi-coding-agent").AgentSession} AgentSession */
/** @typedef {import("@earendil-works/pi-coding-agent").ExtensionAPI} ExtensionAPI */
/** @typedef {import("@earendil-works/pi-coding-agent").ExtensionContext} ExtensionContext */
/**
 * @typedef {{
 *   type: "factory",
 *   childAtFactory: boolean,
 *   resolvedChildContext: string,
 *   accessor: () => unknown,
 * } | {
 *   type: "root-session",
 *   pi: ExtensionAPI,
 *   context: ExtensionContext,
 * } | {
 *   type: "session-start",
 *   childAtFactory: boolean,
 *   accessorAtSessionStart: unknown,
 * } | {
 *   type: "agent-start" | "provider-start",
 * }} ProbeObservation
 */
/** @typedef {(observation: ProbeObservation) => void} ProbeObserver */
/**
 * @typedef {{
 *   pi: ExtensionAPI,
 *   signal: AbortSignal,
 *   onSessionCreated: (session: AgentSession) => void,
 * }} ProbeRunOptions
 */
/** @typedef {(context: ExtensionContext, type: "general-purpose", prompt: string, options: ProbeRunOptions) => Promise<unknown>} RunAgent */

/** @param {unknown} value @returns {value is Record<string, unknown>} */
function isRecord(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** @param {unknown} value @returns {value is () => unknown} */
function isAccessor(value) {
  return typeof value === "function";
}

/** @param {unknown} value @returns {value is ProbeObserver} */
function isProbeObserver(value) {
  return typeof value === "function";
}

/** @param {unknown} value @returns {value is RunAgent} */
function isRunAgent(value) {
  return typeof value === "function";
}

/** @returns {ProbeObserver} */
function observer() {
  /** @type {unknown} */
  const value = Reflect.get(globalThis, OBSERVER_KEY);
  if (!isProbeObserver(value)) {
    throw new Error("Pi child probe observer is unavailable");
  }
  return value;
}

function loadNativeChildContext() {
  const manifestPath = nativeRequire.resolve(`${PACKAGE_NAME}/package.json`);
  /** @type {unknown} */
  const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
  if (!isRecord(manifest) || manifest.name !== PACKAGE_NAME || manifest.version !== PACKAGE_VERSION) {
    throw new Error("Pi child probe did not resolve the pinned subagents package");
  }
  const childContextPath = join(dirname(manifestPath), "dist", "child-context.js");
  /** @type {unknown} */
  const nativeModule = nativeRequire(childContextPath);
  if (!isRecord(nativeModule) || !isAccessor(nativeModule.inChildSessionContext)) {
    throw new Error("Pi child probe did not resolve the native child-context accessor");
  }
  return {
    accessor: nativeModule.inChildSessionContext,
    childContextPath: nativeRequire.resolve(childContextPath),
    packageRoot: dirname(manifestPath),
  };
}

const nativeChildContext = loadNativeChildContext();

/** @type {import("@earendil-works/pi-coding-agent").ExtensionFactory} */
export default function piChildProbe(pi) {
  const childAtFactory = isPiChildSession();
  observer()({
    type: "factory",
    childAtFactory,
    resolvedChildContext: nativeChildContext.childContextPath,
    accessor: nativeChildContext.accessor,
  });

  pi.registerTool({
    name: "steward_child_probe",
    label: "Steward child probe",
    description: "Synthetic marker proving that the ordinary probe extension loaded.",
    parameters: Type.Object({}),
    async execute() {
      return { content: [{ type: "text", text: "loaded" }], details: {} };
    },
  });

  pi.on("agent_start", () => {
    observer()({ type: "agent-start" });
  });
  pi.on("before_provider_request", () => {
    observer()({ type: "provider-start" });
  });
  pi.on("session_start", () => {
    observer()({
      type: "session-start",
      childAtFactory,
      accessorAtSessionStart: nativeChildContext.accessor(),
    });
  });
  pi.on("session_start", (_event, context) => {
    if (!childAtFactory) {
      observer()({ type: "root-session", pi, context });
    }
  });
}

/** @param {string} api @param {string[]} attempts @returns {(...args: unknown[]) => never} */
function createNetworkBlocker(api, attempts) {
  return (..._args) => {
    attempts.push(api);
    throw new Error(`Synthetic Pi child probe blocked network API: ${api}`);
  };
}

/**
 * @param {object} target
 * @param {PropertyKey} key
 * @param {string} api
 * @param {string[]} attempts
 */
function blockNetworkMethod(target, key, api, attempts) {
  Object.defineProperty(target, key, {
    configurable: true,
    value: createNetworkBlocker(api, attempts),
    writable: true,
  });
}

/** @param {string} packageName @param {string | URL} base */
function packageIdentity(packageName, base) {
  const manifestPath = findPackageJSON(packageName, base);
  if (manifestPath === undefined) {
    throw new Error(`Synthetic Pi child probe could not resolve ${packageName}`);
  }
  /** @type {unknown} */
  const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
  if (!isRecord(manifest) || manifest.name !== packageName || typeof manifest.version !== "string") {
    throw new Error(`Synthetic Pi child probe found an invalid ${packageName} manifest`);
  }
  return {
    manifestPath: realpathSync(manifestPath),
    version: manifest.version,
  };
}

/** @param {AgentSession | undefined} session */
async function closeSession(session) {
  if (session === undefined) {
    return;
  }
  await session.abort();
  session.dispose();
}

export async function runProbe() {
  /** @type {string[]} */
  const networkAttempts = [];
  const [netNamespace, tlsNamespace, httpNamespace, httpsNamespace, http2Namespace, dgramNamespace, dnsNamespace] =
    await Promise.all([
      import("node:net"),
      import("node:tls"),
      import("node:http"),
      import("node:https"),
      import("node:http2"),
      import("node:dgram"),
      import("node:dns"),
    ]);
  const net = netNamespace.default;
  blockNetworkMethod(net, "connect", "net.connect", networkAttempts);
  blockNetworkMethod(net, "createConnection", "net.createConnection", networkAttempts);
  blockNetworkMethod(net.Socket.prototype, "connect", "net.Socket.connect", networkAttempts);
  blockNetworkMethod(tlsNamespace.default, "connect", "tls.connect", networkAttempts);
  blockNetworkMethod(httpNamespace.default, "request", "http.request", networkAttempts);
  blockNetworkMethod(httpNamespace.default, "get", "http.get", networkAttempts);
  blockNetworkMethod(httpsNamespace.default, "request", "https.request", networkAttempts);
  blockNetworkMethod(httpsNamespace.default, "get", "https.get", networkAttempts);
  blockNetworkMethod(http2Namespace.default, "connect", "http2.connect", networkAttempts);
  blockNetworkMethod(dgramNamespace.default, "createSocket", "dgram.createSocket", networkAttempts);
  blockNetworkMethod(dnsNamespace.default, "lookup", "dns.lookup", networkAttempts);
  blockNetworkMethod(dnsNamespace.default, "resolve", "dns.resolve", networkAttempts);
  Object.defineProperty(globalThis, "fetch", {
    configurable: true,
    value: createNetworkBlocker("fetch", networkAttempts),
    writable: true,
  });
  syncBuiltinESMExports();

  /** @type {Array<{ childAtFactory: boolean, resolvedChildContext: string, accessor: () => unknown }>} */
  const factories = [];
  /** @type {Array<{ childAtFactory: boolean, accessorAtSessionStart: unknown }>} */
  const sessionStarts = [];
  /** @type {ExtensionAPI | undefined} */
  let rootPi;
  /** @type {ExtensionContext | undefined} */
  let rootContext;
  let agentStarts = 0;
  let providerStarts = 0;
  /** @type {ProbeObserver} */
  const receiveObservation = (observation) => {
    switch (observation.type) {
      case "factory":
        factories.push({
          childAtFactory: observation.childAtFactory,
          resolvedChildContext: observation.resolvedChildContext,
          accessor: observation.accessor,
        });
        break;
      case "root-session":
        rootPi ??= observation.pi;
        rootContext ??= observation.context;
        break;
      case "session-start":
        sessionStarts.push({
          childAtFactory: observation.childAtFactory,
          accessorAtSessionStart: observation.accessorAtSessionStart,
        });
        break;
      case "agent-start":
        agentStarts += 1;
        break;
      case "provider-start":
        providerStarts += 1;
        break;
    }
  };
  Reflect.set(globalThis, OBSERVER_KEY, receiveObservation);

  const project = process.cwd();
  const agentDir = process.env.PI_CODING_AGENT_DIR;
  if (agentDir === undefined) {
    throw new Error("Synthetic Pi child probe requires PI_CODING_AGENT_DIR");
  }

  const {
    createAgentSession,
    DefaultResourceLoader,
    ModelRuntime,
    SessionManager,
    SettingsManager,
  } = await import("@earendil-works/pi-coding-agent");
  const settingsManager = SettingsManager.create(project, agentDir);
  const modelRuntime = await ModelRuntime.create({
    authPath: join(agentDir, "auth.json"),
    modelsPath: null,
    modelsStorePath: join(agentDir, "models-store.json"),
    allowModelNetwork: false,
    refreshOnCreate: false,
  });
  const model = modelRuntime.getModel("anthropic", "claude-haiku-4-5");
  if (model === undefined) {
    throw new Error("Pinned Pi runtime did not provide the synthetic construction model");
  }

  const rootLoader = new DefaultResourceLoader({
    cwd: project,
    agentDir,
    settingsManager,
    noSkills: true,
    noPromptTemplates: true,
    noThemes: true,
    noContextFiles: true,
    systemPromptOverride: () => "Synthetic parent session; no prompt will be sent.",
    appendSystemPromptOverride: () => [],
  });
  await rootLoader.reload();
  const rootLoad = rootLoader.getExtensions();
  const { session: rootSession } = await createAgentSession({
    cwd: project,
    agentDir,
    model,
    modelRuntime,
    resourceLoader: rootLoader,
    sessionManager: SessionManager.inMemory(project),
    settingsManager,
  });

  /** @type {AgentSession | undefined} */
  let childSession;
  /** @type {Promise<void> | undefined} */
  let childAbort;
  /** @type {unknown} */
  let caught;
  let accessorAtOnSessionCreated;
  let childMessagesAtCallback;
  /** @type {string[]} */
  let childAllTools = [];
  /** @type {string[]} */
  let childActiveTools = [];
  let childProbeSource;
  /** @type {ReturnType<AgentSession["resourceLoader"]["getExtensions"]> | undefined} */
  let childLoad;
  const stopController = new AbortController();

  try {
    await rootSession.bindExtensions({});
    if (rootPi === undefined || rootContext === undefined) {
      throw new Error("Synthetic parent extensions did not bind");
    }

    const runnerPath = join(nativeChildContext.packageRoot, "dist", "agent-runner.js");
    /** @type {unknown} */
    const runnerModule = nativeRequire(runnerPath);
    if (!isRecord(runnerModule) || !isRunAgent(runnerModule.runAgent)) {
      throw new Error("Pinned native subagents runner export is unavailable");
    }

    agentStarts = 0;
    providerStarts = 0;
    try {
      await runnerModule.runAgent(
        rootContext,
        "general-purpose",
        "THIS PROMPT MUST NOT BE ENQUEUED",
        {
          pi: rootPi,
          signal: stopController.signal,
          onSessionCreated(session) {
            childSession = session;
            childLoad = session.resourceLoader.getExtensions();
            accessorAtOnSessionCreated = nativeChildContext.accessor();
            childMessagesAtCallback = session.messages.length;
            const allTools = session.getAllTools();
            childAllTools = allTools.map((tool) => tool.name);
            childActiveTools = session.getActiveToolNames();
            childProbeSource = allTools.find((tool) => tool.name === "steward_child_probe")?.sourceInfo.path;
            stopController.abort(STOP);
            childAbort = session.abort();
            throw STOP;
          },
        },
      );
    } catch (error) {
      caught = error;
    }
    if (childAbort !== undefined) {
      await childAbort;
    }

    const childMessagesAfter = childSession?.messages.length;
    const rootTools = rootSession.getAllTools().map((tool) => tool.name);
    const exactIndex = join(nativeChildContext.packageRoot, "dist", "index.js");
    const codingFromProbe = packageIdentity("@earendil-works/pi-coding-agent", import.meta.url);
    const codingFromSubagents = packageIdentity("@earendil-works/pi-coding-agent", exactIndex);
    const aiFromSdk = packageIdentity(
      "@earendil-works/pi-ai",
      join(dirname(codingFromProbe.manifestPath), "dist", "index.js"),
    );
    const aiFromSubagents = packageIdentity("@earendil-works/pi-ai", exactIndex);
    const tuiFromSdk = packageIdentity(
      "@earendil-works/pi-tui",
      join(dirname(codingFromProbe.manifestPath), "dist", "index.js"),
    );
    const tuiFromSubagents = packageIdentity("@earendil-works/pi-tui", exactIndex);
    const rootFactories = factories.filter((factory) => factory.childAtFactory === false);
    const childFactories = factories.filter((factory) => factory.childAtFactory === true);
    const rootAdapter = rootLoad.extensions.find((extension) => extension.path.endsWith("/pi-extension.mjs"));
    const childAdapter = childLoad?.extensions.find((extension) => extension.path.endsWith("/pi-extension.mjs"));
    const requiredRootHandlers = [
      "session_start",
      "session_shutdown",
      "session_info_changed",
      "agent_start",
      "session_tree",
      "agent_settled",
    ];
    const accessorPaths = new Set(factories.map((factory) => realpathSync(factory.resolvedChildContext)));
    const accessors = new Set(factories.map((factory) => factory.accessor));
    const assertions = {
      rootLoadedWrapperAdapterAndProbe:
        rootLoad.errors.length === 0 &&
        rootLoad.extensions.some((extension) => extension.path.endsWith("/pi-subagents.mjs")) &&
        rootLoad.extensions.some((extension) => extension.path.endsWith("/pi-extension.mjs")) &&
        rootLoad.extensions.some((extension) => extension.path.endsWith("/pi-child-probe.mjs")),
      rootClassifierFalse: factories[0]?.childAtFactory === false,
      childClassifierTrueBeforeAdmission: childFactories.length === 1,
      ownedAdapterRootRegistered:
        requiredRootHandlers.every((name) => rootAdapter?.handlers.get(name)?.length === 1),
      ownedAdapterChildSuppressedBeforeRegistration:
        childAdapter !== undefined && childAdapter.handlers.size === 0,
      oneRootAndOneChildFactory: rootFactories.length === 1 && childFactories.length === 1,
      extraProbeExtensionLoaded:
        childAllTools.includes("steward_child_probe") &&
        childActiveTools.includes("steward_child_probe") &&
        typeof childProbeSource === "string",
      sameNativeAccessorAcrossFactories:
        accessors.size === 1 && factories[0]?.accessor === nativeChildContext.accessor,
      onePhysicalPinnedSubagentsPackage:
        accessorPaths.size === 1 &&
        accessorPaths.has(realpathSync(nativeChildContext.childContextPath)),
      activePiSdkAiTuiGraphShared:
        codingFromProbe.version === "0.85.1" &&
        codingFromSubagents.version === "0.85.1" &&
        codingFromProbe.manifestPath === codingFromSubagents.manifestPath &&
        aiFromSdk.version === "0.85.1" &&
        aiFromSubagents.version === "0.85.1" &&
        aiFromSdk.manifestPath === aiFromSubagents.manifestPath &&
        tuiFromSdk.version === "0.85.1" &&
        tuiFromSubagents.version === "0.85.1" &&
        tuiFromSdk.manifestPath === tuiFromSubagents.manifestPath,
      rootAgentToolRegistered: rootTools.includes("Agent"),
      childAgentToolAbsent: !childAllTools.includes("Agent"),
      stoppedAtOnSessionCreated: caught === STOP && accessorAtOnSessionCreated === false,
      zeroChildMessages: childMessagesAtCallback === 0 && childMessagesAfter === 0,
      zeroAgentStarts: agentStarts === 0,
      zeroProviderStarts: providerStarts === 0,
      zeroNetworkAttempts: networkAttempts.length === 0,
      parentClassifierFalseAfterChild: isPiChildSession() === false,
    };
    const proofMet = Object.values(assertions).every(Boolean);
    const output = {
      proofMet,
      package: {
        subagentsVersion: PACKAGE_VERSION,
        childContextPath: realpathSync(nativeChildContext.childContextPath),
        codingAgentManifest: codingFromProbe.manifestPath,
        piAiManifest: aiFromSdk.manifestPath,
        piTuiManifest: tuiFromSdk.manifestPath,
      },
      rootLoad: {
        extensionPaths: rootLoad.extensions.map((extension) => extension.path),
        errors: rootLoad.errors,
      },
      observations: {
        factories: factories.map((factory) => ({
          childAtFactory: factory.childAtFactory,
          resolvedChildContext: realpathSync(factory.resolvedChildContext),
        })),
        sessionStarts,
        rootAdapterHandlers: rootAdapter === undefined ? [] : [...rootAdapter.handlers.keys()],
        childAdapterHandlers: childAdapter === undefined ? [] : [...childAdapter.handlers.keys()],
        childProbeSource,
        childAllTools,
        childActiveTools,
        childMessagesAtCallback,
        childMessagesAfter,
        accessorAtOnSessionCreated,
        caught: caught instanceof Error ? caught.message : String(caught),
        agentStarts,
        providerStarts,
        networkAttempts,
      },
      assertions,
    };
    process.stdout.write(`${JSON.stringify(output)}\n`);
    if (!proofMet) {
      process.exitCode = 1;
    }
  } finally {
    Reflect.deleteProperty(globalThis, OBSERVER_KEY);
    await closeSession(childSession);
    await closeSession(rootSession);
  }
}
