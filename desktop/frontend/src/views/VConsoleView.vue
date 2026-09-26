<script setup lang="ts">
import { computed, ref } from 'vue';
import { backend } from '../api/bridge';
import ConfirmDialog from '../components/ConfirmDialog.vue';
import PageHeader from '../components/PageHeader.vue';
import StateToggle from '../components/StateToggle.vue';
import { useEngineStore } from '../stores/engine';
import { messageOf } from '../utils/format';
import { notify } from '../utils/notify';

const store = useEngineStore();
const connected = computed(() => store.status.miniapp);
const confirming = ref(false);
const docsError = ref('');
const miniappLabel = computed(() => store.status.appInfo?.name?.trim() || store.status.appInfo?.appid?.trim() || '');
// vConsole 是否打开只存在于小程序页面里，后端没有读取接口：这里显示的是最近
// 一次操作的结果，未操作过就如实显示「状态未知」。
const stateLabel = computed(() => (store.vconsoleEnabled === null ? '状态未知' : store.vconsoleEnabled ? '已开启' : '已关闭'));
const stateTone = computed(() => (store.vconsoleEnabled === null ? 'off' : store.vconsoleEnabled ? 'ok' : 'warn'));

function confirmEnable() {
  confirming.value = false;
  void store.setVConsole(true);
}

function disable() {
  void store.setVConsole(false);
}

function docs() {
  docsError.value = '';
  backend.call('shell.openUrl', { url: 'https://developers.weixin.qq.com/miniprogram/dev/api/base/debug/wx.setEnableDebug.html' })
    .catch((error: unknown) => {
      docsError.value = messageOf(error);
      notify(`打开文档失败：${messageOf(error)}`, 'error');
    });
}
</script>

<template>
  <section class="vconsole-view" aria-labelledby="vconsole-title">
    <PageHeader title="vConsole" title-id="vconsole-title" />

    <div class="stack">
      <div class="panel">
        <header class="panel-header">
          <h2>操作</h2>
          <span class="status-pill" :class="connected ? 'ok' : 'off'">{{ connected ? '小程序已连接' : '未连接小程序' }}</span>
          <span v-if="miniappLabel" class="status-pill">{{ miniappLabel }}</span>
          <span class="status-pill" :class="stateTone" data-testid="vconsole-state">调试面板：{{ stateLabel }}</span>
        </header>
        <div class="panel-body stack">
          <div class="actions">
            <!-- 开启方向要过确认框（非正规调试有封号风险），关闭方向直接执行 —— 由 StateToggle
                 发 start 事件、页面决定弹不弹确认，组件本身不认识确认框。 -->
            <StateToggle
              test-id="vconsole-toggle"
              :active="store.vconsoleEnabled === true"
              :pending="store.vconsolePhase === 'enable' ? 'start' : store.vconsolePhase === 'disable' ? 'stop' : ''"
              :disabled="!connected"
              start-label="开启调试"
              stop-label="关闭调试"
              starting-label="开启中…"
              stopping-label="关闭中…"
              start-title="非正规开启小程序调试有封号风险，建议使用测试账号"
              @start="confirming = true"
              @stop="disable"
            />
            <button type="button" class="ghost small" @click="docs">微信官方文档</button>
          </div>
          <p v-if="!connected" class="error">需要先连接小程序</p>
          <p v-else-if="store.vconsoleEnabled === null" class="status-line">状态未知：执行一次开启或关闭后即可确认小程序内的实际状态；重新加载小程序也会回到未知。</p>
          <p v-if="store.vconsoleError" class="error" role="alert">操作失败：{{ store.vconsoleError }}</p>
          <p v-if="docsError" class="error" role="alert">打开文档失败：{{ docsError }}</p>
          <p class="status-line">
            开启后，调试面板出现在<strong>微信小程序窗口里</strong>：可以在小程序内执行 JS、查看网络请求、调用云函数和查看 Storage。
            该面板位于小程序内、不经过本程序的连接，因此<strong>无法读取其内容</strong> —— 这一页只提供这个开关。
            点击后的结果只有两种：开启成功，或者小程序拒绝这次调用（下方会显示其给出的失败原因）—— 后者说明当前 WMPF 版本不支持或不允许，请改用 DevTools 页。
          </p>
          <p class="status-line">
            如需在此查看日志/请求，请走已经接通的几条路径：<RouterLink to="/console" data-testid="link-console">Console 日志</RouterLink>（小程序与注入脚本的 console 输出）、
            <RouterLink to="/traffic" data-testid="link-traffic">历史记录</RouterLink> / <RouterLink to="/wxapi" data-testid="link-wxapi">WxAPI</RouterLink> / <RouterLink to="/cloud" data-testid="link-cloud">云函数</RouterLink>（请求与调用）、
            <RouterLink to="/devtools" data-testid="link-devtools">DevTools</RouterLink>（断点与全功能调试）。
          </p>
          <p class="status-line">
            如仅需在小程序页面中执行自己的 JS，无需开启 vConsole：<RouterLink to="/hook" data-testid="link-hook">注入脚本</RouterLink> 页可以直接把脚本注入当前页面上下文。
          </p>
        </div>
      </div>
    </div>

    <ConfirmDialog
      :open="confirming"
      title="确认开启调试"
      message="确认开启调试？非正规开启有封号风险，建议仅在测试账号上使用。"
      confirm-text="确认开启"
      test-id="confirm-vconsole"
      @confirm="confirmEnable"
      @cancel="confirming = false"
    />
  </section>
</template>
