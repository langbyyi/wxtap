/**
 * macOS WMPF host detection.
 *
 * The host is the process the WMPF framework is loaded into. It is matched on
 * the bundle it runs from first (see isMacWmpfHostProcess), because that is how
 * a working macOS port locates it; the shape Windows uses — the WMPF workers'
 * parent — is kept as the fallback, since that is what WMPFDebugger's darwin
 * platform does and it is the route this project has always taken.
 *
 * The build number does NOT come from the install path the way it does on
 * Windows. A macOS install lives at /Applications/WeChat.app/…, which carries
 * no build digits at all; WMPF's own CFBundleVersion inside the nested
 * WeChatAppEx.app bundle is what the address tables under
 * resources/frida/config/mac are named after.
 *
 * UNVERIFIED ON REAL HARDWARE (mac 实机待验证) — stated plainly because the
 * checklist points here: no macOS implementation of this is known to have been
 * confirmed working. Upstream WMPFDebugger labels its own macOS support "not
 * working yet" in the commit that added it, and the third-party macOS port is
 * the only one claiming real operation. So neither the Helper's name, nor the
 * Helper's parent, nor the plist contents can be assumed — which is why the
 * host match does not depend on the first two, and parseMacWmpfVersion accepts
 * both a bare and a dotted CFBundleVersion.
 */
import { readFile } from "node:fs/promises";

import type { ProcessInfo } from "./win32-target.js";
import { wmpfTargetError } from "./wmpf-target-error.js";

export type { ProcessInfo };

export type MacWmpfTarget = {
  pid: number;
  version: number;
};

/** Name of the WMPF worker processes (macOS spells the host app's workers with a space). */
const MAC_WMPF_HELPER = "WeChatAppEx Helper";

/** Path fragment identifying the main WMPF host process. */
const MAC_WMPF_HOST_BUNDLE = "/WeChatAppEx.app/Contents/MacOS/WeChatAppEx";

/**
 * The main WMPF host, matched on the path it runs from.
 *
 * A working macOS port locates it exactly this way (`pgrep -f
 * '/MacOS/WeChatAppEx.app/Contents/MacOS/WeChatAppEx'`), and the path is the
 * safer signal of the two: macOS caps how long a recorded process name may be,
 * so a name match on the 18-character "WeChatAppEx Helper" is not something
 * this project can assume holds on a real machine. The path also cannot pick up
 * a Helper, which runs from Contents/Frameworks/WeChatAppEx Helper.app.
 */
function isMacWmpfHostProcess(process: ProcessInfo): boolean {
  const path = process.parameters.path;
  if (typeof path === "string" && path.includes(MAC_WMPF_HOST_BUNDLE)) {
    return true;
  }
  // With no path reported the name is all that is left. A Helper is spelled
  // differently, so this cannot pick one up.
  return process.name === "WeChatAppEx";
}

/** The pid to attach to, or the error explaining why there is none. */
function resolveMacHostPid(processes: ProcessInfo[]): number {
  const hosts = processes.filter(isMacWmpfHostProcess);
  if (hosts.length === 1) {
    return hosts[0].pid;
  }
  if (hosts.length > 1) {
    throw wmpfTargetError("ambiguous_host", "发现多个微信 WMPF 宿主进程：请确认同时只登录一个微信实例");
  }

  // Fall back to the shape Windows uses: the WMPF workers' parent. Kept because
  // it is how this project has always located the mac host, but it rests on the
  // Helpers existing under that exact name and being parented by the host —
  // neither of which is verified on real hardware here.
  const helpers = processes.filter((process) => process.name === MAC_WMPF_HELPER);
  if (helpers.length === 0) {
    throw wmpfTargetError(
      "no_host",
      "未找到微信 WMPF 宿主进程（WeChatAppEx / WeChatAppEx Helper）：请先登录微信，并打开任意小程序后再启动引擎",
    );
  }

  // Every helper is a worker of the same host, so their ppids must agree. Two
  // distinct parents means two WeChat instances and no way to tell which one
  // the user is looking at.
  const hostPids = new Set<number>();
  for (const helper of helpers) {
    const ppid = helper.parameters.ppid;
    if (typeof ppid === "number" && ppid > 0) {
      hostPids.add(ppid);
    }
  }
  if (hostPids.size === 0) {
    throw wmpfTargetError("no_main_process", "微信 WMPF 宿主进程信息不完整（拿不到父进程）：请重开小程序后重试");
  }
  if (hostPids.size > 1) {
    throw wmpfTargetError("ambiguous_host", "发现多个微信 WMPF 宿主进程：请确认同时只登录一个微信实例");
  }
  const pid = [...hostPids][0];

  // A helper whose parent is gone would leave a stale pid behind, and pid reuse
  // would then have us attach to an unrelated process.
  if (!processes.some((process) => process.pid === pid)) {
    throw wmpfTargetError("no_ancestor", "未能定位微信主进程：请确认微信已登录且未被安全软件隔离");
  }
  return pid;
}

