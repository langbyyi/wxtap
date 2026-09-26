/*
 * WxTap Core console hook — page-side console capture.
 *
 * Wraps the page realm's console methods (and the error/unhandledrejection
 * events that never reach console) into a bounded, sequence-numbered buffer
 * drained via the seq+drain protocol (see core/src/hooks/drain.ts).
 *
 * Scope: 当前 WMPF 把小程序的 console 做成了 configurable:false 的访问器，页内
 * 包不上（install 会如实回 ok:false），所以 console.* 的记录改由 Core 的 CDP 事件
 * （Runtime.consoleAPICalled）采集；这个 hook 负责它做不到的部分——未捕获异常
 * （window.onerror）与未处理的 Promise 拒绝。WMPF 内部与原生侧输出两者都看不到，
 * 那类排查看 DevTools 页。
 *
 * Exposed API (invoked through CDP Runtime.evaluate):
 *   window.consoleAudit.install()                  -> {ok, hookedConsoles, unwrappableConsoles, errorsHooked, levels, appId}
 *   window.consoleAudit.uninstallHook()            -> {ok}
 *   window.consoleAudit.drain(afterSeq, limit)     -> {records:[{seq,record}], nextSeq, hasMore, droppedRecords}
 *   window.consoleAudit.clearHookedCalls()          -> void
 */
