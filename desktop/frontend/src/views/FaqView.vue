<script setup lang="ts">
import { computed, ref } from 'vue';
import { RouterLink } from 'vue-router';
import PageHeader from '../components/PageHeader.vue';

type Link = { to: string; label: string };
type Entry = { title: string; answer: string[]; link?: Link };
type Step = { title: string; detail: string; link?: Link };
// 上手顺序是实测结论：引擎 attach 的对象是 WMPF 宿主进程（小程序起来才有），
// 9421 也是 Core 监听、小程序拨入。这一段不参与下面的筛选——「怎么用起来」要在进页第一屏。
const quickStart: Step[] = [
  { title: '准备环境', detail: 'Windows 10/11 x64（需已安装 WebView2 Runtime，Win11 自带）或 macOS（Intel / Apple Silicon）；两者都需要已安装 Node.js 22 或更新版本（WxTap 不内置 Node，Core 是一个 Node 进程）。' },
  { title: '先在微信里打开一个小程序', detail: '引擎附加的是 WMPF 宿主进程（Windows 为 WeChatAppEx.exe，macOS 为 WeChatAppEx），它由小程序启动时创建。' },
  { title: '在「状态」页启动引擎', detail: '启动后「组件状态」里的「Frida 注入」变为「已连接」；小程序与 DevTools 两条通道也通时，页面出现「三条通道已就绪」的提示。', link: { to: '/control', label: '状态' } },
  { title: '确认版本支持', detail: '「状态」页的「版本支持」为「支持」表示该微信构建有静态地址表（引擎尚未读到微信构建号时显示「未检测」）；为「不支持」时启动会自动检测偏移，这期间保持小程序打开，通常约 1 分钟（上限 5 分钟）。', link: { to: '/control', label: '状态' } },
  { title: '按需进入功能页', detail: '调试：页面路由 / Console 日志 / DevTools / vConsole / 注入脚本；流量：WxAPI / 云函数 / 历史记录；代码：反编译 / 代码浏览；利用：SessionKey / 微信 AK；系统：设置 / MCP 服务；帮助：使用帮助 / 交流反馈。' },
  { title: '出现问题时先查看运行日志', detail: '「状态」页的运行日志是 shell 与 Core 的唯一输出口，同一份内容落盘在数据目录的 logs/wxtap-<日期>.log（Windows 与 WxTap.exe 同目录，macOS 是 ~/Library/Application Support/WxTap）。', link: { to: '/control', label: '状态' } },
];

