import { ref, watch } from 'vue';

// 列表页的密度偏好：行高就是容量——几百上千条的列表里，一行省下十几像素就是一屏多两行。
// 按页面各记一份（不写进 config：它只是这台机器上的观感偏好，不该跨设备同步、也不该进
// 用户配置），默认「紧凑」——会盯着这些列表翻数据的人，先要的是吞吐。
//
// 读取一律走 try/catch：隐私模式下 localStorage 可能直接抛，记住偏好不是功能本身，
// 抛了就当没记过。
export type Density = 'compact' | 'comfort';

const STORAGE_PREFIX = 'wxtap-density:';

function readPref(key: string): Density | undefined {
  try {
    const stored = localStorage.getItem(key);
    return stored === 'compact' || stored === 'comfort' ? stored : undefined;
  } catch {
    return undefined;
  }
}

function writePref(key: string, value: Density) {
  try {
    localStorage.setItem(key, value);
  } catch {
    // 记不住就算了：这次会话里的切换照常生效。
  }
}

/**
 * 一个列表页的密度开关。`key` 决定偏好存在哪一格（一页一个），`initial` 是没有偏好时的默认值。
 * 用法：`const { compact } = useDensity('traffic')`，列表容器上 `:class="{ 'is-compact': compact }"`。
 */
export function useDensity(key: string, initial: Density = 'compact') {
  const storageKey = `${STORAGE_PREFIX}${key}`;
  const stored = readPref(storageKey);
  const compact = ref(stored ? stored === 'compact' : initial === 'compact');
  // flush: 'sync'——写入跟着这次切换立刻发生，而不是等下一个 tick：偏好是「用户刚做的
  // 决定」，不该受渲染时序影响（也省得调用方在断言或跳转前先 await 一拍）。
  watch(compact, (value) => writePref(storageKey, value ? 'compact' : 'comfort'), { flush: 'sync' });
  return { compact };
}