/** Bundle that carries CFBundleVersion and the WMPF framework. */
export const MAC_WMPF_INFO_PLIST =
  "/Applications/WeChat.app/Contents/MacOS/WeChatAppEx.app/Contents/Info.plist";

/** The Info.plist to read, and where to fall back.
 *
 * The standard install comes first because it is the only layout ever confirmed
 * here. The second entry is the same bundle derived from where the host process
 * actually runs: a per-user WeChat in ~/Applications is a normal macOS layout,
 * and reading only the hardcoded path would make the engine unstartable there
 * even though the host was found. */
export function macInfoPlistPaths(hostExecutablePath?: string): string[] {
  const paths = [MAC_WMPF_INFO_PLIST];
  const derived = deriveMacInfoPlist(hostExecutablePath);
  if (derived !== undefined && !paths.includes(derived)) {
    paths.push(derived);
  }
  return paths;
}

/** The host's own bundle: everything up to the last .app that contains a
 * Contents/ directory, which for a normal install is the nested WeChatAppEx.app
 * and so yields the standard path unchanged. */
function deriveMacInfoPlist(hostExecutablePath: string | undefined): string | undefined {
  if (typeof hostExecutablePath !== "string") return undefined;
  const marker = ".app/Contents/";
  const index = hostExecutablePath.lastIndexOf(marker);
  if (index < 0) return undefined;
  return `${hostExecutablePath.slice(0, index + ".app".length)}/Contents/Info.plist`;
}

// Anchored on the key element, so CFBundleShortVersionString (which plists
// list first) cannot satisfy it.
const CF_BUNDLE_VERSION = /<key>\s*CFBundleVersion\s*<\/key>\s*<string>([^<]*)<\/string>/;

/**
 * WMPF build number out of the host bundle's Info.plist.
 *
 * CFBundleVersion is not necessarily the bare build number. The macOS port this
 * project's SIP prerequisite comes from derives it as the second component of a
 * dotted value shaped "4.18788.<patch>" — so on at least those builds the field
 * is dotted and the build is one component inside it. Refusing a dotted value
 * would fail to start on every such mac, so both shapes are accepted: when the
 * value is dotted the component carrying the build is the large one, because a
 * WMPF build runs to five or six digits while the marketing and patch
 * components around it are single digits.
 */
export function parseMacWmpfVersion(infoPlist: string): number {
  const text = infoPlist.match(CF_BUNDLE_VERSION)?.[1]?.trim();
  const version = buildNumberFrom(text);
  if (version === undefined) {
    throw wmpfTargetError(
      "no_version",
      `无法从 Info.plist 读取 WMPF 构建号（CFBundleVersion=${text ?? "缺失"}）：请确认微信安装在 /Applications 且未被改名`,
    );
  }
  return version;
}

/** The build number inside a CFBundleVersion, bare integer or dotted. */
function buildNumberFrom(text: string | undefined): number | undefined {
  if (text === undefined) return undefined;
  const components = text
    .split(".")
    .map((part) => Number(part.trim()))
    .filter((value) => Number.isSafeInteger(value) && value > 0);
  if (components.length === 0) return undefined;
  return Math.max(...components);
}

export async function findMacWmpfTarget(
  processes: ProcessInfo[],
  readInfoPlist: (path: string) => Promise<string> = async (path) => await readFile(path, "utf8"),
): Promise<MacWmpfTarget> {
  const pid = resolveMacHostPid(processes);

  // The host's own path is what makes a per-user install resolvable.
  const hostPath = processes.find((process) => process.pid === pid)?.parameters.path;
  const failures: string[] = [];
  let infoPlist: string | undefined;
  for (const candidate of macInfoPlistPaths(typeof hostPath === "string" ? hostPath : undefined)) {
    try {
      infoPlist = await readInfoPlist(candidate);
      break;
    } catch (error) {
      // Only an unreadable candidate moves on to the next one: once a plist has
      // been read, its contents are the answer and a parse failure in it should
      // surface as itself rather than as a "could not read" list.
      failures.push(`${candidate}：${error instanceof Error ? error.message : String(error)}`);
    }
  }
  if (infoPlist === undefined) {
    throw wmpfTargetError(
      "no_version",
      `读不到微信 WMPF 版本信息（${failures.join("；")}）：请确认微信已安装且未被改名`,
    );
  }
  return { pid, version: parseMacWmpfVersion(infoPlist) };
}