const entries: Entry[] = [
  // —— 上手 ——
  {
    title: '为什么必须先打开小程序，再启动引擎？',
    answer: [
      '引擎附加的是 WMPF 宿主进程（Windows 为 WeChatAppEx.exe，macOS 为 WeChatAppEx），它由小程序启动时创建；微信主进程里没有可附加的 WMPF 运行时。',
      '9421 调试通道同样是 Core 监听、小程序主动拨入，目标小程序不存在时这条链路根本不会建立。',
    ],
    link: { to: '/control', label: '状态' },
  },
  {
    title: '发布包是否需要单独安装 Node.js？',
    answer: [
      '需要。WxTap 不随包分发 Node（发布目录里没有 runtime，Core 直接由系统上的 Node 拉起），所以也要求 Node 22+：Core 编译到 ES2024。版本探测在启动之前完成，低于 22 的候选直接被拒绝并给出可执行报错 —— 不会出现「启动后才失败」的情况。',
      '解析顺序是 WXTAP_CORE_CMD 环境变量（开发者覆盖用）→「设置」页保存的路径 → 系统 PATH（nvm-windows / volta 的 .cmd 垫片同样可用）→ 常见安装位置自动检测；每个候选都会先执行一次版本探测。前两级是显式指定的，不合格即当场报错，不会改用其他 Node；PATH 与自动检测这两级则会跳过不合格的候选继续查找，跳过原因会一并显示。',
      '启动引擎报找不到 Node 时，「设置」页的「Node 运行时」会显示当前解析结果与错误原文，可以用「自动检测」一键填入。',
    ],
    link: { to: '/settings', label: '设置' },
  },
  // —— 连接 ——
  {
    title: 'Frida 一直显示「未连接」，或启动时报找不到 WMPF 宿主',
    answer: [
      '按顺序排查：微信是否已登录 → 是否已经打开过至少一个小程序 → 「版本支持」是否为「支持」→ 安全软件是否隔离或拦截了微信进程。',
      'macOS 上还可能是 SIP / 代码签名不允许调试，报错形如「Unable to access process with pid … from the current user account」：推荐对 WeChatAppEx 做 Ad-Hoc 重签名，关闭 SIP 是备选且不推荐（命令与核实步骤见 docs/WECHAT_E2E_CHECKLIST.md 的 macOS 前置项）。Windows 上的同类原因是权限不匹配——微信以管理员身份运行时，本程序也要以管理员身份启动。',
      '出现「未找到微信 WMPF 宿主进程（WeChatAppEx.exe）」表示前两项条件未满足（macOS 上的同类报错是「未找到微信 WMPF 宿主进程（WeChatAppEx / WeChatAppEx Helper）」）；出现「未找到可验证的微信 WMPF 宿主：宿主进程与已登录微信不匹配」（macOS 上是「发现多个微信 WMPF 宿主进程」）表示找到的宿主与当前登录的微信不匹配，通常是有多个微信实例，请只保留一个后重启。',
      '仍然失败时请查看「状态」页的运行日志：附加失败的原因记录在其中，首条错误通常为根本原因，后续错误多为连锁反应。',
    ],
    link: { to: '/control', label: '状态' },
  },
  {
    title: '「版本支持」显示「不支持」如何处理？',
    answer: [
      '含义是当前微信构建没有随包的静态地址表。Windows 上启动时会自动检测偏移（保持小程序打开，通常约 1 分钟），检测失败会给出失败原因（例如小程序没打开导致的超时）；macOS 不做自动检测（偏移检测是 PE 扫描器，没有 Mach-O 对应物），会直接报「需补充 resources/frida/config/mac/addresses.<build>.json」。',
      '把微信升级或降级到地址表已覆盖的构建，可以免去这一步检测。',
    ],
    link: { to: '/control', label: '状态' },
  },
  // —— 调试 ——
  {
    title: 'DevTools 打开后显示空白',
    answer: [
      '顺序是：启动引擎 → 打开小程序 → 再打开 DevTools 地址；若先打开 DevTools 再进入小程序，内容需等待调试器连接后才会出现。',
      '引擎没启动时「DevTools」页会直接提示先到「状态」页启动；多个小程序时页面会列出 appid 清单（默认选中第一个），确认选中的是目标再打开。',
    ],
    link: { to: '/devtools', label: 'DevTools' },
  },
  {
    title: '注入后的脚本能否撤回？',
    answer: [
      '注入后无法撤回：代码会留在当前页面里，直到小程序重新加载或被切换。',
      '标记为「全局」的脚本会在小程序重新加载后自动再次注入。',
    ],
    link: { to: '/hook', label: '注入脚本' },
  },
  {
    title: '是否建议开启 vConsole？',
    answer: [
      '它走的是非正规的小程序调试通道，有封号风险，建议只对测试账号使用。',
      '面板位于小程序窗口内，本程序无法读取其内容 —— 查看日志请走「Console 日志」页，请求走「WxAPI」「云函数」「历史记录」页。',
      '如仅需查看页面 console 输出，「Console 日志」页或注入脚本即可满足。',
    ],
    link: { to: '/vconsole', label: 'vConsole' },
  },

  // —— 流量 ——
  {
    title: '如何开始捕获 wx.* 调用？',
    answer: [
      '在「WxAPI」页点击「开启捕获」，前提是小程序已连接。页面上下文不可用（未连接、页面里没有 wx 对象）时会报错并停留在「未捕获」，不会显示成正在捕获。',
      '连接本身不开始采集：钩子是在点击「开启捕获」的那一刻安装的，因此列表里只会出现点击之后的调用。',
      '实时列表上限 2000 行，超出后最早的记录被裁掉并给出提示；已入库的历史记录不受影响。',
    ],
    link: { to: '/wxapi', label: 'WxAPI' },
  },
  {
    title: '开启捕获报「安装 wxapi 钩子失败」',
    answer: [
      '冒号后面是页面给出的失败原因（例如 no frames with wx found，或「页面未找到可注入的 wx 环境」），含义都是钩子未能在当前页面安装，因为找不到可注入的 wx 环境。',
      '先在「状态」页确认小程序为「运行中」，待页面加载完成后再点击「开启捕获」；刚切换小程序时尤其容易出现这一时序窗口。',
    ],
    link: { to: '/wxapi', label: 'WxAPI' },
  },
  {
    title: '列表中部分调用始终停留在「等待中」',
    answer: [
      '这是还没落定的调用：记录在发起时先以 pending 入队（进行中可见），回调落定后才把状态、返回值和耗时原地覆写回来。',
      '耗时较长的接口显示「等待中」属于正常现象；长时间未落定时，请检查小程序是否已被切换或页面已销毁。',
    ],
    link: { to: '/wxapi', label: 'WxAPI' },
  },
  {
    title: '删除或清空历史记录前需要注意什么？',
    answer: [
      '工具栏的「清空」打开确认框，写明将删除多少条、此操作不可恢复：确认后一次删掉全部已入库记录。',
      '只想删除少量记录，可使用列表里的勾选：「删除选中 (N)」或详情面板的「删除这条」，都会先弹确认框写明条数。删除的都是已入库记录，不可恢复。',
      '流量库不会自动裁剪——没有保留策略，也没有后台清理：只有主动删除或清空才能使其变小。清空会执行一次空间回收（VACUUM，删除比例达到阈值时），大库上此期间界面可能变慢。',
    ],
    link: { to: '/traffic', label: '历史记录' },
  },
  {
    title: '重放是否会改动线上数据？',
    answer: [
      '重放会用原参数重新发送一次请求，语义与原调用一致——对写接口执行重放等同于再次真实写入。',
      '只对自己有授权的目标重放；重放结果在记录详情下方单独展示。',
    ],
    link: { to: '/wxapi', label: 'WxAPI' },
  },

  // —— 代码与利用 ——
  {
    title: '反编译需要先准备什么？',
    answer: [
      '需要指向微信的小程序包目录（Applet）。「反编译」页的「?」列出了各版本微信的默认路径与本机候选目录，命中后可直接「使用」。',
      'Windows v3 与 v4 的路径不同，v4 还会带一段 32 位随机的用户目录名。反编译是长任务，页内显示阶段与进度，失败会给出原因。',
    ],
    link: { to: '/extract', label: '反编译' },
  },
  {
    title: '为什么「反编译」页找不到某个小程序？',
    answer: [
      '列表只收录「有包可还原」的小程序：原始 wxapkg 已被微信清理掉、只剩上次产物的，不进列表。',
      '页面模板由微信新版编译模板运行时（__wxCodeSpace__）生成的小程序也不进列表 —— 本工具无法还原这类模板，若强行还原，整次还原会中止且不产出任何文件。这类 app 在枚举阶段即被识别并排除，因此「找不到」不等于「微信里没有」。',
    ],
    link: { to: '/extract', label: '反编译' },
  },
  {
    title: '如何获取 session_key 与 iv？',
    answer: [
      '三种方式：直接粘贴 code2Session 返回值或抓包得到的组合 JSON（兼容 session_key / sessionKey、iv / ivBase64、encryptedData / encrypted_data，并可自动做 URL 解码）；从已入库的抓包记录中提取；扫描反编译产物。',
      '两条提取路径都要求先有数据——抓包记录要先在「WxAPI」或「云函数」开启过捕获，反编译产物需先执行过「反编译」。拿到后即可对开放数据做 AES 加解密。',
    ],
    link: { to: '/sessionkey', label: 'SessionKey' },
  },
  {
    title: '「微信 AK」页有哪些功能？',
    answer: [
      '填入凭据（小程序 / 公众号用 AppID + AppSecret，企业微信用 CorpID + CorpSecret），向官方接口确认凭据是否有效并取得 access_token，然后按类型浏览可利用的官方接口。',
      '接口清单不包含任何消息发送类接口（模板消息 / 订阅消息 / 客服消息 / webhook）。凭据与 token 只存在于当前会话内存，重启后本页为空。',
    ],
    link: { to: '/ak', label: '微信 AK' },
  },

  // —— 系统 ——
  {
    title: '界面有哪些快捷键？',
    answer: [
      'Ctrl/⌘ + K 打开快速跳转面板：输入页面名或路径即可跳转，↑↓ 选择、Enter 打开、Esc 关闭。',
      '在任意页面上按 / 会直接定位到该页的搜索框（长列表不必先用鼠标点进去）。',
      '捕获类列表（WxAPI / 云函数 / 历史记录）的工具栏里有「紧凑」勾选框，压缩行高与字号，一屏可显示更多行。',
    ],
  },
  {
    title: '运行目录、日志和抓取结果存放在哪里？',
    answer: [
      'Windows 上默认与 WxTap.exe 同目录：配置、日志、反编译输出、traffic.db 均存放在该目录；macOS 位于 ~/Library/Application Support/WxTap。',
      '审计类记录（WxAPI / 云函数）写入 traffic.db，重启后仍在；Console 与 vConsole 的输出不入库。',
      '「设置」页可以看到当前生效的路径。可用 WXTAP_DATA_DIR、WXTAP_TRAFFIC_DB、WXTAP_PACKAGES_DIR 覆盖；数据目录不应纳入 Git。',
    ],
    link: { to: '/settings', label: '设置' },
  },
  {
    title: '日志是否可以直接对外提交？',
    answer: [
      '不可以直接提交：日志含目标小程序与云函数的运行数据，对外分享前请先审阅并脱敏。',
      '日志按天写入，单文件超过 1 MB 会自动轮转。',
    ],
  },
  {
    title: '「MCP 服务」如何接入？',
    answer: [
      '两种方式：「MCP 服务」页启动 GUI 内嵌的 MCP 服务（默认 9527，同一端口同时提供 Streamable HTTP 的 POST /mcp 与旧式 SSE 的 GET /sse），或让 MCP 客户端用 WxTap.exe -mcp 以 stdio 方式启动。页内可直接复制启动命令与 mcpServers 配置。',
      '接入后外部智能体可以调试当前连接的小程序，工具面覆盖引擎、抓包、导航、反编译等。',
    ],
    link: { to: '/mcp', label: 'MCP 服务' },
  },
  {
    title: '捕获页出现「存储不可用」警示如何处理？',
    answer: [
      '「WxAPI」或「云函数」页顶部出现该警示，说明本地存储初始化失败：捕获本身照常工作，实时列表继续接收记录，但记录不会写入历史记录库。',
      '记录仍可查看：当前捕获页中照常显示且可复制；但「历史记录」页查询不到这些内容，重启后也不会保留。',
      '恢复方式：重启应用让存储重新初始化；若重启后仍然出现，请检查磁盘剩余空间与数据目录的读写权限（目录位置见「设置」页）。',
    ],
    link: { to: '/traffic', label: '历史记录' },
  },
];

