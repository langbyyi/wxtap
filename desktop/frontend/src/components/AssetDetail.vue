<script setup lang="ts">
import { computed } from 'vue';
import { useRouter } from 'vue-router';
import { copyText } from '../utils/notify';

// 资产详情块：完整 URL、标签、来源列表、流量记录跳转。明细表的行展开与
// 结构树/页面分组的叶子详情共用，形状与 AssetsView 的 AssetItem 子集一致。
type AssetSource = { type: string; ref: string };
type AssetLike = { url: string; host: string; path: string; tags?: string[]; sources: AssetSource[]; trafficSeen?: boolean };

const props = defineProps<{ asset: AssetLike }>();
const router = useRouter();

// 只对流量里出现过的端点提供跳转：纯代码来源的地址跳过去多半是空结果。
const hasTraffic = computed(
  () => props.asset.trafficSeen === true || props.asset.sources.some((source) => source.type === 'traffic'),
);

// host+path 正好是流量记录 URL 的公共子串，作为关键词预填历史记录页。
function openInTraffic() {
  void router.push({ path: '/traffic', query: { q: `${props.asset.host}${props.asset.path}` } });
}

function copyUrl() {
  void copyText(props.asset.url, 'URL');
}
</script>

<template>
  <div class="asset-detail">
    <p class="detail-url"><strong>完整 URL：</strong><code>{{ asset.url }}</code></p>
    <p v-if="asset.tags?.length" class="detail-tags">
      <strong>标签：</strong>
      <span v-for="tag in asset.tags" :key="tag" class="record-badge">{{ tag }}</span>
    </p>
    <div class="detail-sources">
      <strong>来源（{{ asset.sources.length }}）</strong>
      <ul>
        <li v-for="(source, index) in asset.sources" :key="index" class="mono">
          {{ source.type }} → {{ source.ref }}
        </li>
        <li v-if="!asset.sources.length" class="status-line">无来源记录</li>
      </ul>
    </div>
    <div class="detail-actions">
      <button class="ghost small" data-testid="asset-copy-url" type="button" @click="copyUrl">复制 URL</button>
      <button v-if="hasTraffic" class="ghost small" data-testid="asset-open-traffic" type="button" @click="openInTraffic">在流量记录中查看</button>
    </div>
  </div>
</template>

<style scoped>
.asset-detail {
  display: grid;
  gap: .45rem;
  font-size: .8rem;
  padding: .3rem 0;
}

.asset-detail p {
  margin: 0;
  overflow-wrap: anywhere;
}

.detail-tags {
  align-items: center;
  display: flex;
  flex-wrap: wrap;
  gap: .35rem;
}

.detail-sources ul {
  margin: .25rem 0 0;
  overflow-wrap: anywhere;
  padding-left: 1.1rem;
}

.detail-actions {
  display: flex;
  gap: .4rem;
}
</style>
