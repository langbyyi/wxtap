const hookFileNames: Record<string, string> = {
  navigator: "nav.js",
};

export function hookScriptFileName(name: string): string {
  return hookFileNames[name] ?? `${name}.js`;
}
