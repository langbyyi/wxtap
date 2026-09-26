import { describe, expect, it } from "vitest";

import { parseDetectedWindowsTable } from "./windows-auto-detect.js";

describe("parseDetectedWindowsTable", () => {
  it("formats a unique detector result for hook injection", () => {
    expect(
      parseDetectedWindowsTable(
        { loadStart: 0x20, cdpFilterCandidates: [0x40, 0x50, 0x60], sceneOffsets: [64, 1536, 8, 1472, 16, 456] },
        0x100,
        99999,
      ),
    ).toEqual({
      Version: 99999,
      LoadStartHookOffset: "0x20",
      CDPFilterHookOffset: "0x40",
      CDPFilterHookOffsets: ["0x40", "0x50", "0x60"],
      SceneOffsets: [64, 1536, 8, 1472, 16, 456],
    });
  });

  it("rejects an incomplete scene pointer chain", () => {
    expect(() =>
      parseDetectedWindowsTable({ loadStart: 0x20, cdpFilterCandidates: [0x40], sceneOffsets: [1] }, 0x100, 99999),
    ).toThrow("SceneOffsets");
  });

  it("rejects a detector run that produced no CDPFilter candidates", () => {
    expect(() =>
      parseDetectedWindowsTable({ loadStart: 0x20, cdpFilterCandidates: [], sceneOffsets: [64, 1536, 8, 1472, 16, 456] }, 0x100, 99999),
    ).toThrow("CDPFilter");
  });

  it("rejects candidates outside the module", () => {
    expect(() =>
      parseDetectedWindowsTable(
        { loadStart: 0x20, cdpFilterCandidates: [0x40, 0x200], sceneOffsets: [64, 1536, 8, 1472, 16, 456] },
        0x100,
        99999,
      ),
    ).toThrow("outside module");
  });

  it("rejects duplicate filter candidates instead of installing the same hook twice", () => {
    expect(() =>
      parseDetectedWindowsTable(
        { loadStart: 0x20, cdpFilterCandidates: [0x40, 0x40], sceneOffsets: [64, 1536, 8, 1472, 16, 456] },
        0x100,
        99999,
      ),
    ).toThrow("duplicate CDPFilter");
  });

  it("rejects an implausibly unaligned scene pointer chain", () => {
    expect(() =>
      parseDetectedWindowsTable(
        { loadStart: 0x20, cdpFilterCandidates: [0x40], sceneOffsets: [65, 1536, 8, 1472, 16, 456] },
        0x100,
        99999,
      ),
    ).toThrow("SceneOffsets");
  });
});
