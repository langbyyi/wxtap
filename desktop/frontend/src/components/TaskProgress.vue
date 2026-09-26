<script setup lang="ts">
import type { TaskState } from '../stores/engine';

defineProps<{ tasks: TaskState[] }>();
</script>

<template>
  <section v-if="tasks.length" class="task-progress panel" aria-label="后台任务进度">
    <header class="panel-header"><h2>后台任务</h2><span>{{ tasks.length }} 项</span></header>
    <div v-for="task in tasks" :key="task.id" class="task-row" :data-testid="`task-${task.id}`">
      <div class="task-heading"><strong>{{ task.message || task.kind }}</strong><span>{{ task.phase }}</span></div>
      <progress v-if="task.total" :value="task.current || 0" :max="task.total" />
      <small v-if="task.error" class="error">{{ task.error }}</small>
    </div>
  </section>
</template>

<style scoped>
.task-progress { margin-top: 1rem; }
.task-row { display: grid; gap: .4rem; padding: .75rem 1rem; border-top: 1px solid var(--border); }
.task-heading { display: flex; justify-content: space-between; gap: 1rem; }
.task-heading span { color: var(--faint); font-size: .78rem; }
.task-row progress { width: 100%; }
</style>
