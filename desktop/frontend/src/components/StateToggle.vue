<script setup lang="ts">
import { computed } from 'vue';

// 一个开关，一个按钮：同一件事的两种状态不再并排成「开启 X」+「停止 X」。两个按钮里总有
// 一个是死按钮，用户得先读两个再决定点哪个；这里标签写的是**按下之后会发生什么**，看到什么
// 就是什么。
//
// 组件是纯展示的：它不认识后端，也不需要知道被开关的是捕获、引擎还是服务。禁用条件由调用方
// 按当前方向算好传进来（未连接、端口非法这类判断各页都不一样），在飞方向用 pending 表达。
const props = withDefaults(defineProps<{
  /** 当前是否处于开启态（决定标签、样式与按下后发哪个事件） */
  active: boolean;
  /** 在飞方向：'' 空闲；'start' / 'stop' 各自替换成「…中…」并禁用按钮 */
  pending?: '' | 'start' | 'stop';
  /** 当前方向的禁用条件（调用方算：未连接、端口非法…）。在飞时一律禁用，不必自己叠加 */
  disabled?: boolean;
  startLabel: string;
  stopLabel: string;
  startingLabel?: string;
  stoppingLabel?: string;
  testId: string;
  /** 悬停说明（如 vConsole 的封号风险提示） */
  startTitle?: string;
  stopTitle?: string;
}>(), {
  pending: '',
  disabled: false,
  startingLabel: '启动中…',
  stoppingLabel: '停止中…',
  startTitle: '',
  stopTitle: '',
});

const emit = defineEmits<{ start: []; stop: [] }>();

const label = computed(() => {
  if (props.pending === 'start') return props.startingLabel;
  if (props.pending === 'stop') return props.stoppingLabel;
  return props.active ? props.stopLabel : props.startLabel;
});

// 分开写而不是 emit(active ? 'stop' : 'start')：带类型的 emit 不接受联合字面量。
function press() {
  if (props.active) emit('stop');
  else emit('start');
}
</script>

<template>
  <button
    :data-testid="testId"
    type="button"
    :class="{ secondary: active }"
    :disabled="disabled || pending !== ''"
    :title="active ? stopTitle : startTitle"
    :aria-pressed="active"
    @click="press"
  >
    {{ label }}
  </button>
</template>