(function() {
  if (window._wxTapConsoleHooked) return;
  window._wxTapConsoleHooked = true;

  var DRAIN_BUFFER_CAPACITY = 1000;
  // 单条记录的载荷上限：一个 console.log(巨大对象) 不能把 drain 撑爆。
  var MAX_ARG_TEXT = 2000;
  var MAX_ARGS = 8;
  var LEVELS = ['log', 'info', 'warn', 'error', 'debug'];

  var hookState = {
    seq: 0,
    buffer: [],
    // 缓冲溢出时被淘汰的累计条数（drain 响应里以 droppedRecords 报回，与
    // wxapi.js 的 drain 契约一致）：没有这个数，页内丢的记录对所有下游都是
    // 无声的缺口。clearHookedCalls() 把它随缓冲一起归零。
    droppedRecords: 0,
    hookedConsoles: [],
    originals: [],
    errorHooked: false,
    errorsHooked: false,
    appId: '',
    // 只有在「已安装」状态下才允许访问器兜底重新包装，否则 uninstall 之后
    // 任何一次 console 读取都会把它重新包上。
    enabled: false
  };

  function pushRecord(record) {
    hookState.seq += 1;
    hookState.buffer.push({ seq: hookState.seq, record: record });
    while (hookState.buffer.length > DRAIN_BUFFER_CAPACITY) {
      hookState.buffer.shift();
      hookState.droppedRecords += 1;
    }
    return hookState.seq;
  }

  function drainRecords(afterSeq, limit) {
    var out = [];
    var nextSeq = afterSeq;
    var hasMore = false;
    for (var i = 0; i < hookState.buffer.length; i++) {
      var entry = hookState.buffer[i];
      if (entry.seq <= afterSeq) continue;
      if (out.length >= limit) { hasMore = true; break; }
      out.push(entry);
      nextSeq = entry.seq;
    }
    return { records: out, nextSeq: nextSeq, hasMore: hasMore, droppedRecords: hookState.droppedRecords };
  }

  function truncate(text) {
    if (typeof text !== 'string') return '';
    return text.length > MAX_ARG_TEXT ? text.slice(0, MAX_ARG_TEXT) + '…[截断]' : text;
  }

  // 页面里的参数可能是循环引用对象、Error、DOM 节点或函数，JSON.stringify
  // 对前两者会抛错。逐类降级，任何情况下都要返回一个字符串。
  // Error 可能来自另一个 frame 的 realm，instanceof 在跨 realm 时会失败，
  // 所以再按 [[Class]] 判一次。
  function isErrorLike(value) {
    try {
      if (value instanceof Error) return true;
      return Object.prototype.toString.call(value) === '[object Error]';
    } catch (e) {
      return false;
    }
  }

  function formatArg(value) {
    try {
      if (value === undefined) return 'undefined';
      if (value === null) return 'null';
      var kind = typeof value;
      if (kind === 'string') return truncate(value);
      if (kind === 'number' || kind === 'boolean') return String(value);
      if (kind === 'function') return '[Function ' + (value.name || 'anonymous') + ']';
      if (isErrorLike(value)) {
        return truncate((value.name || 'Error') + ': ' + (value.message || '') + (value.stack ? '\n' + value.stack : ''));
      }
      if (kind === 'object') {
        if (value.nodeType) return '[DOM ' + (value.nodeName || 'node') + ']';
        try {
          var json = JSON.stringify(value);
          return json === undefined ? String(value) : truncate(json);
        } catch (e) {
          // 循环引用 / BigInt / 自定义 toJSON 抛错：给一个可读的降级结果。
          return '[无法序列化]';
        }
      }
      return String(value);
    } catch (e) {
      try { return String(value); } catch (e2) { return '[无法序列化]'; }
    }
  }

  function formatArgs(args) {
    var out = [];
    var count = Math.min(args.length, MAX_ARGS);
    for (var i = 0; i < count; i++) {
      out.push(formatArg(args[i]));
    }
    if (args.length > MAX_ARGS) out.push('…还有 ' + (args.length - MAX_ARGS) + ' 个参数');
    return out;
  }

  function getAppId() {
    if (hookState.appId) return hookState.appId;
    var candidates = [window];
    try { if (window.parent && window.parent !== window) candidates.push(window.parent); } catch (e) {}
    for (var i = 0; i < candidates.length; i++) {
      try {
        var source = candidates[i];
        var cfg = source.__wxConfig || {};
        var ai = cfg.accountInfo || {};
        var id = (ai.appAccount && ai.appAccount.appId) || ai.appId || cfg.appid || '';
        if (id) { hookState.appId = id; return id; }
        var info = source.wx && source.wx.getAccountInfoSync && source.wx.getAccountInfoSync();
        var mp = info && info.miniProgram;
        if (mp && mp.appId) { hookState.appId = mp.appId; return mp.appId; }
      } catch (e) {}
    }
    return '';
  }

  function record(level, text, extra) {
    var entry = {
      type: 'console',
      level: level,
      text: text,
      appId: getAppId(),
      timestamp: new Date().toLocaleTimeString(),
      ts: Date.now()
    };
    if (extra !== undefined) entry.extra = extra;
    return pushRecord(entry);
  }

  // 同一段脚本可能被注入到多个窗口（页面 frame 与外层 window 都持有 console），
  // 用对象身份去重，否则每行日志会被记录多次。
  function collectConsoles() {
    var found = [];
    var seen = [];
    function tryAdd(scope) {
      try {
        var target = scope && scope.console;
        if (!target || typeof target.log !== 'function') return;
        for (var i = 0; i < seen.length; i++) { if (seen[i] === target) return; }
        seen.push(target);
        found.push(target);
      } catch (e) {}
    }
    tryAdd(window);
    var sources = [window];
    try { if (window.parent && window.parent !== window) sources.push(window.parent); } catch (e) {}
    for (var s = 0; s < sources.length; s++) {
      try {
        var src = sources[s];
        var frames = src.frames;
        if (!frames) continue;
        for (var i = 0; i < frames.length; i++) {
          try { tryAdd(frames[i]); } catch (e) {}
        }
      } catch (e) {}
    }
    return found;
  }

  // 我们包上去的替换函数挂在同一条 entry 上：每次 install 都按「当前方法是不是
  // 我们那个替换函数」判断，而不是只看对象标记 —— vConsole 这类库会替换掉
  // console 方法，只认标记的话捕获会静默停掉。
  function entryFor(target) {
    for (var i = 0; i < hookState.originals.length; i++) {
      if (hookState.originals[i].target === target) return hookState.originals[i];
    }
    return undefined;
  }

  function wrappedLevelOf(entry, level) {
    if (!entry) return undefined;
    for (var i = 0; i < entry.entries.length; i++) {
      if (entry.entries[i].level === level) return entry.entries[i];
    }
    return undefined;
  }

  // 用访问器而不是普通赋值：小程序运行时（或 vConsole 这类库）会在装载过程中把
  // console 方法换成自己的实现，一旦被换掉，我们的记录就静默停了 —— 真机上
  // console.log/warn 全部丢、只有 window.onerror 还能收到，就是这个原因。
  // 访问器让「谁替换都能重新包上」：
  //   - 读 console.log 永远拿到我们的包装函数（调用链里仍会调到对方的实现）
  //   - 对方赋值时我们把新实现包一层，而不是被它顶掉
  function wrapLevel(target, level) {
    var original = readBack(target, level);
    if (typeof original !== 'function') return undefined;
    var entry = { level: level, original: original, replacement: makeReplacement(target, level, original) };
    function install() {
      Object.defineProperty(target, level, {
        configurable: true,
        enumerable: true,
        get: function() { return entry.replacement; },
        set: function(next) {
          if (typeof next === 'function' && next !== entry.replacement) {
            entry.replacement = makeReplacement(target, level, next);
          }
        }
      });
    }
    try {
      install();
    } catch (e) {
      // defineProperty 不可用时退回普通赋值：至少装得上，只是防不住后续替换。
      try {
        target[level] = entry.replacement;
      } catch (e2) {
        return undefined;
      }
    }
    // 装完必须回读验证：小程序把 console 做成 configurable:false 的访问器时
    // （真机实测 `Cannot redefine property: log`，普通赋值又被对方 setter 吞掉），
    // 上面两条路都会“成功返回”却什么都没换上 —— 不验证就会报「装好了」而零记录。
    if (readBack(target, level) !== entry.replacement) return undefined;
    return entry;
  }

  function readBack(target, level) {
    try {
      return target[level];
    } catch (e) {
      return undefined;
    }
  }

  function makeReplacement(target, level, original) {
    return function() {
      try {
        // 先记录再调用原始方法：原始方法抛错时日志仍然留得下来。
        record(level, formatArgs(arguments).join(' '));
      } catch (e) {}
      return original.apply(target, arguments);
    };
  }

  // window.console 在小程序里通常是访问器实现的（真机实测其 getter 形如
  // ()=>o.value），而对方替换 console 方法走的是 Object.defineProperty ——
  // 不会经过我们的 setter，安装时包好的那一层会被静默顶掉：
  // install 回 ok、window.onerror 收得到，console.log 却一条都采不到。
  // 因此在 window.console 的读取上兜底：每次读到 console 都确保方法是我们的包装。
  function guardConsoleAccessor() {
    try {
      var descriptor = Object.getOwnPropertyDescriptor(window, 'console');
      if (!descriptor || typeof descriptor.get !== 'function' || descriptor.configurable === false) return;
      if (descriptor.get.__wxTapGuarded) return;
      var originalGet = descriptor.get;
      var guarded = function() {
        var target = originalGet.call(window);
        if (hookState.enabled && target) {
          try { hookConsole(target); } catch (e) {}
        }
        return target;
      };
      guarded.__wxTapGuarded = true;
      Object.defineProperty(window, 'console', {
        configurable: true,
        enumerable: descriptor.enumerable,
        get: guarded,
        set: descriptor.set
      });
    } catch (e) {}
  }

  function hookConsole(target) {
    var entry = entryFor(target);
    var wrapped = [];
    for (var i = 0; i < LEVELS.length; i++) {
      var level = LEVELS[i];
      var current;
      try { current = target[level]; } catch (e) { continue; }
      if (typeof current !== 'function') continue;
      var existing = wrappedLevelOf(entry, level);
      if (existing !== undefined && current === existing.replacement) {
        // 已经是我们的访问器：读出来就是包装函数，不用再包。
        wrapped.push(existing);
        continue;
      }
      var fresh = wrapLevel(target, level);
      if (fresh !== undefined) wrapped.push(fresh);
    }
    if (wrapped.length === 0) return;
    if (entry !== undefined) {
      entry.entries = wrapped;
      return;
    }
    hookState.hookedConsoles.push(target);
    hookState.originals.push({ target: target, entries: wrapped });
  }

  // console 之外的错误出口：未捕获异常与未处理的 Promise 拒绝。两者都必须
  // 挂到作用域自己的 window 上，页面 frame 的错误才收得到。
  // 返回是否至少挂上了一个出口：CDP 事件源靠这个决定要不要报
  // Runtime.exceptionThrown（页内已覆盖时再报就是双份错误记录）。
  function hookErrors(scope) {
    var attached = false;
    try {
      var previous = scope.onerror;
      scope.onerror = function(message, source, line, column, error) {
        try {
          record('error', truncate(String(message)), {
            source: String(source || ''),
            line: line === undefined ? '' : String(line),
            column: column === undefined ? '' : String(column),
            stack: error && error.stack ? truncate(String(error.stack)) : ''
          });
        } catch (e) {}
        if (typeof previous === 'function') return previous.apply(scope, arguments);
        return false;
      };
      attached = true;
    } catch (e) {}
    try {
      if (scope.addEventListener) {
        scope.addEventListener('unhandledrejection', function(event) {
          try {
            var reason = event && event.reason;
            var text = reason && reason.stack ? String(reason.stack) : formatArg(reason);
            record('error', truncate('Unhandled rejection: ' + text));
          } catch (e) {}
        });
        attached = true;
      }
    } catch (e) {}
    return attached;
  }

  function install() {
    hookState.enabled = true;
    guardConsoleAccessor();
    var consoles = collectConsoles();
    for (var i = 0; i < consoles.length; i++) {
      hookConsole(consoles[i]);
    }
    if (!hookState.errorHooked) {
      hookState.errorHooked = true;
      hookState.errorsHooked = hookErrors(window);
      var sources = [window];
      try { if (window.parent && window.parent !== window) sources.push(window.parent); } catch (e) {}
      for (var s = 0; s < sources.length; s++) {
        try {
          var frames = sources[s].frames || [];
          for (var f = 0; f < frames.length; f++) {
            try { if (frames[f] !== window && hookErrors(frames[f])) hookState.errorsHooked = true; } catch (e) {}
          }
        } catch (e) {}
      }
    }
    // ok 反映“console.* 是否真的包上了”：页面里没有可包装的 console 时不能报
    // 成功，否则界面会显示「捕获中」却永远零记录。unwrappable 说明有几层被页面
    // 锁住了。errorsHooked 是独立的布尔：未捕获异常/Promise 拒绝的出口与 console
    // 无关，锁住 console 时它依然成立 —— CDP 事件源据此对错误事件去重。
    return {
      ok: hookState.hookedConsoles.length > 0,
      hookedConsoles: hookState.hookedConsoles.length,
      unwrappableConsoles: consoles.length - hookState.hookedConsoles.length,
      errorsHooked: hookState.errorsHooked === true,
      levels: LEVELS.length,
      appId: getAppId()
    };
  }

  function uninstallHook() {
    // 先关兜底：否则还原之后任何一次 console 读取又会把它包回去。
    hookState.enabled = false;
    for (var i = 0; i < hookState.originals.length; i++) {
      var entry = hookState.originals[i];
      for (var j = 0; j < entry.entries.length; j++) {
        var wrapped = entry.entries[j];
        try {
          // 访问器要整个换回普通属性：走 setter 会把还原值再包一层。
          Object.defineProperty(entry.target, wrapped.level, {
            configurable: true,
            enumerable: true,
            writable: true,
            value: wrapped.original
          });
        } catch (e) {
          try { entry.target[wrapped.level] = wrapped.original; } catch (e2) {}
        }
      }
    }
    hookState.originals = [];
    hookState.hookedConsoles = [];
    return { ok: true };
  }

  window.consoleAudit = {
    install: install,
    uninstallHook: uninstallHook,
    drain: drainRecords,
    // 丢弃计数与缓冲一起归零（同 wxapi.js 的语义）：计数描述的是"缓冲里
    // （曾经）有什么"，缓冲清空后旧读数就过期了。
    clearHookedCalls: function() {
      hookState.buffer = [];
      hookState.droppedRecords = 0;
    }
  };
})();
