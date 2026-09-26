<script setup lang="ts">
defineProps<{ message: string; retryable?: boolean }>();
</script>
<template><div class="error-state" role="alert"><strong>{{ message }}</strong><button v-if="retryable" class="secondary small" type="button" @click="$emit('retry')">重试</button></div></template>
