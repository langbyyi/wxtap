<script setup lang="ts">
// 页头只负责无障碍标题（section[aria-labelledby] 的锚点）与工具条插槽。
// 页面标题由顶部分组子导航给出，这里不再重复渲染可见标题或说明文字。
defineProps<{ title: string; titleId?: string }>();
</script>
<template>
  <header class="page-chrome">
    <h1 v-if="titleId" :id="titleId" class="sr-only">{{ title }}</h1>
    <div v-if="$slots.actions" class="page-toolbar"><slot name="actions" /></div>
  </header>
</template>
