<script setup lang="ts">
import { dismissNotice, useNotices } from '../utils/notify';

const { notices } = useNotices();
</script>

<template>
  <Teleport to="body">
    <div class="notice-stack" aria-live="polite" aria-relevant="additions">
      <TransitionGroup name="notice">
        <div
          v-for="notice in notices"
          :key="notice.id"
          class="notice-card"
          :class="`notice-${notice.tone}`"
          :role="notice.tone === 'error' ? 'alert' : 'status'"
        >
          <span class="notice-mark" aria-hidden="true" />
          <span class="notice-message">{{ notice.message }}</span>
          <button class="notice-close" type="button" aria-label="关闭提示" @click="dismissNotice(notice.id)">×</button>
        </div>
      </TransitionGroup>
    </div>
  </Teleport>
</template>

<style scoped>
.notice-stack {
  bottom: 1.25rem;
  display: grid;
  gap: .55rem;
  max-width: min(24rem, calc(100vw - 2rem));
  pointer-events: none;
  position: fixed;
  right: 1.25rem;
  z-index: 80;
}

.notice-card {
  align-items: center;
  animation: notice-in .16s ease-out;
  backdrop-filter: blur(12px);
  background: color-mix(in srgb, var(--panel) 94%, transparent);
  border: 1px solid var(--border-strong);
  border-radius: var(--radius-sm);
  box-shadow: 0 12px 32px rgba(1, 5, 15, .28);
  display: grid;
  gap: .55rem;
  grid-template-columns: auto minmax(0, 1fr) auto;
  padding: .62rem .65rem .62rem .75rem;
  pointer-events: auto;
}

.notice-mark {
  background: var(--accent);
  border-radius: 50%;
  box-shadow: 0 0 0 3px var(--accent-soft);
  height: .45rem;
  width: .45rem;
}

.notice-success .notice-mark {
  background: var(--success);
  box-shadow: 0 0 0 3px var(--success-soft);
}

.notice-error {
  border-color: color-mix(in srgb, var(--danger) 60%, var(--border-strong));
}

.notice-error .notice-mark {
  background: var(--danger);
  box-shadow: 0 0 0 3px var(--danger-soft);
}

.notice-message {
  font-size: .84rem;
  line-height: 1.4;
  min-width: 0;
  overflow-wrap: anywhere;
}

.notice-close {
  background: transparent;
  border: 0;
  color: var(--faint);
  font-size: 1rem;
  height: 1.5rem;
  padding: 0;
  width: 1.5rem;
}

.notice-close:hover:not(:disabled) {
  background: var(--panel-2);
  color: var(--text);
}

.notice-enter-active,
.notice-leave-active {
  transition: opacity .14s ease, translate .14s ease;
}

.notice-enter-from,
.notice-leave-to {
  opacity: 0;
  translate: 0 .4rem;
}

@keyframes notice-in {
  from { opacity: 0; translate: 0 .4rem; }
}

@media (max-width: 760px) {
  .notice-stack {
    bottom: 1rem;
    left: 1rem;
    right: 1rem;
  }
}
</style>
