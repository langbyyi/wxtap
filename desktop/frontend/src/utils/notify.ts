import { readonly, ref } from 'vue';
import { messageOf } from './format';

export type NoticeTone = 'info' | 'success' | 'error';
export type Notice = { id: number; message: string; tone: NoticeTone };

const notices = ref<Notice[]>([]);
const timers = new Map<number, ReturnType<typeof setTimeout>>();
let nextId = 0;

export function notify(message: string, tone: NoticeTone = 'info', duration = 3200) {
  const text = message.trim();
  if (!text) return;
  const id = ++nextId;
  const next = [...notices.value, { id, message: text, tone }];
  while (next.length > 4) {
    const removed = next.shift();
    if (removed) {
      const timer = timers.get(removed.id);
      if (timer) clearTimeout(timer);
      timers.delete(removed.id);
    }
  }
  notices.value = next;
  timers.set(id, setTimeout(() => dismissNotice(id), duration));
}

export function dismissNotice(id: number) {
  const timer = timers.get(id);
  if (timer) clearTimeout(timer);
  timers.delete(id);
  notices.value = notices.value.filter((notice) => notice.id !== id);
}

// navigator.clipboard only exists in a secure context, and Wails serves the
// frontend from a custom scheme on macOS, which is not guaranteed to qualify.
// The runtime's native clipboard is the fallback, so the copy buttons work on
// both platforms instead of failing on the one where the web API is missing.
async function writeNativeClipboard(value: string): Promise<boolean> {
  const setText = window.runtime?.ClipboardSetText;
  if (!setText) return false;
  try {
    return await setText(value);
  } catch {
    return false;
  }
}

async function writeClipboard(value: string): Promise<void> {
  if (!navigator.clipboard?.writeText) {
    if (await writeNativeClipboard(value)) return;
    throw new Error('当前环境没有可用的剪贴板');
  }
  try {
    await navigator.clipboard.writeText(value);
  } catch (reason) {
    if (!(await writeNativeClipboard(value))) throw reason;
  }
}

export async function copyText(value: string, label = '内容') {
  try {
    await writeClipboard(value);
    notify(`${label}已复制`, 'success');
    return true;
  } catch (reason) {
    notify(`复制${label}失败：${messageOf(reason)}`, 'error');
    return false;
  }
}

export function useNotices() {
  return { notices: readonly(notices), dismissNotice };
}
