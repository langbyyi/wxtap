<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue';

const props = withDefaults(defineProps<{
  open: boolean;
  title: string;
  message: string;
  confirmText?: string;
  cancelText?: string;
  tone?: 'primary' | 'danger';
  testId?: string;
}>(), {
  confirmText: '确认',
  cancelText: '取消',
  tone: 'primary',
  testId: 'confirm-clear',
});

const emit = defineEmits<{ confirm: []; cancel: [] }>();
const confirmButton = ref<HTMLButtonElement | null>(null);
let trigger: HTMLElement | null = null;

function handleKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') emit('cancel');
}

watch(() => props.open, async (open) => {
  if (open) {
    trigger = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    window.addEventListener('keydown', handleKeydown);
    await nextTick();
    confirmButton.value?.focus();
    return;
  }

  window.removeEventListener('keydown', handleKeydown);
  if (trigger?.isConnected) trigger.focus();
  trigger = null;
}, { immediate: true });

onBeforeUnmount(() => window.removeEventListener('keydown', handleKeydown));
</script>

<template>
  <div v-if="open" class="modal-backdrop" @click.self="emit('cancel')">
    <div class="modal-card" role="dialog" aria-modal="true" :aria-label="title">
      <h2 class="modal-title">{{ title }}</h2>
      <p>{{ message }}</p>
      <div class="modal-actions">
        <button type="button" class="secondary" @click="emit('cancel')">
          {{ cancelText }}
        </button>
        <button
          ref="confirmButton"
          :data-testid="testId"
          type="button"
          :class="tone === 'danger' ? 'danger' : ''"
          @click="emit('confirm')"
        >
          {{ confirmText }}
        </button>
      </div>
    </div>
  </div>
</template>