const query = ref('');
const expanded = ref(new Set<number>());
const qsOpen = ref(true);
const searchNeedle = computed(() => query.value.trim().toLowerCase());
const searching = computed(() => searchNeedle.value.length > 0);
const filtered = computed(() => {
  const needle = searchNeedle.value;
  return entries
    .map((entry, index) => ({ ...entry, index }))
    .filter((entry) => !needle || `${entry.title} ${entry.answer.join(' ')}`.toLowerCase().includes(needle));
});
// 搜索时把命中片段标出来：答案往往好几段，标出命中点就不用逐行找
type Segment = { text: string; hit: boolean };
function segments(text: string): Segment[] {
  const needle = searchNeedle.value;
  if (!needle) return [{ text, hit: false }];
  const output: Segment[] = [];
  let rest = text;
  while (rest) {
    const at = rest.toLowerCase().indexOf(needle);
    if (at < 0) {
      output.push({ text: rest, hit: false });
      break;
    }
    if (at > 0) output.push({ text: rest.slice(0, at), hit: false });
    output.push({ text: rest.slice(at, at + needle.length), hit: true });
    rest = rest.slice(at + needle.length);
  }
  return output;
}
// 搜索时自动展开匹配项，避免逐条点开才能阅读答案
function isOpen(index: number) {
  return searching.value || expanded.value.has(index);
}
const allExpanded = computed(() => filtered.value.length > 0 && filtered.value.every((entry) => isOpen(entry.index)));
function toggle(index: number) {
  const next = new Set(expanded.value);
  if (next.has(index)) next.delete(index);
  else next.add(index);
  expanded.value = next;
}
function toggleAll() {
  expanded.value = allExpanded.value
    ? new Set()
    : new Set(filtered.value.map((entry) => entry.index));
}
</script>

