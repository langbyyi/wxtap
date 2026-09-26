<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue';
import { backend } from '../api/bridge';
import { useEngineStore } from '../stores/engine';
import { messageOf } from '../utils/format';
import { copyText, notify } from '../utils/notify';
import PageHeader from '../components/PageHeader.vue';
import TaskProgress from '../components/TaskProgress.vue';

// 与后端 settings.getPaths 返回的 6 个键一一对应；漏掉的键会以原始英文键名显示。
type Paths = Record<
  'hook_scripts' | 'frida_config' | 'skills' | 'outputDir' | 'dbPath' | 'logDir',
  string
>;
type Config = { electron_path?: string; node_path?: string; [key: string]: unknown };
type Version = { current?: string; latest?: string; has_update?: boolean; notes?: string; total_size?: number; error?: string };
type UpdateStatus = { staged?: boolean; version?: string };
// 后端 NodeRuntime 的形状：path/version/source 描述解析结果，error 说明为什么不可用，
// skipped 记录「存在但用不了」的候选，避免换用另一个 Node 时无声无息。
type NodeRuntime = { path?: string; version?: string; source?: string; error?: string; skipped?: string[] };
// Electron 的解析结果：设置里填的路径优先，否则后端自己找（PATH、npm 全局、
// 常见安装位置）。source 与 Node 的那套标签同义，自动选中的不会被静默替换。
type ElectronRuntime = { path?: string; source?: string; error?: string };
const electronRuntime = ref<ElectronRuntime>({});
const electronSourceLabels: Record<string, string> = {
  config: '本页指定',
  path: '系统 PATH',
  detected: '自动检测',
};
type NodeCandidates = { candidates?: NodeRuntime[] };
const nodeSourceLabels: Record<string, string> = {
  env: '环境变量 WXTAP_CORE_CMD',
  config: '本页指定',
  path: '系统 PATH',
  detected: '自动检测',
};
const paths = ref<Partial<Paths>>({});
const config = ref<Config>({});
const electronPath = ref('');
const savedElectronPath = ref('');
const nodePath = ref('');
const savedNodePath = ref('');
const nodeRuntime = ref<NodeRuntime>({});
const nodeCandidates = ref<NodeRuntime[]>([]);
const electronCandidates = ref<ElectronRuntime[]>([]);
const store = useEngineStore();
const version = ref<Version>({});
const appVersion = __WXTAP_VERSION__;
const electronDirty = computed(() => electronPath.value.trim() !== savedElectronPath.value.trim());
const nodeDirty = computed(() => nodePath.value.trim() !== savedNodePath.value.trim());
const message = ref('');
const working = ref('');
const loading = ref(true);
// 已下载待重启的版本。它比「可更新」更强：包已经在本地校验通过，只差重启。
const stagedVersion = ref('');
const downloadTaskId = ref('');
const downloadTask = computed(() => (downloadTaskId.value ? store.tasks[downloadTaskId.value] : undefined));
const downloading = computed(() => !!downloadTask.value && !['done', 'failed', 'cancelled'].includes(downloadTask.value.phase || ''));
const updateSize = computed(() => {
  const bytes = version.value.total_size ?? 0;
  return bytes > 0 ? `${(bytes / 1024 / 1024).toFixed(1)} MB` : '';
});
const pathLabels: Record<string, string> = {
  hook_scripts: 'Hook 脚本目录',
  frida_config: 'Frida 配置目录',
  skills: '技能文档目录',
  outputDir: '反编译输出目录',
  dbPath: '流量数据库',
  logDir: '日志目录',
};
async function load() {
  loading.value = true;
  message.value = '';
  try {
    const [loadedPaths, loadedConfig, status, node, electron] = await Promise.all([
      backend.call<Paths>('settings.getPaths'),
      backend.call<Config>('config.load'),
      backend.call<UpdateStatus>('update.status'),
      backend.call<NodeRuntime>('node.status'),
      backend.call<ElectronRuntime>('electron.status'),
    ]);
    paths.value = loadedPaths;
    config.value = loadedConfig;
    electronPath.value = loadedConfig.electron_path ?? '';
    savedElectronPath.value = electronPath.value;
    nodePath.value = loadedConfig.node_path ?? '';
    savedNodePath.value = nodePath.value;
    nodeRuntime.value = node;
    electronRuntime.value = electron;
    stagedVersion.value = status.staged ? status.version ?? '' : '';
  } catch (reason) {
    message.value = messageOf(reason);
    notify(`加载设置失败：${messageOf(reason)}`, 'error');
  } finally {
    loading.value = false;
  }
}
function open(name: keyof Paths) {
  const path = paths.value[name] ?? '';
  if (!path) {
    notify(`${pathLabels[name] ?? name} 尚未配置`, 'error');
    return;
  }
  backend.call('shell.openFolder', { path })
    .then(() => notify(`已打开${pathLabels[name] ?? name}`))
    .catch((reason) => {
      message.value = messageOf(reason);
      notify(`打开目录失败：${messageOf(reason)}`, 'error');
    });
}

