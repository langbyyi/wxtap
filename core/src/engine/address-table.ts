/**
 * Host-side view of the arch selection that resources/frida/hook.js performs
 * inside the target process.
 *
 * The hook stays the authority — it reads Process.arch — but the host can still
 * tell whether a shipped table has a section for the machine it is running on.
 * That is the difference between "this build has no table at all" and "this
 * build's offsets were only recovered for another architecture", which is the
 * Intel-Mac case against the arm64-only table under resources/frida/config/mac:
 * the hook stays dormant rather than patching with the wrong arch's offsets, so
 * the status must not report a usable table.
 */

/** Mirrors hook.js's candidate list: Frida reports x86_64, tables are keyed x64. */
export function tableArchCandidates(arch: string): string[] {
  if (arch === "x86_64") return [arch, "x64"];
  if (arch === "amd64") return [arch, "x64"];
  return [arch];
}

export type AddressTableCoverage =
  /** Nothing for this helper to judge: the Windows layout carries its offsets at
   * the top level, and a value that is not a table at all is the hook's problem. */
  | { kind: "unchecked" }
  /** An arch-keyed table with a section for this arch. */
  | { kind: "covered" }
  /** An arch-keyed table whose sections are all for other architectures. */
  | { kind: "missing-arch"; available: string[] };

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined;
}

export function addressTableCoverage(config: unknown, arch: string): AddressTableCoverage {
  const table = asRecord(config);
  if (table === undefined) return { kind: "unchecked" };
  // Offsets at the top level mean the Windows layout, which is arch-independent.
  if (table.LoadStartHookOffset !== undefined) return { kind: "unchecked" };
  const archMap = asRecord(table.Arch) ?? asRecord(table.arch);
  if (archMap === undefined) return { kind: "unchecked" };
  const covered = tableArchCandidates(arch).some(
    (key) => asRecord(archMap[key])?.LoadStartHookOffset !== undefined,
  );
  return covered ? { kind: "covered" } : { kind: "missing-arch", available: Object.keys(archMap) };
}
