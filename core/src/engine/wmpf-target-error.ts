/**
 * WMPF 宿主定位失败的结构化标记。
 *
 * 这些错误会一路冒到界面上（状态页的「微信 / 版本支持」与启动失败提示），所以
 * 文案是中文的、可变的；而调用方（`weChatHostNote`）需要按**失败类型**决定该
 * 显示哪句提示。靠 message 里的英文关键字去猜会在文案改动时静默失效，因此把
 * 类型挂在 error.code 上。
 */
export type WmpfTargetFailureCode =
  /** 找不到 WeChatAppEx 宿主进程（微信没开 / 没打开过小程序） */
  | "no_host"
  /** 宿主进程拿不到父进程，无法确认它属于哪个微信 */
  | "no_main_process"
  /** 有宿主进程但无法定位到已登录的微信主进程 */
  | "no_ancestor"
  /** 宿主进程与已登录微信不匹配，或同时存在多个候选 */
  | "ambiguous_host"
  /** 无法从安装路径推断 WMPF 构建号 */
  | "no_version";

export type WmpfTargetError = Error & { code: WmpfTargetFailureCode };

/** 构造带失败类型的宿主定位错误。 */
export function wmpfTargetError(code: WmpfTargetFailureCode, message: string): WmpfTargetError {
  const error = new Error(message) as WmpfTargetError;
  error.code = code;
  return error;
}

/** 读取失败类型；不是本模块抛出的错误返回 undefined。 */
export function wmpfTargetFailureCode(error: unknown): WmpfTargetFailureCode | undefined {
  const code = (error as { code?: unknown } | undefined)?.code;
  switch (code) {
    case "no_host":
    case "no_main_process":
    case "no_ancestor":
    case "ambiguous_host":
    case "no_version":
      return code;
    default:
      return undefined;
  }
}
