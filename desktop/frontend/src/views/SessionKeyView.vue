<script setup lang="ts">
import { computed, ref } from 'vue';
import { backend } from '../api/bridge';
import { WxCryptoError, wxDecrypt, wxEncrypt } from '../utils/wx-crypto';
import { messageOf, prettyJson } from '../utils/format';
import { copyText, notify } from '../utils/notify';
import PageHeader from '../components/PageHeader.vue';

type Mode = 'decrypt' | 'encrypt';
type Finding = { appid: string; source: string; value: string; kind: string; context: string; masked: string; foundAt: string };

const mode = ref<Mode>('decrypt');
// 加解密模式挂在输入面板的头上：它决定的是这一整块输入/结果，不再自己占页面顶上一行
const cryptoModes: Array<{ id: Mode; label: string }> = [
  { id: 'decrypt', label: '解密' },
  { id: 'encrypt', label: '加密' },
];
const sessionKey = ref('');
const iv = ref('');
const encryptedData = ref('');
const plaintext = ref('');
const result = ref('');
const error = ref('');
const busy = ref(false);

// Capture scan state: session_key / iv / encryptedData ride in packets, so the
// tool reads what the wxapi and cloud hook drainers already stored.
const trafficFindings = ref<Finding[]>([]);
const trafficScanned = ref(false);
const trafficScannedCount = ref(0);
const scanningTraffic = ref(false);

const payloadValue = computed({
  get: () => (mode.value === 'decrypt' ? encryptedData.value : plaintext.value),
  set: (val: string) => { if (mode.value === 'decrypt') encryptedData.value = val; else plaintext.value = val; },
});

const placeholder = computed(() =>
  mode.value === 'decrypt'
    ? '粘贴 base64 编码的 encryptedData'
    : '输入待加密的 JSON 明文',
);

async function scanTraffic() {
  if (scanningTraffic.value) return;
  scanningTraffic.value = true;
  error.value = '';
  try {
    const res = await backend.call<{ findings: Finding[]; scanned: number }>('sessionkey.scanTraffic', {});
    trafficFindings.value = res.findings ?? [];
    trafficScannedCount.value = res.scanned ?? 0;
    trafficScanned.value = true;
    notify(`抓包提取完成：${trafficFindings.value.length} 项发现`, trafficFindings.value.length ? 'success' : 'info');
  } catch (reason) {
    error.value = messageOf(reason);
  } finally {
    scanningTraffic.value = false;
  }
}

// One filler for every source: the kind decides which input receives the value.
function applyFinding(f: Finding) {
  if (f.kind === 'iv') iv.value = f.value;
  else if (f.kind === 'encryptedData') encryptedData.value = f.value;
  else sessionKey.value = f.value;
  result.value = '';
  error.value = '';
  notify(`已填入 ${f.kind}（${f.source}）`, 'success');
}

function swapMode(next: Mode) {
  if (mode.value === next) return;
  mode.value = next;
  error.value = '';
  result.value = '';
  if (mode.value === 'encrypt') {
    plaintext.value = encryptedData.value;
    encryptedData.value = '';
  } else {
    encryptedData.value = plaintext.value;
    plaintext.value = '';
  }
}

async function run() {
  error.value = '';
  result.value = '';
  busy.value = true;
  try {
    if (mode.value === 'decrypt') {
      const raw = await wxDecrypt(encryptedData.value, iv.value, sessionKey.value);
      try { result.value = prettyJson(JSON.parse(raw)); } catch { result.value = raw; }
    } else {
      JSON.parse(plaintext.value);
      result.value = await wxEncrypt(plaintext.value, iv.value, sessionKey.value);
    }
    notify(mode.value === 'decrypt' ? '解密成功' : '加密成功', 'success');
  } catch (reason) {
    error.value = reason instanceof WxCryptoError ? reason.message : reason instanceof Error ? reason.message : String(reason);
  } finally {
    busy.value = false;
  }
}

async function copyResult() {
  if (!result.value) return;
  await copyText(result.value, mode.value === 'decrypt' ? '解密结果' : '加密结果');
}

// 解密出来的是 JSON，改几个字段再加密回去是这条链路最常见的下一步：把结果直接
// 送进加密输入，省掉手工复制粘贴（切模式本身不会搬运结果，只会搬运输入框里的文本）。
function useResultAsPlaintext() {
  if (!result.value) return;
  plaintext.value = result.value;
  encryptedData.value = '';
  result.value = '';
  mode.value = 'encrypt';
  error.value = '';
  notify('已填入加密输入', 'success');
}

