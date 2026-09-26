// End-to-end smoke for the built bundle: the vitest toolchain resolves CJS
// deps with its own interop, so only running dist/cli.js under plain node
// proves the shipped artifact actually loads (protobufjs/ws named exports).
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import process from "node:process";

const cliPath = join(dirname(fileURLToPath(import.meta.url)), "..", "dist", "cli.js");
const bundle = readFileSync(cliPath, "utf8");
if (bundle.includes("frida_binding")) {
  console.error("check-dist: frida native bindings were bundled; keep frida external so it resolves its .node file correctly");
  process.exit(1);
}
const child = spawn(process.execPath, [cliPath], { stdio: ["pipe", "pipe", "pipe"] });

const requests = [
  { id: 1, method: "engine.status", params: {} },
  { id: 2, method: "engine.status", params: {} },
];
child.stdin.write(`${requests.map((r) => JSON.stringify(r)).join("\n")}\n`);

let output = "";
const timeout = setTimeout(() => {
  child.kill();
  console.error("check-dist: timed out waiting for RPC responses");
  console.error("stderr:", child.stderr.read()?.toString() ?? "(none)");
  process.exit(1);
}, 15000);

child.stdout.on("data", (chunk) => {
  output += chunk.toString();
  const lines = output.split("\n");
  output = lines.pop() ?? "";
  for (const line of lines) {
    if (line.length === 0) continue;
    const response = JSON.parse(line);
    if (response.id !== 1 && response.id !== 2) {
      clearTimeout(timeout);
      console.error(`check-dist: unexpected response id ${response.id}`);
      process.exit(1);
    }
    // Either a result or a structured error proves the RPC layer is alive.
    if (!("result" in response) && !("error" in response)) {
      clearTimeout(timeout);
      console.error(`check-dist: malformed response ${line}`);
      process.exit(1);
    }
    requests.splice(requests.findIndex((r) => r.id === response.id), 1);
    if (requests.length === 0) {
      clearTimeout(timeout);
      child.kill();
      console.log("check-dist: bundle serves RPC correctly");
      process.exit(0);
    }
  }
});

child.stderr.on("data", (chunk) => {
  console.error("core stderr:", chunk.toString());
});

child.on("exit", (code) => {
  clearTimeout(timeout);
  console.error(`check-dist: core exited early with code ${code}`);
  process.exit(1);
});