async function copyPath(name: keyof Paths) {
  const path = paths.value[name];
  if (path) await copyText(path, `${pathLabels[name] ?? name}路径`);
}

async function saveElectron() {
  if (!electronDirty.value || working.value === 'electron') return;
  working.value = 'electron';
  try {
    config.value = { ...config.value, electron_path: electronPath.value };
    await backend.call('config.save', config.value);
    savedElectronPath.value = electronPath.value;
    message.value = 'Electron 路径已保存';
    notify('Electron 路径已保存', 'success');
  } catch (reason) {
    message.value = messageOf(reason);
    notify(`保存失败：${messageOf(reason)}`, 'error');
  } finally {
    working.value = '';
  }
}

async function detectNode() {
  working.value = 'node-detect';
  try {
    const result = await backend.call<NodeCandidates>('node.detect');
    nodeCandidates.value = result.candidates ?? [];
    if (!nodeCandidates.value.length) {
      message.value = '未在 PATH 或常见安装位置找到可用的 Node，请手动填写路径或从 nodejs.org 安装';
      notify(message.value, 'error');
      return;
    }
    // 只检测到一个就直接填好；多个先填排在最前的那个（后端按「常见安装位置优先、
    // 版本目录从新到旧」排序），其余列出来供用户改选。
    nodePath.value = nodeCandidates.value[0].path ?? '';
    message.value = `检测到 ${nodeCandidates.value.length} 个可用的 Node`;
    notify(message.value, 'success');
  } catch (reason) {
    message.value = messageOf(reason);
    notify(`自动检测失败：${messageOf(reason)}`, 'error');
  } finally {
    working.value = '';
  }
}

// 与 detectNode 同形：后端把 PATH 上的那个排在候选最前，检测结果不会与上面那行
// 「当前使用…」互相打脸。
async function detectElectron() {
  working.value = 'electron-detect';
  try {
    const result = await backend.call<{ candidates?: ElectronRuntime[] }>('electron.detect');
    electronCandidates.value = result.candidates ?? [];
    if (!electronCandidates.value.length) {
      message.value = '未在 PATH 或常见安装位置找到可用的 Electron，请手动填写路径或执行 npm install -g electron';
      notify(message.value, 'error');
      return;
    }
    electronPath.value = electronCandidates.value[0].path ?? '';
    message.value = `检测到 ${electronCandidates.value.length} 个可用的 Electron`;
    notify(message.value, 'success');
  } catch (reason) {
    message.value = messageOf(reason);
    notify(`自动检测失败：${messageOf(reason)}`, 'error');
  } finally {
    working.value = '';
  }
}