function loadSample() {
  sessionKey.value = 'tiihtNczf5v6AKRyjwEUhQ==';
  iv.value = 'Xsdni3/wBgoPUlvmCMljyA==';
  if (mode.value === 'decrypt') {
    encryptedData.value = 'kOM74Dk6JNXG6Dc7dwyrpmdalmoEyVhCqNGPmQf2n1yQL/z6bwHQ81eUtWBppkjvA4ZfXyqUgmqX+uyT5InF1w6TPgwfcgEOy/vDMCk3koVTzcZVhfbnHCRu7EcWby30dgZUdRyqTTnf/3X3H5/esmHsFvKdwblZajwJz/TqJg4=';
    plaintext.value = '';
  } else {
    plaintext.value = '{"phoneNumber":"13888888888","purePhoneNumber":"13888888888","countryCode":"86"}';
    encryptedData.value = '';
  }
  result.value = '';
  error.value = '';
}

// Combined-JSON recognition for code2Session payloads and captured requests.
const combinedInput = ref('');

// Decompiled mini-program scan state (extract output root).
const decompiledFindings = ref<Finding[]>([]);
const decompiledScanned = ref(false);
const scanningDecompiled = ref(false);

const sessionKeyKeys = ['session_key', 'sessionKey', 'sessionkey', 'session-key'];
const ivKeys = ['iv', 'IV', 'ivBase64'];
const encryptedKeys = ['encryptedData', 'encrypted_data'];

function firstString(source: Record<string, unknown>, keys: string[]): string {
  for (const key of keys) {
    const value = source[key];
    if (typeof value === 'string' && value.trim()) return value.trim();
  }
  return '';
}

function urlDecode() {
  const raw = combinedInput.value.trim();
  if (!raw) { notify('请先粘贴组合 JSON'); return; }
  try {
    combinedInput.value = decodeURIComponent(raw);
    notify('已执行 URL 解码', 'success');
  } catch {
    notify('URL 解码失败：内容不是有效的 URL 编码串', 'error');
  }
}

function recognizeFill() {
  error.value = '';
  const raw = combinedInput.value.trim();
  if (!raw) { error.value = '请先粘贴组合 JSON（session_key / iv / encryptedData）'; return; }
  let text = raw;
  if (text.includes('%')) {
    try { text = decodeURIComponent(text); } catch { /* keep raw text */ }
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    error.value = '无法解析组合 JSON，请检查内容或先点击 URL 解码';
    return;
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    error.value = '组合 JSON 需要是对象（含 session_key / iv / encryptedData）';
    return;
  }
  const source = parsed as Record<string, unknown>;
  const key = firstString(source, sessionKeyKeys);
  const vector = firstString(source, ivKeys);
  const cipher = firstString(source, encryptedKeys);
  if (!key && !vector && !cipher) {
    error.value = '未在 JSON 中找到 session_key / iv / encryptedData 字段';
    return;
  }
  if (key) sessionKey.value = key;
  if (vector) iv.value = vector;
  if (cipher) encryptedData.value = cipher;
  result.value = '';
  notify('已识别并填充字段', 'success');
}

async function scanDecompiled() {
  if (scanningDecompiled.value) return;
  scanningDecompiled.value = true;
  error.value = '';
  try {
    const res = await backend.call<{ findings: Finding[] }>('sessionkey.scanDecompiled', {});
    decompiledFindings.value = res.findings ?? [];
    decompiledScanned.value = true;
    notify(`反编译产物扫描完成：${decompiledFindings.value.length} 项`, decompiledFindings.value.length ? 'success' : 'info');
  } catch (reason) {
    error.value = messageOf(reason);
  } finally {
    scanningDecompiled.value = false;
  }
}

async function copyCiphertext() {
  if (!encryptedData.value.trim()) { notify('暂无可复制的密文'); return; }
  await copyText(encryptedData.value, '密文');
}

async function copyPlaintext() {
  const value = plaintext.value.trim();
  if (!value) { notify('暂无可复制的明文'); return; }
  await copyText(value, '明文');
}

function clearAll() {
  sessionKey.value = '';
  iv.value = '';
  encryptedData.value = '';
  plaintext.value = '';
  result.value = '';
  error.value = '';
  combinedInput.value = '';
  trafficFindings.value = [];
  trafficScanned.value = false;
  trafficScannedCount.value = 0;
  decompiledFindings.value = [];
  decompiledScanned.value = false;
  notify('已清空输入、结果与扫描列表', 'success');
}

</script>

