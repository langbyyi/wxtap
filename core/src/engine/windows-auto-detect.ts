export type DetectedWindowsOffsets = {
  loadStart: number;
  cdpFilterCandidates: number[];
  sceneOffsets: number[];
};

/** First WeChat build known to use the newer launch-config layout (it needs
 * CastToJson plus MiniAppConfigStructOffsets instead of six SceneOffsets).
 * The detector below recovers the older layout only — it does no disassembly —
 * so a build at or after this one must not be handed a synthesized table, or it
 * would attach and then hook nothing. */
export const MODERN_LAYOUT_BUILD = 25710;

export type WindowsAddressTable = {
  Version: number;
  LoadStartHookOffset: string;
  CDPFilterHookOffset: string;
  /** Detected builds only: every narrowed candidate gets hooked; the hook's
   * runtime guard makes wrong candidates inert. Static tables stay single-valued. */
  CDPFilterHookOffsets?: string[];
  SceneOffsets: number[];
};

export function parseDetectedWindowsTable(value: DetectedWindowsOffsets, moduleSize: number, version: number): WindowsAddressTable {
  if (!Number.isSafeInteger(moduleSize) || moduleSize <= 0) throw new Error("invalid module size");
  if (!Number.isSafeInteger(value.loadStart) || value.loadStart <= 0 || value.loadStart >= moduleSize) {
    throw new Error("detected hook offset is outside module");
  }
  // The detector narrows CDPFilter to a handful of candidates; every one is
  // hooked and the hook's runtime guard (patch only when [arg]+8 == 6) makes
  // wrong candidates inert.
  if (!Array.isArray(value.cdpFilterCandidates) || value.cdpFilterCandidates.length === 0 || value.cdpFilterCandidates.length > 16) {
    throw new Error("invalid CDPFilter candidate count");
  }
  if (new Set(value.cdpFilterCandidates).size !== value.cdpFilterCandidates.length) {
    throw new Error("duplicate CDPFilter candidate");
  }
  for (const offset of value.cdpFilterCandidates) {
    if (!Number.isSafeInteger(offset) || offset <= 0 || offset >= moduleSize) throw new Error("detected hook offset is outside module");
  }
  if (value.sceneOffsets.length !== 6
    || value.sceneOffsets.some((offset) => !Number.isSafeInteger(offset) || offset < 0 || offset > 0x10000)
    || value.sceneOffsets.slice(0, -1).some((offset) => offset % 8 !== 0)
    || value.sceneOffsets.at(-1)! % 4 !== 0) {
    throw new Error("invalid SceneOffsets");
  }
  return {
    Version: version,
    LoadStartHookOffset: `0x${value.loadStart.toString(16)}`,
    // Static tables carry the single reversed offset; detected builds hook
    // every candidate, so the list form replaces the single value.
    CDPFilterHookOffset: `0x${value.cdpFilterCandidates[0].toString(16)}`,
    CDPFilterHookOffsets: value.cdpFilterCandidates.map((offset) => `0x${offset.toString(16)}`),
    SceneOffsets: value.sceneOffsets,
  };
}
