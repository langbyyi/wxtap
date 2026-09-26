<script setup lang="ts">
import StateToggle from '../StateToggle.vue';

defineProps<{
  connected: boolean;
  loading: boolean;
  autoVisiting: boolean;
  autoProgress: number;
  autoCurrent: string;
  autoFailed: number;
  autoTotal: number;
}>();

const emit = defineEmits<{
  startAutoVisit: [];
  stopAutoVisit: [];
}>();
</script>

<template>
  <div class="panel">
    <header class="panel-header">
      <h2>自动访问</h2>
      <span v-if="autoVisiting" class="status-pill warn">遍历中</span>
      <span v-else-if="autoFailed" class="status-pill off">{{ autoFailed }} 页失败</span>
    </header>
    <div class="panel-body stack">
      <p class="status-line">按配置的页面列表逐页 reLaunch，每页停留 2 秒；单页失败不会中断，结束后会汇总失败数。</p>
      <div class="actions">
        <StateToggle
          test-id="auto-visit-toggle"
          :active="autoVisiting"
          :disabled="autoVisiting ? false : loading || !connected"
          start-label="开始遍历"
          stop-label="停止访问"
          @start="emit('startAutoVisit')"
          @stop="emit('stopAutoVisit')"
        />
      </div>
      <div v-if="autoVisiting" class="progress-block" role="status">
        <div class="progress-track" aria-hidden="true"><div class="progress-fill" :style="{ width: `${autoProgress}%` }" /></div>
        <p class="status-line">自动访问中 {{ autoProgress }}%（{{ autoTotal }} 页）{{ autoCurrent ? `- ${autoCurrent}` : '' }}</p>
      </div>
    </div>
  </div>
</template>

<style scoped>
.progress-block {
  display: grid;
  gap: .4rem;
}

.progress-track {
  background: var(--panel-2);
  border: 1px solid var(--border);
  border-radius: 99px;
  height: .5rem;
  overflow: hidden;
}

.progress-fill {
  background: var(--accent);
  height: 100%;
  transition: width .3s ease;
}
</style>
