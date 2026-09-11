# Pi child-context coupling

Steward's Pi root/child boundary depends on `@tintinweb/pi-subagents` **0.19.0** and Pi **0.85.1**. The dependency is intentionally exact: child classification is valid only when Steward and pi-subagents observe the same native `dist/child-context.js` module and therefore the same `AsyncLocalStorage` instance.

## Extension installation

Pi must load `runtime/pi-subagents.mjs`, not the package's declared `src/index.ts` extension entry. The wrapper resolves the installed 0.19.0 package, loads its compiled `dist/index.js` with Node's native `createRequire`, validates its default extension factory, and delegates to that factory. It contains no subagent behavior or child marker of its own.

A Steward Pi extension should import `isPiChildSession` from `runtime/pi-child-context.mjs` and capture its result immediately in the extension factory:

```js
import { isPiChildSession } from "./pi-child-context.mjs";

export default function stewardExtension(pi) {
  const isChild = isPiChildSession();
  // Register root-only behavior only when isChild is false.
}
```

Do not defer classification to `session_start` or a later callback. Pi-subagents scopes its marker around child resource loading and session construction; the accessor is expected to be `false` again by `onSessionCreated` and `session_start`.

The classifier resolves the same installed package and loads its compiled `dist/child-context.js` through native `createRequire`. A missing package, wrong version, missing accessor, or non-boolean accessor result throws the fixed visible coupling error. A coupling failure is never treated as a root session.

## Physical Pi graph

The wrapper's native package graph must bind these pi-subagents peers to the physical Pi runtime used by the parent TUI:

- `@earendil-works/pi-coding-agent` 0.85.1
- `@earendil-works/pi-ai` 0.85.1
- `@earendil-works/pi-tui` 0.85.1

Matching version strings are not enough. Loading another physical SDK, AI, or TUI copy in the same process can split module identity. The published coding-agent package carries a shrinkwrap that initially installs nested AI and TUI copies. Steward's exact root pins and lock provide the shared copies. The owned `prepare:pi-runtime` package-preparation step validates the root SDK and peers at 0.85.1, validates any generated nested copies, and then removes only those two exact shrinkwrapped duplicates so coding-agent and pi-subagents both resolve the root manifests. Normal installs invoke the same step from `postinstall`.

An install performed with `npm ci --ignore-scripts` intentionally leaves the raw generated graph unaligned. Run `npm run prepare:pi-runtime` explicitly before tests or packaging; CI keeps lifecycle hooks disabled and runs this narrowly scoped preparation as its next Node step. Invalid, missing, corrupt, or symlinked package paths fail preparation rather than being silently pruned.

The construction regression verifies this actual prepared worktree graph before constructing a real extension-enabled `general-purpose` child. It does not copy the sources or assemble a substitute peer graph for its positive proof.

`packages.steward-pi-runtime` independently makes this binding in the packaged graph and installs one compiled pi-subagents copy with the owned wrapper. See [Nix packaging](packaging.md) for its evaluated paths and package smoke probe. Current Nix consumers are unchanged; consumer cutover remains a separate deployment step.

Steward's sessionless composition and quota helpers run in separate processes and do not import this wrapper. Their package identity is independent of this in-process extension coupling.

## Maintained proof

`runtime/pi-child-context.test.mjs` launches `runtime/pi-child-probe.mjs` in a sanitized temporary home with offline startup and userland network calls blocked. Through Pi's public `DefaultResourceLoader` and `createAgentSession` APIs it loads the owned wrapper and probe extension, binds a root session, and invokes the real compiled `runAgent(..., "general-purpose", ...)` export with extensions enabled.

The probe throws from `onSessionCreated` before `prompt()`. It verifies root/child construction-time classification, native accessor identity, physical Pi peer identity, root-only `Agent` registration, child probe loading, zero child messages, and zero agent/provider/network starts. A second real construction with an independent child-context copy is required to fail, demonstrating that the test does not pass on a mocked or guessed child marker.
