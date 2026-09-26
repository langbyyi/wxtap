<script setup lang="ts">
import { computed } from 'vue';
import { copyText } from '../../utils/notify';

type BlockedRedirect = { type?: string; url?: string; time?: string };

// 勾选状态由 model 驱动：后端拒绝开启时父组件会把值改回去，DOM 跟着回弹。
// 若改成「受控 input + 只读当前值」，回退到同一个值不会触发重绘，勾选框会
// 停在一个后端并未接受的状态上。
const enabled = defineModel<boolean>({ required: true });

const props = defineProps<{
  connected: boolean;
  redirects: BlockedRedirect[];
  loading: boolean;
}>();

const emit = defineEmits<{
  refresh: [];
  change: [enabled: boolean];
}>();

const TYPE_FALLBACK = '未知';
const described = computed(() => props.redirects.map((item) => ({
  type: item.type || TYPE_FALLBACK,
  url: item.url ?? '',
  time: item.time ?? '',
})));

function copyUrl(url: string) {
  void copyText(url, '拦截地址');
}
</script>

<template>
  <div class="panel guard-panel">
    <header class="panel-header">
      <h2>防跳转</h2>
      <span class="status-pill" :class="enabled ? 'ok' : 'off'" data-testid="guard-state">{{ enabled ? '拦截中' : '未开启' }}</span>
      <button data-testid="refresh-blocked" class="ghost small" type="button" :disabled="loading || !connected" @click="emit('refresh')">{{ loading ? '刷新中…' : '刷新记录' }}</button>
    </header>
    <div class="panel-body stack">
      <label class="check-inline" for="redirect-guard">
        <!-- 回调里读事件目标而不是 model：v-model 与 @change 同时存在时，两个
             处理器谁先跑并不由这里决定，读 model 可能拿到上一轮的值。 -->
        <input id="redirect-guard" data-testid="redirect-guard" v-model="enabled" type="checkbox" :disabled="!connected" @change="emit('change', ($event.target as HTMLInputElement).checked)"> 拦截页面重定向（redirectTo / reLaunch / navigateTo）
      </label>
      <p class="status-line">拦截器仅在小程序页面内生效：页面重新加载后拦截自动失效，此处状态以后端回读结果为准。</p>

      <h3 class="blocked-title">拦截记录 <span class="subnav-count">{{ described.length }}</span></h3>
      <ul v-if="described.length" class="blocked-list" data-testid="blocked-list">
        <li v-for="(item, index) in described" :key="index">
          <span class="blocked-type">{{ item.type }}</span>
          <code>{{ item.url || '(空地址)' }}</code>
          <span v-if="item.time" class="blocked-time">{{ item.time }}</span>
          <button v-if="item.url" class="ghost small" type="button" @click="copyUrl(item.url)">复制</button>
        </li>
      </ul>
      <div v-else class="empty-state compact">
        <strong>{{ loading ? '正在读取拦截记录…' : '暂无拦截记录' }}</strong>
        <span v-if="!loading">开启防跳转后，被阻止的重定向会显示在这里。</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.guard-panel :deep(.panel-header) {
  flex-wrap: wrap;
}

.blocked-title {
  font-size: .82rem;
  margin: .2rem 0 0;
}

.blocked-list {
  display: grid;
  gap: .4rem;
  list-style: none;
  margin: 0;
  padding: 0;
}

.blocked-list li {
  align-items: center;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  display: flex;
  flex-wrap: wrap;
  gap: .5rem;
  padding: .45rem .6rem;
}

.blocked-list code {
  font-family: var(--mono);
  font-size: .75rem;
  overflow-wrap: anywhere;
}

.blocked-type {
  background: var(--warning-soft);
  border-radius: 4px;
  color: var(--warning);
  font-size: .7rem;
  font-weight: 700;
  padding: .05rem .35rem;
}

.blocked-time {
  color: var(--muted);
  font-size: .74rem;
  font-variant-numeric: tabular-nums;
}

.empty-state.compact {
  padding: .8rem;
}
</style>
