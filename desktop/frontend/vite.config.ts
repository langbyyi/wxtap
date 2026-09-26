import { existsSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath, URL } from 'node:url';
import { defineConfig } from 'vite';
import vue from '@vitejs/plugin-vue';

// 发布脚本会把本目录暂存到 desktop/build/frontend-stage，那里的 ../wails.json
// 不存在，因此从配置所在目录向上查找。
function readWailsJson(): { info?: { productVersion?: string } } {
  let dir = dirname(fileURLToPath(import.meta.url));
  for (;;) {
    const file = join(dir, 'wails.json');
    if (existsSync(file)) return JSON.parse(readFileSync(file, 'utf8'));
    const parent = dirname(dir);
    if (parent === dir) return {};
    dir = parent;
  }
}

const wails = readWailsJson();
const productVersion = wails.info?.productVersion || '0.0.0';
const appVersion = productVersion.startsWith('v') ? productVersion : `v${productVersion}`;

export default defineConfig({
  define: {
    __WXTAP_VERSION__: JSON.stringify(appVersion),
  },
  plugins: [vue()],
  // 本机 IPv6 回环不可用（::1 连接一律 EACCES），绑定 IPv4 回环以保证
  // localhost 能访问到 dev server。
  server: {
    host: '127.0.0.1',
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  test: {
    environment: 'jsdom',
  },
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
});
