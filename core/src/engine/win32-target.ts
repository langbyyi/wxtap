import { wmpfTargetError } from "./wmpf-target-error.js";

export type ProcessInfo = {
  pid: number;
  name: string;
  parameters: Record<string, unknown>;
};

export type WindowsWmpfTarget = {
  pid: number;
  version: number;
};

function isWeChatWmpfHostPath(path: unknown): path is string {
  return typeof path === "string" && /\\wechat(?:\\|$)/i.test(path.replaceAll("/", "\\"));
}

function isXWeChatRadiumWmpfHostPath(path: unknown): path is string {
  return typeof path === "string"
    && /\\tencent\\xwechat\\xplugin\\plugins\\radiumwmpf\\/i.test(path.replaceAll("/", "\\"));
}

export function isWindowsWeChatProcess(process: ProcessInfo): boolean {
  const name = process.name.toLowerCase();
  if (name === "wechat.exe") return true;
  if (name !== "weixin.exe") return false;
  return typeof process.parameters.path === "string"
    && /\\tencent\\weixin\\weixin\.exe$/i.test(process.parameters.path.replaceAll("/", "\\"));
}

function weChatInstallRoot(path: unknown): string | undefined {
  if (typeof path !== "string") return undefined;
  const normalised = path.replaceAll("/", "\\").toLowerCase();
  const marker = "\\wechat\\";
  const index = normalised.indexOf(marker);
  return index === -1 ? undefined : normalised.slice(0, index + marker.length);
}

function findWeChatAncestor(host: ProcessInfo, processesByPID: Map<number, ProcessInfo>): ProcessInfo | undefined {
  const visited = new Set<number>([host.pid]);
  let process = host;
  while (typeof process.parameters.ppid === "number" && process.parameters.ppid > 0) {
    const parent = processesByPID.get(process.parameters.ppid);
    if (parent === undefined || visited.has(parent.pid)) return undefined;
    if (isWindowsWeChatProcess(parent)) return parent;
    visited.add(parent.pid);
    process = parent;
  }
  return undefined;
}

export function findWindowsWmpfTarget(processes: ProcessInfo[]): WindowsWmpfTarget {
  const workers = processes.filter((process) => process.name === "WeChatAppEx.exe");
  if (workers.length === 0) {
    throw wmpfTargetError("no_host", "未找到微信 WMPF 宿主进程（WeChatAppEx.exe）：请先登录微信，并打开任意小程序后再启动引擎");
  }

  const parentCounts = new Map<number, number>();
  for (const worker of workers) {
    const ppid = worker.parameters.ppid;
    if (typeof ppid === "number" && ppid > 0) {
      parentCounts.set(ppid, (parentCounts.get(ppid) ?? 0) + 1);
    }
  }
  if (parentCounts.size === 0) {
    throw wmpfTargetError("no_main_process", "微信 WMPF 宿主进程信息不完整（拿不到父进程）：请重开小程序后重试");
  }
  const processesByPID = new Map(processes.map((process) => [process.pid, process]));
  const hosts = [...parentCounts.keys()]
    .map((pid) => processesByPID.get(pid))
    .filter((process): process is ProcessInfo => process !== undefined);
  if (hosts.length === 0) {
    throw wmpfTargetError("no_ancestor", "未能定位微信主进程：请确认微信已登录且未被安全软件隔离");
  }
  const candidates = hosts.filter((host) => {
    const ancestor = findWeChatAncestor(host, processesByPID);
    if (ancestor === undefined) return false;
    if (isXWeChatRadiumWmpfHostPath(host.parameters.path)) return true;
    return isWeChatWmpfHostPath(host.parameters.path)
      && weChatInstallRoot(host.parameters.path) === weChatInstallRoot(ancestor.parameters.path);
  });
  if (candidates.length !== 1) {
    throw wmpfTargetError("ambiguous_host", "未找到可验证的微信 WMPF 宿主：宿主进程与已登录微信不匹配，请确认同时只登录一个微信实例");
  }
  const parent = candidates[0];
  const executablePath = parent.parameters.path;
  const versionText = typeof executablePath === "string" ? executablePath.match(/\d+/g)?.at(-1) : undefined;
  const version = versionText === undefined ? NaN : Number.parseInt(versionText, 10);
  if (!Number.isSafeInteger(version) || version <= 0) {
    throw wmpfTargetError("no_version", "无法从微信路径推断 WMPF 构建号：请确认微信安装目录未被移动或改名");
  }
  return { pid: parent.pid, version };
}
