/*
 * WxTap Core wxapi hook — page-side API audit hook.
 *
 * Captures wx.* API traffic into a bounded, sequence-numbered buffer
 * drained via the seq+drain protocol (see core/src/hooks/drain.ts).
 *
 * Exposed API (invoked through CDP Runtime.evaluate):
 *   window.wxApiAudit.install()                       -> {ok, hookedCount, installed, frames}
 *   window.wxApiAudit.uninstall()                     -> {ok}
 *   window.wxApiAudit.drain(afterSeq, limit, afterUpdateSeq, updateLimit)
 *                                                     -> {records:[{seq,record}], updates:[{seq,update}],
 *                                                         nextSeq, nextUpdateSeq, hasMore,
 *                                                         droppedRecords, droppedUpdates}
 *   window.wxApiAudit.clearHookedCalls()               -> void
 *   window.wxApiAudit.replay(apiName, options)        -> via window._wxApiReplayDone
 *
 * Two streams share this buffer, both shaped `{seq, <payload>}`:
 *   - records: every call, `pending` at capture time (drained while in flight).
 *   - updates: the outcome frame of every record that *settles*, so a
 *     callback/promise landing after the record was drained still reaches the
 *     consumer instead of mutating an entry it has already read.
 *
 * Overload is observable (R12): both buffers count the entries they evict, and
 * drain() reports the cumulative counts as droppedRecords/droppedUpdates.
 */
