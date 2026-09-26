<script setup lang="ts">
// 数据量大的列表共用一个密度开关：捕获类列表的行是同一套结构（.record-list 里的
// .record-title / .record-badge / .record-meta / .record-url），压缩规则写在 base.css 一处，
// 这里只负责那个复选框的样子与文案。
defineProps<{
  modelValue: boolean;
  /** 各页自己的 data-testid（默认 list-density） */
  testId?: string;
}>();

const emit = defineEmits<{ 'update:modelValue': [value: boolean] }>();
</script>

<template>
  <label class="check-inline" title="压缩行高与字号，一屏可显示更多行">
    <input
      type="checkbox"
      :data-testid="testId ?? 'list-density'"
      :checked="modelValue"
      @change="emit('update:modelValue', ($event.target as HTMLInputElement).checked)"
    > 紧凑
  </label>
</template>