async function saveNode() {
  if (!nodeDirty.value || working.value === 'node') return;
  working.value = 'node';
  message.value = '';
  try {
    const path = nodePath.value.trim();
    // 先校验再落盘：这里的笔误会让引擎起不来，而把一条能用的路径覆盖成坏的
    // 不是用户能自己走出来的状态。
    if (path) {
      const probed = await backend.call<NodeCandidates>('node.detect', { path });
      const failure = probed.candidates?.[0]?.error;
      if (failure) {
        message.value = `无法使用该路径：${failure}`;
        notify(message.value, 'error');
        return;
      }
    }
    const next = { ...config.value, node_path: path };
    await backend.call('config.save', next);
    config.value = next;
    savedNodePath.value = nodePath.value;
    nodeRuntime.value = await backend.call<NodeRuntime>('node.status');
    message.value = path ? 'Node 路径已保存' : '已改为按 PATH 查找 Node';
    notify(message.value, 'success');
  } catch (reason) {
    message.value = messageOf(reason);
    notify(`保存失败：${messageOf(reason)}`, 'error');
  } finally {
    working.value = '';
  }
}

async function downloadRelease() {
  working.value = 'release';
  try {
    const result = await backend.call<{ staged?: boolean; version?: string; reason?: string; async?: boolean; task_id?: string }>('update.downloadRelease');
    if (result.staged) {
      stagedVersion.value = result.version ?? '';
      message.value = `更新已就绪：重启后生效`;
      notify(message.value, 'success');
      return;
    }
    if (result.reason) {
      message.value = result.reason;
      notify(result.reason, 'success');
      return;
    }
    // 下载在后台跑，进度经 task 事件回来（store 已经在监听）。
    downloadTaskId.value = result.task_id ?? '';
    message.value = `正在下载 ${result.version ?? ''}，完成后重启生效`;
  } catch (reason) {
    message.value = messageOf(reason);
    notify(`下载更新失败：${messageOf(reason)}`, 'error');
  } finally {
    working.value = '';
  }
}

async function checkVersion() {
  working.value = 'version';
  try {
    version.value = await backend.call<Version>('update.checkVersion');
    if (version.value.error) {
      message.value = `检查更新失败：${version.value.error}`;
      notify(message.value, 'error');
      return;
    }
    notify(version.value.has_update ? `发现新版本 ${version.value.latest ?? ''}` : '当前已是最新版本', version.value.has_update ? 'info' : 'success');
  } catch (reason) {
    message.value = messageOf(reason);
    notify(`检查更新失败：${messageOf(reason)}`, 'error');
  } finally {
    working.value = '';
  }
}

async function restart() {
  working.value = 'restart';
  try {
    await backend.call('update.restart');
    // 进程即将退出，这里不需要恢复按钮状态。
  } catch (reason) {
    working.value = '';
    message.value = messageOf(reason);
    notify(`重启失败：${messageOf(reason)}`, 'error');
  }
}

// 下载任务落定时把「待重启」状态接上，否则用户看不出下一步该做什么。
// 版本号从 update.status 回读，而不是取 version.latest：用户可能直接点了
// 下载、没先点过「检查更新」，那时 latest 还是空的。
watch(downloadTask, async (task) => {
  if (!task) return;
  if (task.phase === 'failed') {
    // 否则那行「正在下载…」会一直留着，和任务面板里的报错自相矛盾。
    message.value = `下载更新失败：${task.error ?? ''}`;
    return;
  }
  if (task.phase !== 'done') return;
  try {
    const status = await backend.call<UpdateStatus>('update.status');
    stagedVersion.value = status.version ?? version.value.latest ?? '';
  } catch {
    stagedVersion.value = version.value.latest ?? '';
  }
  message.value = '更新已下载完成，重启后生效';
  notify(message.value, 'success');
});

async function sync(method: 'update.syncWMPF' | 'update.syncSkills') {
  working.value = method;
  try {
    const result = await backend.call<{ updated?: number; error?: string }>(method);
    message.value = result.error ? `更新失败：${result.error}` : `已更新 ${result.updated ?? 0} 个文件`;
    notify(message.value, result.error ? 'error' : 'success');
  } catch (reason) {
    message.value = messageOf(reason);
    notify(`更新失败：${messageOf(reason)}`, 'error');
  } finally {
    working.value = '';
  }
}
onMounted(load);
</script>