<template>
  <section class="sessionkey-view" aria-labelledby="sessionkey-title">
    <PageHeader title="SessionKey" title-id="sessionkey-title">
    </PageHeader>

    <p v-if="error" class="error error-line" role="alert" data-testid="sessionkey-error">{{ error }}</p>

    <div class="work-layout">
      <aside class="source-rail" aria-label="数据来源与辅助操作">
        <div class="panel rail-panel">
          <header class="panel-header">
            <h2>抓包记录 <span v-if="trafficFindings.length" class="subnav-count">{{ trafficFindings.length }}</span></h2>
            <button type="button" class="secondary small" data-testid="scan-traffic" :disabled="scanningTraffic" @click="scanTraffic">{{ scanningTraffic ? '提取中…' : '从抓包记录提取' }}</button>
          </header>
          <div v-if="trafficFindings.length" class="findings-table">
            <div v-for="(f, i) in trafficFindings" :key="i" :data-testid="`traffic-finding-${i}`" class="finding-row" @click="applyFinding(f)">
              <div class="finding-main">
                <span class="finding-mask">{{ f.value }}</span>
                <span class="finding-kind">{{ f.kind }}</span>
              </div>
              <span v-if="f.appid" class="finding-src">{{ f.appid }}</span>
              <span class="finding-src" :title="f.source">{{ f.source }}</span>
            </div>
          </div>
          <p v-else-if="trafficScanned" class="hint-text">已扫描 {{ trafficScannedCount }} 条报文，未发现目标字段。</p>
        </div>

        <div class="panel rail-panel">
          <header class="panel-header">
            <h2>组合 JSON</h2>
            <div class="panel-actions">
              <!-- 示例与清空都是对下面这块输入的操作：放在同一个面板头里。 -->
              <button type="button" class="secondary small" data-testid="load-sample" @click="loadSample">填入示例</button>
              <button type="button" class="ghost danger small" data-testid="clear-sessionkey" @click="clearAll">清空</button>
              <button type="button" class="ghost small" data-testid="url-decode" @click="urlDecode">URL 解码</button>
              <button type="button" class="secondary small" data-testid="recognize-fill" @click="recognizeFill">识别填充</button>
            </div>
          </header>
          <div class="input-stack compact-stack">
            <textarea id="combined-input" v-model="combinedInput" data-testid="combined-input" rows="4" placeholder="粘贴 code2Session 返回值或抓包得到的组合 JSON" spellcheck="false"></textarea>
          </div>
        </div>

        <div class="panel rail-panel">
          <header class="panel-header">
            <h2>反编译产物 <span v-if="decompiledFindings.length" class="subnav-count">{{ decompiledFindings.length }}</span></h2>
            <button type="button" class="secondary small" data-testid="scan-decompiled" :disabled="scanningDecompiled" @click="scanDecompiled">{{ scanningDecompiled ? '扫描中…' : '扫描' }}</button>
          </header>
          <div v-if="decompiledFindings.length" class="findings-table">
            <div v-for="(f, i) in decompiledFindings" :key="i" :data-testid="`decompiled-finding-${i}`" class="finding-row" @click="applyFinding(f)">
              <div class="finding-main">
                <span class="finding-mask">{{ f.value }}</span>
                <span class="finding-kind">{{ f.kind }}</span>
              </div>
              <span v-if="f.appid" class="finding-src">{{ f.appid }}</span>
              <span class="finding-src" :title="[f.source, f.foundAt].filter(Boolean).join(' · ')">{{ f.source }}</span>
            </div>
          </div>
          <p v-else-if="decompiledScanned" class="hint-text">未在反编译产物中发现可用字段。</p>
        </div>
      </aside>

      <main class="workbench" aria-label="SessionKey 加解密">
        <div class="tool-grid">
          <div class="panel inputs-panel">
            <header class="panel-header">
              <h2>输入 <span class="panel-note">{{ mode === 'decrypt' ? 'Base64 密文' : '明文 JSON' }}</span></h2>
              <div class="panel-actions">
                <div class="chip-row" role="tablist" aria-label="加解密模式">
                  <button v-for="tab in cryptoModes" :key="tab.id" class="chip" :class="{ active: mode === tab.id }" type="button" role="tab" :aria-selected="mode === tab.id" :data-testid="`mode-${tab.id}`" @click="swapMode(tab.id)">{{ tab.label }}</button>
                </div>
                <button v-if="mode === 'decrypt'" type="button" class="ghost small" data-testid="copy-ciphertext" @click="copyCiphertext">复制密文</button>
                <button v-else type="button" class="ghost small" data-testid="copy-plaintext" @click="copyPlaintext">复制明文</button>
              </div>
            </header>
            <div class="input-stack">
              <label class="field-label" for="sk-input">sessionKey <span class="hint">(16 字节密钥)</span></label>
              <textarea id="sk-input" v-model="sessionKey" data-testid="input-sessionkey" rows="2" placeholder="Base64 编码的 sessionKey" spellcheck="false"></textarea>
              <label class="field-label" for="iv-input">iv <span class="hint">(16 字节偏移)</span></label>
              <textarea id="iv-input" v-model="iv" data-testid="input-iv" rows="2" placeholder="Base64 编码的 iv" spellcheck="false"></textarea>
              <label class="field-label" for="payload-input">{{ mode === 'decrypt' ? 'encryptedData' : '明文 JSON' }}</label>
              <textarea id="payload-input" v-model="payloadValue" data-testid="input-payload" rows="8" :placeholder="placeholder" spellcheck="false"></textarea>
              <button data-testid="run-crypto" type="button" class="primary" :disabled="busy" @click="run">{{ busy ? '处理中…' : mode === 'decrypt' ? '解密' : '加密' }}</button>
            </div>
          </div>

          <div class="panel output-panel">
            <header class="panel-header">
              <h2>{{ mode === 'decrypt' ? '解密结果' : '加密结果' }}</h2>
              <div class="panel-actions">
                <!-- 解密出来的是明文：改完再加密回去不必手工搬运 -->
                <button v-if="result && mode === 'decrypt'" data-testid="use-as-plaintext" type="button" class="ghost small" @click="useResultAsPlaintext">用作加密输入</button>
                <button v-if="result" data-testid="copy-result" type="button" class="ghost small" @click="copyResult">复制</button>
              </div>
            </header>
            <pre v-if="result" data-testid="crypto-result" class="result-output">{{ result }}</pre>
            <div v-else class="empty-state">
              <strong>{{ mode === 'decrypt' ? '暂无解密结果' : '暂无加密结果' }}</strong>
              <span>填入 sessionKey、iv 和数据后点击执行。</span>
            </div>
          </div>
        </div>
      </main>
    </div>
  </section>