<template>
  <section class="help-view" aria-labelledby="help-title">
    <PageHeader title="使用帮助" title-id="help-title" />

    <div class="panel help-quickstart">
      <header class="panel-header">
        <h2>快速上手</h2>
        <div class="toolbar"><button class="secondary small" type="button" @click="qsOpen = !qsOpen">{{ qsOpen ? '收起' : '展开' }}</button></div>
      </header>
      <ol v-show="qsOpen" class="help-steps">
        <li v-for="step in quickStart" :key="step.title">
          <strong>{{ step.title }}</strong>
          <p>{{ step.detail }}</p>
          <RouterLink v-if="step.link" class="help-link" :to="step.link.to">打开「{{ step.link.label }}」</RouterLink>
        </li>
      </ol>
    </div>

    <div class="panel help-search">
      <input v-model="query" type="search" placeholder="搜索用法、报错原文或关键词" aria-label="搜索帮助">
      <span v-if="searching" class="subnav-count">{{ filtered.length }} / {{ entries.length }}</span>
      <button type="button" class="secondary" :disabled="!filtered.length || searching" @click="toggleAll">
        {{ allExpanded ? '收起全部' : '展开全部' }}
      </button>
    </div>

    <div v-if="filtered.length" class="stack">
      <article v-for="item in filtered" :key="item.index" class="panel help-item">
        <button class="help-question" type="button" :aria-expanded="isOpen(item.index)" @click="toggle(item.index)">
          <h2>
            <template v-for="(seg, segIndex) in segments(item.title)" :key="segIndex">
              <mark v-if="seg.hit">{{ seg.text }}</mark>
              <template v-else>{{ seg.text }}</template>
            </template>
          </h2>
          <span class="help-chevron" aria-hidden="true">{{ isOpen(item.index) ? '−' : '+' }}</span>
        </button>
        <div v-show="isOpen(item.index)" class="panel-body help-answer">
          <p v-for="(line, lineIndex) in item.answer" :key="lineIndex">
            <template v-for="(seg, segIndex) in segments(line)" :key="segIndex">
              <mark v-if="seg.hit">{{ seg.text }}</mark>
              <template v-else>{{ seg.text }}</template>
            </template>
          </p>
          <RouterLink v-if="item.link" class="help-link" :to="item.link.to">打开「{{ item.link.label }}」</RouterLink>
        </div>
      </article>
    </div>
    <div v-else class="panel empty-state">
      <strong>没有匹配的内容</strong>
      <span>请更换更短的关键词，例如「Frida」「CDP」「session_key」，或改用上方「快速上手」。</span>
      <span>仍未解决？<RouterLink class="help-link" to="/feedback">前往「交流反馈」提交 Issue</RouterLink></span>
    </div>
  </section>