<template>
  <section class="settings-view" aria-labelledby="settings-title">
    <PageHeader title="设置" title-id="settings-title" />

    <div v-if="loading && !Object.keys(paths).length" class="panel loading-panel" role="status">
      <strong>正在读取设置…</strong>
      <span>正在加载运行目录和组件配置。</span>
    </div>
    <div v-else class="stack">
      <div class="panel">
        <header class="panel-header">
          <h2>版本信息</h2>
          <div class="toolbar">
            <span v-if="stagedVersion" class="status-pill warn">待重启生效 {{ stagedVersion }}</span>
            <span v-else-if="version.has_update" class="status-pill warn">可更新至 {{ version.latest }}</span>
            <button class="secondary small" type="button" :disabled="loading || !!working" @click="load">{{ loading ? '加载中…' : '重新加载' }}</button>
          </div>
        </header>
        <div class="panel-body stack">
          <p class="status-line">当前版本：{{ version.current || appVersion }}</p>
          <p v-if="version.latest" class="status-line">最新版本：{{ version.latest }}<template v-if="updateSize">（{{ updateSize }}）</template></p>
          <p v-if="version.notes" class="status-line">{{ version.notes }}</p>
          <p v-if="version.error" class="status-line">检查更新失败：{{ version.error }}</p>
          <div class="actions">
            <button data-testid="check-version" type="button" :disabled="working === 'version'" @click="checkVersion">
              {{ working === 'version' ? '检查中…' : '检查更新' }}
            </button>
            <button data-testid="download-release" class="secondary" type="button" :disabled="downloading || !!working" @click="downloadRelease">
              {{ downloading ? '正在下载…' : '下载更新' }}
            </button>
            <button v-if="stagedVersion" data-testid="restart-update" type="button" :disabled="working === 'restart'" @click="restart">
              {{ working === 'restart' ? '正在重启…' : '立即重启' }}
            </button>
          </div>
        </div>
      </div>

      <TaskProgress :tasks="downloadTask ? [downloadTask] : []" />

      <div class="panel">
        <header class="panel-header"><h2>目录与资源</h2></header>
        <div class="panel-body stack">
          <ul v-if="Object.keys(paths).length" class="path-list">
            <li v-for="(path, name) in paths" :key="name">
              <div class="path-info">
                <strong>{{ pathLabels[name] ?? name }}</strong>
                <code>{{ path }}</code>
              </div>
              <div class="path-actions">
                <button :data-testid="`copy-${name}`" class="ghost small" type="button" @click="copyPath(name as keyof Paths)">复制</button>
                <button :data-testid="`open-${name}`" class="secondary small" type="button" @click="open(name as keyof Paths)">打开</button>
              </div>
            </li>
          </ul>
          <div v-else class="empty-state compact">
            <strong>暂无运行目录</strong>
            <span>设置服务没有返回可用路径，请点击「重新加载」重试。</span>
          </div>
          <div class="actions">
            <button data-testid="sync-wmpf" type="button" :disabled="!!working" @click="sync('update.syncWMPF')">
              {{ working === 'update.syncWMPF' ? '正在更新 Frida 配置…' : '更新 Frida 配置' }}
            </button>
            <button data-testid="sync-skills" class="secondary" type="button" :disabled="!!working" @click="sync('update.syncSkills')">
              {{ working === 'update.syncSkills' ? '正在更新技能文档…' : '更新技能文档' }}
            </button>
          </div>
        </div>
      </div>

      <div class="panel">
        <header class="panel-header">
          <h2>Node 运行时</h2>
          <span class="status-pill" :class="nodeRuntime.error ? 'warn' : 'off'">{{ nodeRuntime.error ? '不可用' : `v${nodeRuntime.version ?? ''}` }}</span>
        </header>
        <div class="panel-body stack">
          <p v-if="nodeRuntime.error" class="status-line">未找到可用的 Node：{{ nodeRuntime.error }}</p>
          <p v-else class="status-line">
            当前使用 <code>{{ nodeRuntime.path }}</code>（{{ nodeSourceLabels[nodeRuntime.source ?? ''] ?? nodeRuntime.source }}）
          </p>
          <p v-for="note in nodeRuntime.skipped ?? []" :key="note" class="status-line node-note">{{ note }}</p>
          <p class="status-line node-note">Core 由这个 Node 启动。WxTap 不自带运行时，需要 Node.js 22 或更高；未安装请从 nodejs.org 下载，或在此处指定路径。保存后重启引擎生效。</p>
          <label class="field" for="node-path">
            <span>node 可执行文件</span>
            <input id="node-path" data-testid="node-path" v-model="nodePath" placeholder="留空则使用 PATH 上的 node" @keydown.enter="saveNode">
          </label>
          <ul v-if="nodeCandidates.length" class="path-list">
            <li v-for="candidate in nodeCandidates" :key="candidate.path">
              <div class="path-info">
                <strong>v{{ candidate.version }}</strong>
                <code>{{ candidate.path }}</code>
              </div>
              <div class="path-actions">
                <button class="ghost small" type="button" @click="nodePath = candidate.path ?? ''">使用</button>
              </div>
            </li>
          </ul>
          <div class="actions">
            <button data-testid="detect-node" class="secondary" type="button" :disabled="!!working" @click="detectNode">{{ working === 'node-detect' ? '检测中…' : '自动检测' }}</button>
            <button data-testid="save-node" type="button" :disabled="working === 'node' || !nodeDirty" @click="saveNode">{{ working === 'node' ? '保存中…' : '保存' }}</button>
          </div>
        </div>
      </div>

      <div class="panel">
        <header class="panel-header">
          <h2>Electron 路径</h2>
          <span class="status-pill" :class="electronDirty ? 'warn' : 'off'">{{ electronDirty ? '有未保存更改' : '已保存' }}</span>
        </header>
        <div class="panel-body stack">
          <p v-if="electronRuntime.path" class="status-line" data-testid="electron-current">
            当前使用 <code>{{ electronRuntime.path }}</code>（{{ electronSourceLabels[electronRuntime.source ?? ''] ?? electronRuntime.source }}）
          </p>
          <p v-else-if="electronRuntime.error" class="status-line" data-testid="electron-missing">{{ electronRuntime.error }}</p>
          <p class="status-line">DevTools 页面用这个 Electron 打开独立窗口。留空则自动检测：PATH 上的 electron（含 npm 全局安装）与常见安装位置。</p>
          <label class="field" for="electron-path">
            <span>可执行文件</span>
            <input id="electron-path" data-testid="electron-path" v-model="electronPath" placeholder="留空则自动检测" @keydown.enter="saveElectron">
          </label>
          <ul v-if="electronCandidates.length" class="path-list">
            <li v-for="candidate in electronCandidates" :key="candidate.path">
              <div class="path-info">
                <strong>{{ electronSourceLabels[candidate.source ?? ''] ?? candidate.source }}</strong>
                <code>{{ candidate.path }}</code>
              </div>
              <div class="path-actions">
                <button class="ghost small" type="button" @click="electronPath = candidate.path ?? ''">使用</button>
              </div>
            </li>
          </ul>
          <div class="actions">
            <button data-testid="detect-electron" class="secondary" type="button" :disabled="!!working" @click="detectElectron">{{ working === 'electron-detect' ? '检测中…' : '自动检测' }}</button>
            <button data-testid="save-electron" type="button" :disabled="working === 'electron' || !electronDirty" @click="saveElectron">{{ working === 'electron' ? '保存中…' : '保存' }}</button>
          </div>
        </div>
      </div>

      <p v-if="message" class="status-line" role="status">{{ message }}</p>
    </div>
  </section>
</template>

<style scoped>
.node-note {
  color: var(--muted);
  font-size: .8rem;
}

.path-list {
  display: grid;
  gap: .5rem;
  list-style: none;
  margin: 0;
  padding: 0;
}

.path-list li {
  align-items: center;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  display: flex;
  gap: 1rem;
  justify-content: space-between;
  padding: .55rem .75rem;
}

.path-actions {
  display: flex;
  flex: none;
  gap: .35rem;
}

.path-info {
  display: grid;
  gap: .1rem;
  min-width: 0;
}

.path-info strong {
  font-size: .85rem;
}

.path-info code {
  color: var(--muted);
  font-family: var(--mono);
  font-size: .76rem;
  overflow-wrap: anywhere;
}
</style>

