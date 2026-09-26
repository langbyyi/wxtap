import { describe, expect, it } from "vitest";

import { addressTableCoverage, tableArchCandidates } from "./address-table.js";

const MAC_TABLE = { Version: 269136, Arch: { arm64: { LoadStartHookOffset: "0x4F744C4" } } };

describe("tableArchCandidates", () => {
  it("maps the names Frida reports onto the table's keys", () => {
    expect(tableArchCandidates("arm64")).toEqual(["arm64"]);
    expect(tableArchCandidates("x86_64")).toEqual(["x86_64", "x64"]);
    expect(tableArchCandidates("amd64")).toEqual(["amd64", "x64"]);
  });
});

describe("addressTableCoverage", () => {
  it("leaves the Windows layout alone: its offsets are not arch-keyed", () => {
    expect(addressTableCoverage({ LoadStartHookOffset: "0x1", CDPFilterHookOffset: "0x2" }, "x64")).toEqual({
      kind: "unchecked",
    });
  });

  it("accepts an arch-keyed table that lists this arch", () => {
    expect(addressTableCoverage(MAC_TABLE, "arm64")).toEqual({ kind: "covered" });
  });

  it("accepts a table that keys the arch Frida reports under its alias", () => {
    const table = { Version: 1, Arch: { x64: { LoadStartHookOffset: "0x1" } } };
    expect(addressTableCoverage(table, "x86_64")).toEqual({ kind: "covered" });
  });

  it("reads the lowercase arch key too, as hook.js does", () => {
    const table = { Version: 1, arch: { arm64: { LoadStartHookOffset: "0x1" } } };
    expect(addressTableCoverage(table, "arm64")).toEqual({ kind: "covered" });
  });

  it("reports the architectures a table does carry when this one is absent", () => {
    expect(addressTableCoverage(MAC_TABLE, "x64")).toEqual({ kind: "missing-arch", available: ["arm64"] });
  });

  it("does not count a section without offsets as coverage", () => {
    const table = { Version: 1, Arch: { arm64: { Note: "recovered later" } } };
    expect(addressTableCoverage(table, "arm64")).toEqual({ kind: "missing-arch", available: ["arm64"] });
  });

  it("has no opinion about values that are not tables", () => {
    for (const value of [undefined, null, 42, "offsets", []]) {
      expect(addressTableCoverage(value, "arm64")).toEqual({ kind: "unchecked" });
    }
  });
});
