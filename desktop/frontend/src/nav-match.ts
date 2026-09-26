// 分组高亮只认完整路径，或这条路径下的子路径。
// `/code` 不能把 `/code-browser` 算进同一组。
export function navItemMatches(path: string, itemPath: string): boolean {
  return path === itemPath || path.startsWith(`${itemPath}/`);
}
