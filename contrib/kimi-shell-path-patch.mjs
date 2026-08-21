#!/usr/bin/env node
// Patch an npm-installed @moonshot-ai/kimi-code bundle so the POSIX shell
// probe honors KIMI_SHELL_PATH, on every platform.
//
// Why: kimi resolves its Bash-tool shell from a hardcoded candidate list
// (/bin/bash, /usr/bin/bash, /usr/local/bin/bash, else /bin/sh) and spawns it
// by absolute path. KIMI_SHELL_PATH exists in the source but is only consulted
// on Windows (locateWindowsGitBash). No config key, env var, hook, or plugin
// can redirect the shell on POSIX, which strands tools like reach that need
// the harness's shell to be a program of their choosing.
//
// The upstream-shaped fix is a two-file change:
//   packages/agent-core-v2/src/_base/execEnv/environmentProbe.ts (probeHostEnvironment)
//   packages/kaos/src/environment.ts                            (detectEnvironment)
// prepending process.env.KIMI_SHELL_PATH to the candidate list. This script
// applies exactly that change to the bundled dist/main.mjs of an npm install,
// where both probe sites survive bundling verbatim.
//
// Usage:
//   node contrib/kimi-shell-path-patch.mjs <path-to-dist/main.mjs>
//
// Idempotent: exits 0 without changes when the patch is already applied.
// Fails loudly when the expected probe sites are not found (bundle layout
// changed — re-derive the patch instead of applying blindly).

import { readFileSync, writeFileSync } from "node:fs";

const file = process.argv[2];
if (!file) {
  console.error("usage: node kimi-shell-path-patch.mjs <path-to-dist/main.mjs>");
  process.exit(2);
}

const ORIGINAL = `const candidates = [
		"/bin/bash",
		"/usr/bin/bash",
		"/usr/local/bin/bash"
	];`;
const PATCHED = `const candidates = [
		...process.env.KIMI_SHELL_PATH ? [process.env.KIMI_SHELL_PATH] : [],
		"/bin/bash",
		"/usr/bin/bash",
		"/usr/local/bin/bash"
	];`;

const bundle = readFileSync(file, "utf8");

if (bundle.includes("process.env.KIMI_SHELL_PATH ? [process.env.KIMI_SHELL_PATH]")) {
  console.log(`${file}: already patched, nothing to do`);
  process.exit(0);
}

const occurrences = bundle.split(ORIGINAL).length - 1;
// Two probe sites ship in the bundle: the v2 engine's environmentProbe and
// the legacy v1 kaos detectEnvironment. Both must be patched; patching only
// one would make the seam depend on which engine a release defaults to.
if (occurrences !== 2) {
  console.error(
    `${file}: expected exactly 2 shell-probe candidate lists, found ${occurrences}.\n` +
      "The bundle layout changed; re-derive the patch from the kimi-code source\n" +
      "(packages/agent-core-v2/src/_base/execEnv/environmentProbe.ts and\n" +
      "packages/kaos/src/environment.ts) instead of applying this blindly.",
  );
  process.exit(1);
}

writeFileSync(file, bundle.split(ORIGINAL).join(PATCHED));
console.log(`${file}: patched ${occurrences} shell-probe sites to honor KIMI_SHELL_PATH`);
