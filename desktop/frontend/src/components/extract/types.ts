// Shared view types for the extract page and its child panels.
//
// The panels receive the same app and finding objects the view builds and hand
// them back through their events, so both sides need one definition: two
// structurally similar local copies drift apart and make the handler
// signatures unassignable in one direction (vue-tsc reports the panel's
// narrower type as the mismatch).
export type Severity = 'critical' | 'high' | 'medium' | 'low' | 'info';

export type ExtractPackage = {
  appid: string;
  name?: string;
  path: string;
  mtime?: number;
  decompiled?: boolean;
  output_dir?: string;
  // 后端判定这个包还原不了（页面模板是微信新版编译模板运行时生成的）：它不进
  // 可反编译列表，也不计入小程序数量。
  unsupported?: boolean;
};

// 这张卡片只渲染 AppID、图标、包数量与状态：后端给的 subject / icon_path /
// icon_confidence / metadata_source 这里不再中转（页面不显示它们，图标来源标签
// 是刻意去掉的），要用时在 refresh() 的映射里加一行即可。
export type ExtractApp = {
  appid: string;
  name: string;
  packages: ExtractPackage[];
  decompiled: boolean;
  scanned: boolean;
  mtime: number;
  outputDir: string;
  iconDataURL: string;
  iconBroken: boolean;
  // 同一 appid 的任一包还原不了就为真，页面据此把它排除出列表与计数。
  unsupported?: boolean;
};

// confidence, privilege and tags are classification metadata the scan emits:
// only privilege is read today (by the findings search in ExtractView), so the
// rest stay optional and the panels are free to omit them.
export type ExtractFinding = {
  id: string;
  rule_id?: string;
  category: string;
  title: string;
  severity: Severity;
  confidence?: string;
  value: string;
  masked: string;
  file: string;
  line: number;
  column?: number;
  snippet?: string;
  privilege?: string;
  tags?: string[];
};
