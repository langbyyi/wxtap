<script setup lang="ts">
defineProps<{ logs: string[] }>();
const emit = defineEmits<{ copy: []; clear: [] }>();
</script>

<template>
  <div v-if="logs.length" class="panel">
    <header class="panel-header">
      <h2>解包日志 <span class="subnav-count">{{ logs.length }} 条</span></h2>
      <div class="toolbar">
        <button data-testid="copy-extract-logs" class="ghost small" type="button" @click="emit('copy')">复制</button>
        <button data-testid="clear-extract-logs" class="ghost small" type="button" @click="emit('clear')">清空</button>
      </div>
    </header>
    <p class="log-strip" role="log">{{ logs.join('\n') }}</p>
  </div>
</template>

<style scoped>
.log-strip {
  background: var(--panel-2);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--muted);
  font-family: var(--mono);
  font-size: .78rem;
  margin: 0;
  max-height: 10rem;
  overflow: auto;
  padding: .7rem .85rem;
  white-space: pre-wrap;
}
</style>
