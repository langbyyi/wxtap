import { DEFAULT_CDP_PORT } from './ports';

const MINIAPP_PAGE = /^\/(wx[0-9a-f]{6,})\/(\d+)\/page-frame\.html$/i;
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
  let url: URL | undefined;
  try { url = new URL(target.url ?? ""); } catch { /* Unknown and internal targets keep their generic role. */ }
  const webPage = url?.protocol === 'http:' || url?.protocol === 'https:';
  const miniappHost = webPage && url?.hostname === 'servicewechat.com';
  const page = miniappHost ? MINIAPP_PAGE.exec(url?.pathname ?? '') : null;
  if (page) {
    return { role: "小程序页面", appid: page[1] ?? "", version: page[2] ?? "", miniappPage: true };
  }
  if (miniappHost) {
    return { role: "小程序资源", appid: "", version: "", miniappPage: false };
  }
  if (target.type === "worker" || target.type === "service_worker") {
    return { role: "逻辑层", appid: "", version: "", miniappPage: false };
  }
  // LiteApp 根页面是微信运行时容器，业务内容位于其他目标。
  if (webPage && url?.hostname === 'liteapp.weixin.qq.com' && url.pathname === '/') {
    return { role: '微信容器', appid: '', version: '', miniappPage: false };
  }
  if (webPage && (target.type === 'page' || target.type === 'iframe')) {
    return { role: 'H5 候选', appid: '', version: '', miniappPage: false };
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
