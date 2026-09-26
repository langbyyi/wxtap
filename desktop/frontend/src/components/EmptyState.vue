<script setup lang="ts">
defineProps<{ title: string; description?: string }>();
</script>
<template><div class="empty-state"><strong>{{ title }}</strong><span v-if="description">{{ description }}</span><slot /></div></template>