(function() {
  if (window._wxApiCoreHooked) return;
  window._wxApiCoreHooked = true;

  // Burst-absorption caps for the two page-side buffers. The value bounds how
  // large a burst the page can hold while the shell is busy, and it is also
  // the page realm's worst-case memory: ~5000 buffered records (plus the same
  // order of settled update frames) held in the miniapp's JS heap.
  //
  // This value is mirrored on the shell side: desktop/internal/api/ipc/
  // hookfeeder.go's `deliveredCapacity` (the replay-dedup FIFO) *must* stay
  // larger than RECORD_BUFFER_CAPACITY. After ResetAck the drain re-reads the
  // page buffer from seq 0, and the FIFO is what filters that replay; sized at
  // or below the page buffer it would let replayed records through as fresh.
  var RECORD_BUFFER_CAPACITY = 5000;
  var UPDATE_BUFFER_CAPACITY = 5000;
  var DEFAULT_UPDATE_LIMIT = 200;

  var hookState = {
    seq: 0,
    buffer: [],
    updateSeq: 0,
    updates: [],
    // Cumulative evictions on each stream, since the hook was loaded or
    // clearHookedCalls() ran. Reported by drain() so the shell can show an
    // overload instead of silently losing the oldest captures.
    droppedRecords: 0,
    droppedUpdates: 0,
    hookedFrames: [],
    origApis: {},
    autoScanTimer: null,
    bridgeHooked: false,
    bridgeTarget: null
  };

  function pushRecord(record) {
    hookState.seq += 1;
    var seq = hookState.seq;
    // rid is the record's stable identity, derived exactly like
    // traffic_records.id (desktop/internal/cloud/drainer.go): "<type>-<appId>-<ts>-<seq>".
    // It lets the event stream and the poll path recognise the same capture.
    record.rid = record.type + '-' + (record.appId || '') + '-' + record.ts + '-' + seq;
    hookState.buffer.push({ seq: seq, record: record });
    while (hookState.buffer.length > RECORD_BUFFER_CAPACITY) {
      hookState.buffer.shift();
      hookState.droppedRecords += 1;
    }
    return seq;
  }

  function pushUpdate(record) {
    var settledAt = Date.now();
    var durationMs = settledAt - record.ts;
    if (!(durationMs >= 0)) durationMs = 0;
    // The update frame carries the outcome; the buffer entry wraps it the same
    // way the record buffer does ({seq, record}), so a consumer walks both
    // streams identically.
    var update = {
      rid: record.rid,
      status: record.status,
      durationMs: durationMs,
      settledAt: settledAt
    };
    // result and error are mutually exclusive on a settled call.
    if (record.error !== undefined) {
      update.error = record.error;
    } else if (record.result !== undefined) {
      update.result = record.result;
    }
    hookState.updateSeq += 1;
    hookState.updates.push({ seq: hookState.updateSeq, update: update });
    while (hookState.updates.length > UPDATE_BUFFER_CAPACITY) {
      hookState.updates.shift();
      hookState.droppedUpdates += 1;
    }
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
    return { records: out, nextSeq: nextSeq, hasMore: hasMore };
  }

  function drainUpdates(afterUpdateSeq, limit) {
    var out = [];
    var nextUpdateSeq = afterUpdateSeq;
    var hasMore = false;
    // limit 0 means "do not read the update stream" (the MCP tool predates it
    // and asks for records only). The cursor must stay put, otherwise those
    // updates would be silently skipped for a later caller that does want them.
    if (limit === 0) return { updates: out, nextUpdateSeq: nextUpdateSeq, hasMore: hasMore };
    for (var i = 0; i < hookState.updates.length; i++) {
      var entry = hookState.updates[i];
      if (entry.seq <= afterUpdateSeq) continue;
      if (out.length >= limit) { hasMore = true; break; }
      out.push(entry);
      nextUpdateSeq = entry.seq;
    }
    return { updates: out, nextUpdateSeq: nextUpdateSeq, hasMore: hasMore };
  }

  function safeClone(obj) {
    try { return JSON.parse(JSON.stringify(obj)); } catch (e) { return String(obj); }
  }

  function findAllFrames() {
    var frames = [];
    var seen = [];
    function tryAdd(w) {
      try {
        if (!w || !w.wx) return;
        for (var i = 0; i < seen.length; i++) { if (seen[i] === w) return; }
        seen.push(w);
        frames.push(w);
      } catch (e) {}
    }
    tryAdd(window);
    var sources = [window];
    try { if (window.parent && window.parent !== window) sources.push(window.parent); } catch (e) {}
    for (var s = 0; s < sources.length; s++) {
      try {
        var src = sources[s];
        if (src.frames) {
          for (var i = 0; i < src.frames.length; i++) {
            try { tryAdd(src.frames[i]); } catch (e) {}
          }
        }
      } catch (e) {}
    }
    return frames;
  }

  function getAppIdFromFrame(frame) {
    try {
      var cfg = frame.__wxConfig || {};
      if (cfg.accountInfo) {
        var ai = cfg.accountInfo;
        if (ai.appAccount && ai.appAccount.appId) return ai.appAccount.appId;
        if (ai.appId) return ai.appId;
      }
      if (cfg.appId) return cfg.appId;
      if (cfg.appid) return cfg.appid;
    } catch (e) {}
    try {
      var info = frame.wx && frame.wx.getAccountInfoSync && frame.wx.getAccountInfoSync();
      var miniProgram = info && info.miniProgram;
      if (miniProgram && miniProgram.appId) return miniProgram.appId;
    } catch (e) {}
    return '';
  }

  // calledAt is the moment the API was invoked. Callers that only record after
  // their callback fires (the bridge path below) must pass it, or ts becomes
  // the settle time: the record's CapturedAt drifts late and its settled
  // durationMs collapses to ~0, which the audit panel shows as "instant".
  function record(type, name, appId, data, result, status, error, calledAt) {
    var r = {
      type: type, name: name, appId: appId,
      data: data, timestamp: new Date().toLocaleTimeString(),
      ts: calledAt === undefined ? Date.now() : calledAt, status: status || 'pending'
    };
    if (result !== undefined) r.result = result;
    if (error !== undefined) r.error = error;
    // The async wrappers settle their entry by seq; without the returned
    // sequence every wx.request-style capture stayed 'pending' forever.
    var seq = pushRecord(r);
    // A record created already settled (sync APIs, the bridge path) never
    // reaches settleBySeq, so without this frame its duration and outcome
    // would never be delivered - the panel would show it as pending forever.
    if (r.status !== 'pending') pushUpdate(r);
    return seq;
  }

  var apiList = [
    { name: 'login',                 type: 'wx.auth' },
    { name: 'checkSession',          type: 'wx.auth' },
    { name: 'getUserInfo',           type: 'wx.auth' },
    { name: 'getUserProfile',        type: 'wx.auth' },
    { name: 'getPhoneNumber',        type: 'wx.auth' },
    { name: 'authorize',             type: 'wx.auth' },
    { name: 'request',               type: 'wx.request' },
    { name: 'uploadFile',            type: 'wx.request' },
    { name: 'downloadFile',          type: 'wx.request' },
    { name: 'connectSocket',         type: 'wx.request' },
    { name: 'requestPayment',        type: 'wx.pay' },
    { name: 'requestOrderPayment',   type: 'wx.pay' },
    { name: 'setStorage',            type: 'wx.storage' },
    { name: 'setStorageSync',        type: 'wx.storage' },
    { name: 'getStorage',            type: 'wx.storage' },
    { name: 'getStorageSync',        type: 'wx.storage' },
    { name: 'removeStorage',         type: 'wx.storage' },
    { name: 'shareAppMessage',       type: 'wx.share' },
    { name: 'shareFileMessage',      type: 'wx.share' },
    { name: 'getLocation',           type: 'wx.location' },
    { name: 'chooseLocation',        type: 'wx.location' },
    { name: 'scanCode',              type: 'wx.device' },
    { name: 'getSystemInfo',         type: 'wx.device' },
    { name: 'getSystemInfoSync',     type: 'wx.device' },
    { name: 'navigateTo',            type: 'wx.nav' },
    { name: 'redirectTo',            type: 'wx.nav' },
    { name: 'navigateToMiniProgram', type: 'wx.nav' },
    { name: 'chooseImage',           type: 'wx.media' },
    { name: 'chooseMedia',           type: 'wx.media' },
    { name: 'previewImage',          type: 'wx.media' },
    { name: 'setClipboardData',      type: 'wx.clipboard' },
    { name: 'getClipboardData',      type: 'wx.clipboard' }
  ];

  function install() {
    var frames = findAllFrames();
    // No frame with wx at all: there is genuinely nothing to hook, and no
    // earlier install can have hooked one (uninstall() clears hookedFrames).
    if (frames.length === 0) {
      return { ok: false, reason: 'no frames with wx found' };
    }
    var hookedCount = 0;

    for (var fi = 0; fi < frames.length; fi++) {
      var f = frames[fi];
      try { if (!f.wx) continue; } catch (e) { continue; }
      var wx = f.wx;
      if (wx._wxApiCoreHookInstalled) continue;
      wx._wxApiCoreHookInstalled = true;
      var appId = getAppIdFromFrame(f);
      hookState.hookedFrames.push({ frame: f, appId: appId });

      for (var ai = 0; ai < apiList.length; ai++) {
        (function(api) {
          var orig = wx[api.name];
          if (!orig || typeof orig !== 'function') return;
          if (!hookState.origApis[api.name]) hookState.origApis[api.name] = orig;

          if (api.name.indexOf('Sync') !== -1) {
            wx[api.name] = function() {
              var args = Array.prototype.slice.call(arguments);
              var callData = args.length > 0 ? safeClone(args[0]) : {};
              try {
                var ret = orig.apply(wx, arguments);
                record(api.type, api.name, appId, callData, safeClone(ret), 'success');
                return ret;
              } catch (e) {
                record(api.type, api.name, appId, callData, null, 'fail', e.message || String(e));
                throw e;
              }
            };
            hookedCount++;
            return;
          }

          wx[api.name] = function(options) {
            options = options || {};
            var callData = {};
            var callName = api.name;

            if (api.name === 'request') {
              callName = (options.method || 'GET') + ' ' + (options.url || '');
              callData = {
                url: options.url,
                method: options.method || 'GET',
                data: safeClone(options.data || {}),
                header: safeClone(options.header || {})
              };
            } else {
              var keys = Object.keys(options);
              for (var k = 0; k < keys.length; k++) {
                var key = keys[k];
                if (key === 'success' || key === 'fail' || key === 'complete') continue;
                try { callData[key] = safeClone(options[key]); } catch (e) {}
              }
            }

            var seq = record(api.type, callName, appId, callData, null, 'pending');
            var origSuccess = options.success;
            var origFail = options.fail;
            if (origSuccess) {
              options.success = function(res) {
                settleBySeq(seq, 'success', safeClone(res), undefined);
                origSuccess(res);
              };
            }
            if (origFail) {
              options.fail = function(err) {
                settleBySeq(seq, 'fail', undefined, err ? (err.errMsg || JSON.stringify(err)) : 'unknown');
                origFail(err);
              };
            }

            var ret = orig.call(wx, options);
            if (ret && typeof ret.then === 'function') {
              ret.then(function(res) {
                settleIfPending(seq, 'success', safeClone(res), undefined);
              })['catch'](function(err) {
                settleIfPending(seq, 'fail', undefined, err ? (err.errMsg || JSON.stringify(err)) : 'unknown');
              });
            }
            return ret;
          };
          hookedCount++;
        })(apiList[ai]);
      }
    }

    hookBridge();
    startAutoScan();
    // `ok` is cumulative, not per-call: install() is re-entered on every
    // auto-scan tick and on every "start capture" click, and a second call in
    // the same realm finds wx._wxApiCoreHookInstalled already set, so nothing
    // is newly hooked. Reporting ok:false there would fail a start that is in
    // fact capturing (the shell turns ok:false into an error). `hookedCount`
    // and `frames` keep their per-call meaning; `installed` reports how many
    // frames are hooked right now.
    return {
      ok: hookState.hookedFrames.length > 0 || hookState.bridgeHooked,
      hookedCount: hookedCount,
      installed: hookState.hookedFrames.length,
      frames: frames.length
    };
  }

  function findEntry(seq) {
    for (var i = hookState.buffer.length - 1; i >= 0; i--) {
      if (hookState.buffer[i].seq === seq) return hookState.buffer[i];
    }
    return null;
  }

  function settleBySeq(seq, status, result, error) {
    var entry = findEntry(seq);
    if (!entry) return; // already evicted; nothing to settle
    var r = entry.record;
    // A record settles once: a repeat settle (success callback plus promise
    // resolution) must not append a second update frame for the same rid.
    var changed = r.status !== status;
    if (result !== undefined) r.result = result;
    if (error !== undefined) r.error = error;
    r.status = status;
    if (changed) pushUpdate(r);
  }

  function settleIfPending(seq, status, result, error) {
    var entry = findEntry(seq);
    if (!entry || entry.record.status !== 'pending') return;
    settleBySeq(seq, status, result, error);
  }

  function startAutoScan() {
    if (hookState.autoScanTimer) return;
    hookState.autoScanTimer = setInterval(function() {
      install();
    }, 3000);
  }

  function uninstall() {
    if (hookState.autoScanTimer) {
      clearInterval(hookState.autoScanTimer);
      hookState.autoScanTimer = null;
    }
    for (var fi = 0; fi < hookState.hookedFrames.length; fi++) {
      var f = hookState.hookedFrames[fi].frame;
      try {
        if (!f.wx) continue;
        for (var name in hookState.origApis) {
          try { f.wx[name] = hookState.origApis[name]; } catch (e) {}
        }
        delete f.wx._wxApiCoreHookInstalled;
      } catch (e) {}
    }
    hookState.hookedFrames = [];
    hookState.origApis = {};
    // Restore the bridge wrapper too: leaving it installed kept capturing
    // into the buffer after wxapi.stop, and the next start re-drained those
    // stopped-period records as fresh traffic.
    if (hookState.bridgeTarget) {
      try {
        hookState.bridgeTarget.bridge.invoke = hookState.bridgeTarget.origInvoke;
        delete hookState.bridgeTarget.bridge._wxApiCoreBridgeHooked;
      } catch (e) {}
      hookState.bridgeTarget = null;
      hookState.bridgeHooked = false;
    }
  }

  function replay(apiName, options) {
    var frames = findAllFrames();
    var wx = null;
    for (var i = 0; i < frames.length; i++) {
      try { if (frames[i].wx) { wx = frames[i].wx; break; } } catch (e) {}
    }
    if (!wx) {
      window._wxApiReplayDone = JSON.stringify({ ok: false, error: 'wx not found' });
      return;
    }

    var fn = hookState.origApis[apiName] || wx[apiName];
    if (!fn || typeof fn !== 'function') {
      window._wxApiReplayDone = JSON.stringify({ ok: false, error: apiName + ' not available' });
      return;
    }

    window._wxApiReplayDone = null;
    var opts;
    try { opts = JSON.parse(JSON.stringify(options || {})); } catch (e) { opts = {}; }
    opts.success = function(res) {
      try { window._wxApiReplayDone = JSON.stringify({ ok: true, status: 'success', result: JSON.parse(JSON.stringify(res)) }); }
      catch (e) { window._wxApiReplayDone = JSON.stringify({ ok: true, status: 'success', result: String(res) }); }
    };
    opts.fail = function(err) {
      window._wxApiReplayDone = JSON.stringify({ ok: true, status: 'fail', error: err ? (err.errMsg || JSON.stringify(err)) : 'unknown' });
    };
    opts.complete = function() {
      setTimeout(function() {
        if (!window._wxApiReplayDone) {
          window._wxApiReplayDone = JSON.stringify({ ok: true, status: 'complete', result: 'completed without success/fail' });
        }
      }, 100);
    };
    try {
      fn.call(wx, opts);
    } catch (e) {
      window._wxApiReplayDone = JSON.stringify({ ok: false, status: 'fail', error: e.message || String(e) });
    }
  }

  function hookBridge() {
    if (hookState.bridgeHooked) return;
    var bridge = window.WeixinJSBridge;
    if (!bridge) {
      var frames = findAllFrames();
      for (var fi = 0; fi < frames.length; fi++) {
        try {
          if (frames[fi].WeixinJSBridge) { bridge = frames[fi].WeixinJSBridge; break; }
        } catch (e) {}
      }
    }
    if (!bridge || !bridge.invoke || bridge._wxApiCoreBridgeHooked) return;
    bridge._wxApiCoreBridgeHooked = true;
    var origInvoke = bridge.invoke;
    var appId = '';
    try {
      var info = typeof wx !== 'undefined' && wx.getAccountInfoSync && wx.getAccountInfoSync();
      if (info && info.miniProgram) appId = info.miniProgram.appId || '';
    } catch (e) {}
    hookState.bridgeTarget = { bridge: bridge, origInvoke: origInvoke };

    bridge.invoke = function(method, params, callback) {
      if (method === 'operateWXData' || method === 'operateCloudFunction') {
        return origInvoke.call(bridge, method, params, callback);
      }
      var callData = safeClone(params || {});
      var callName = method;
      // Stamped at the invoke, not in the callback: the record is only created
      // once the bridge answers, and using that moment as ts would report every
      // bridge call as instant.
      var calledAt = Date.now();

      var wrappedCb = function(res) {
        record('wx.bridge', callName, appId, callData, safeClone(res), 'success', undefined, calledAt);
        if (callback) callback(res);
      };
      return origInvoke.call(bridge, method, params, wrappedCb);
    };
    hookState.bridgeHooked = true;
  }

  window.wxApiAudit = {
    install: install,
    uninstall: uninstall,
    drain: function(afterSeq, limit, afterUpdateSeq, updateLimit) {
      var records = drainRecords(afterSeq, limit);
      var updates = drainUpdates(
        afterUpdateSeq === undefined ? 0 : afterUpdateSeq,
        updateLimit === undefined ? DEFAULT_UPDATE_LIMIT : updateLimit
      );
      return {
        records: records.records,
        updates: updates.updates,
        nextSeq: records.nextSeq,
        nextUpdateSeq: updates.nextUpdateSeq,
        hasMore: records.hasMore,
        // Cumulative evictions per stream, independent of the cursors above: a
        // drain neither resets them nor is affected by them.
        droppedRecords: hookState.droppedRecords,
        droppedUpdates: hookState.droppedUpdates
      };
    },
    clearHookedCalls: function() {
      // Both buffers drop their contents; neither sequence rewinds, so an
      // acknowledged consumer still sees everything captured from here on. The
      // drop counters describe what is (no longer) in the buffers, so they
      // restart with them.
      hookState.buffer = [];
      hookState.updates = [];
      hookState.droppedRecords = 0;
      hookState.droppedUpdates = 0;
    },
    replay: replay
  };
})();