</template>

<style scoped>
.work-layout {
  align-items: start;
  display: grid;
  gap: 14px;
  grid-template-columns: minmax(280px, 340px) minmax(0, 1fr);
}

.source-rail {
  display: flex;
  flex-direction: column;
  gap: 12px;
  min-width: 0;
}

.rail-panel + .rail-panel { margin-top: 0; }
.workbench { display: flex; flex-direction: column; gap: 12px; min-width: 0; }

.hint-text {
  color: var(--faint);
  font-size: 12px;
  line-height: 1.5;
  margin: 0;
  padding: 0 14px 10px;
}

.findings-table { display: flex; flex-direction: column; }
.finding-row {
  border-bottom: 1px solid var(--border);
  cursor: pointer;
  display: grid;
  font-size: 12px;
  gap: 3px;
  padding: 8px 14px;
}
.finding-row:last-child { border-bottom: none; }
.finding-row:hover { background: var(--accent-soft); }
.finding-main { align-items: center; display: flex; gap: 8px; min-width: 0; }
.finding-mask { flex: 1; font-family: var(--mono); font-weight: 600; min-width: 0; overflow-wrap: anywhere; white-space: normal; }
.finding-kind { background: var(--panel-3); border-radius: 99px; color: var(--muted); flex: none; font-size: 11px; padding: 1px 7px; }
.finding-src { color: var(--faint); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

/* 加解密模式是输入面板头上的一个控件组，不再自己占一整行 */
.panel-actions { align-items: center; display: flex; flex-wrap: wrap; gap: .45rem; }

.tool-grid {
  align-items: stretch;
  display: grid;
  gap: 14px;
  grid-template-columns: minmax(0, 1fr) minmax(0, .92fr);
}

.panel-note { color: var(--faint); font-size: 12px; }
.input-stack { display: flex; flex-direction: column; gap: 7px; padding: 12px 14px 14px; }
.compact-stack textarea { min-height: 0; }
.input-stack textarea {
  background: var(--panel-3);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  box-sizing: border-box;
  color: var(--text);
  font-family: var(--mono);
  font-size: 13px;
  min-height: 32px;
  padding: 8px 10px;
  resize: vertical;
  width: 100%;
}
.input-stack textarea:focus { border-color: var(--accent); box-shadow: var(--ring); outline: none; }
.field-label { color: var(--text); font-size: 13px; font-weight: 600; }
.field-label .hint { color: var(--faint); font-size: 12px; font-weight: 400; margin-left: 4px; }
button.primary { align-self: flex-end; margin-top: 5px; padding: 8px 26px; }

.result-output {
  background: var(--panel-3);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-family: var(--mono);
  font-size: 13px;
  margin: 12px 14px 14px;
  max-height: min(430px, calc(100vh - 260px));
  min-height: 280px;
  overflow: auto;
  padding: 12px;
  white-space: pre-wrap;
  word-break: break-all;
}
.output-panel { display: flex; flex-direction: column; }
.output-panel .empty-state { flex: 1; }

@media (max-width: 1250px) {
  .tool-grid { grid-template-columns: 1fr; }
  .result-output { max-height: 360px; min-height: 180px; }
}

@media (max-width: 900px) {
  .work-layout { grid-template-columns: 1fr; }
}
</style>