</template>

<style scoped>
.help-quickstart {
  margin-bottom: 1rem;
}

.help-steps {
  display: grid;
  gap: .7rem;
  list-style: none;
  margin: 0;
  padding: 1.1rem 1.1rem 1.1rem 2.6rem;
  counter-reset: step;
}

.help-steps li {
  counter-increment: step;
  display: grid;
  gap: .2rem;
  position: relative;
}

.help-steps li::before {
  color: var(--faint);
  content: counter(step);
  font-family: var(--mono);
  font-size: .82rem;
  left: -1.5rem;
  position: absolute;
  top: .1rem;
}

.help-steps strong {
  font-size: .92rem;
}

.help-steps p {
  color: var(--muted);
  margin: 0;
  max-width: 52rem;
}

.help-search {
  align-items: center;
  display: flex;
  gap: .75rem;
  margin-bottom: 1rem;
  padding: .65rem .8rem;
}

.help-search input {
  flex: 1 1 20rem;
  max-width: 30rem;
  min-width: 12rem;
}

.help-search button {
  flex: none;
  white-space: nowrap;
}

.help-item {
  overflow: hidden;
}

.help-question {
  align-items: baseline;
  background: transparent;
  border: 0;
  border-radius: 0;
  color: var(--text);
  display: flex;
  gap: .7rem;
  padding: .85rem 1rem;
  text-align: left;
  width: 100%;
}

.help-question:hover:not(:disabled) {
  background: var(--panel-2);
}

.help-item h2 {
  flex: 1 1 auto;
  font-size: .95rem;
  font-weight: 650;
  margin: 0;
}

.help-chevron {
  color: var(--faint);
  flex: none;
  font-family: var(--mono);
  font-size: 1rem;
}

.help-answer {
  border-top: 1px solid var(--border);
  display: grid;
  gap: .5rem;
}

.help-answer p {
  color: var(--muted);
  margin: 0;
  max-width: 52rem;
}

.help-link {
  font-size: .82rem;
  justify-self: start;
}

.help-question mark,
.help-answer mark {
  background: rgba(250, 204, 21, 0.3);
  border-radius: 2px;
  color: inherit;
  padding: 0 .05em;
}

.empty-state {
  gap: .3rem;
}
</style>
