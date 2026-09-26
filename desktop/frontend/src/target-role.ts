import { DEFAULT_CDP_PORT } from './ports';

const MINIAPP_PAGE = /servicewechat\.com\/(wx[0-9a-f]{6,})\/(\d+)\/page-frame\.html/i;
// CDP target ids are opaque tokens; the DevTools frontend appends them to the
// ws endpoint as a path, so anything outside this shape is refused.
const SAFE_TARGET_ID = /^[A-Za-z0-9._-]{1,128}$/;

export type TargetRole = {
  role: string;
  appid: string;
  version: string;
  miniappPage: boolean;
};

export function describeTarget(target: { url?: string; type?: string }): TargetRole {
  const url = target.url ?? "";
  const page = MINIAPP_PAGE.exec(url);
  if (page) {
    return { role: "小程序页面", appid: page[1] ?? "", version: page[2] ?? "", miniappPage: true };
  }
  if (/servicewechat\.com/i.test(url)) {
    return { role: "小程序资源", appid: "", version: "", miniappPage: false };
  }
  if (target.type === "worker" || target.type === "service_worker") {
    return { role: "逻辑层", appid: "", version: "", miniappPage: false };
  }
  if (target.type === "page") {
    return { role: "页面", appid: "", version: "", miniappPage: false };
  }
  return { role: target.type || "其他", appid: "", version: "", miniappPage: false };
}

/** Opens the DevTools inspector through the CDP proxy. The target id is
 * validated instead of interpolated: it becomes a URL path segment, and the
 * DevTools frontend would treat anything else as a different endpoint. */
export function inspectorUrlForTarget(cdpPort: number, targetId: string): string {
  const port = Number.isInteger(cdpPort) && cdpPort >= 1 && cdpPort <= 65535 ? cdpPort : DEFAULT_CDP_PORT;
  const target = SAFE_TARGET_ID.test(targetId) ? `/devtools/page/${targetId}` : '';
  return `devtools://devtools/bundled/inspector.html?ws=127.0.0.1:${port}${target}`;
}
