<script setup lang="ts">
import { computed } from 'vue';
import { splitHighlight } from '../utils/text-highlight';

// 关键词命中高亮：把文本按命中分段渲染，命中片段包 <mark>。纯文本插值，无注入面。
const props = defineProps<{ text: string; term: string }>();

const segments = computed(() => splitHighlight(props.text, props.term));
</script>

<template>
  <template v-for="(seg, index) in segments" :key="index"><mark v-if="seg.hit" class="hit-term">{{ seg.text }}</mark><template v-else>{{ seg.text }}</template></template>
</template>

<style scoped>
/* 命中片段：浅紫底即可读，不加纵向 padding 以免撑高行 */
.hit-term {
  background: var(--accent-soft);
  border-radius: 2px;
  color: var(--accent-strong);
  padding: 0 .05em;
}
</style>
