/*
 * WxTap Core cloud audit hook.
 *
 * Captures wx.cloud method traffic (callFunction/storage/container and other
 * methods), database proxy calls (collection add/get/update/remove/count,
 * doc, where, aggregate), direct wx.cloud hooks, and WeixinJSBridge
 * operateWXData/operateCloudFunction. Also provides manual-call and
 * static-scan helpers. Records go into a bounded, sequence-numbered buffer
 * drained via the seq+drain protocol (same shape as core/hooks/wxapi.js).
 *
 * Exposed API (invoked through CDP Runtime.evaluate):
 *   window.cloudAudit.install() (alias installHook)   -> {ok, totalHooked, ...}
 *   window.cloudAudit.uninstallHook()                 -> void
 *   window.cloudAudit.drain(afterSeq, limit, afterUpdateSeq, updateLimit)
 *                                                     -> {records:[{seq,record}], updates:[{seq,update}],
 *                                                         nextSeq, nextUpdateSeq, hasMore,
 *                                                         droppedRecords, droppedUpdates}
 *   window.cloudAudit.detectEnv() / callFunction(name, data) / scanCloudFunctions()
 *   window.cloudAudit.getHookedCalls() / clearHookedCalls() / getHookedApps()
 *   window.cloudAudit.getDiscoveredFunctions() / stopAutoHook()
 *
 * Two streams share this buffer, both shaped `{seq, <payload>}` (same protocol
 * as core/hooks/wxapi.js):
 *   - records: every call.
 *   - updates: the settled outcome frame of every record, so the shell and the
 *     database learn the final status and the call's duration.
 *
 * Overload is observable (R12): both buffers count the entries they evict, and
 * drain() reports the cumulative counts as droppedRecords/droppedUpdates.
 */
(function() {
  var _global = typeof window !== 'undefined' ? window : (typeof globalThis !== 'undefined' ? globalThis : this);
  if (_global._wxtapCloudAuditReady) return;
  _global._wxtapCloudAuditReady = true;

  // Burst-absorption caps for the two page-side buffers (R13). The value bounds
  // how large a burst the page can hold while the shell is busy, and it is also
  // the page realm's worst-case memory.
  //
  // This value is mirrored on the shell side: desktop/internal/api/ipc/
  // hookfeeder.go's `deliveredCapacity` (the replay-dedup FIFO) *must* stay
  // larger than RECORD_BUFFER_CAPACITY + UPDATE_BUFFER_CAPACITY. After ResetAck
  // the drain re-reads both buffers from seq 0, and the FIFO is what filters
  // that replay. The names stay parseable - the shell parses these constants out
  // of this file instead of hardcoding a second copy (see core/hooks/wxapi.js).
  var RECORD_BUFFER_CAPACITY = 5000;
  var UPDATE_BUFFER_CAPACITY = 5000;
  var DEFAULT_UPDATE_LIMIT = 200;

  var drainState = {
    seq: 0,
    buffer: [],
    updateSeq: 0,
    updates: [],
    // Cumulative evictions on each stream, since the hook was loaded or
    // clearHookedCalls() ran. Reported by drain() so the shell can show an
    // overload instead of silently losing the oldest captures.
    droppedRecords: 0,
    droppedUpdates: 0
  };

  function pushRecord(record) {
    drainState.seq += 1;
    var seq = drainState.seq;
    // rid is the record's stable identity, derived exactly like
    // traffic_records.id (desktop/internal/cloud/drainer.go's Convert, which
    // prefers rid over recomputing it): "<type>-<appId>-<ts>-<seq>". It lets the
    // event stream and the poll path recognise the same capture.
    record.rid = record.type + '-' + (record.appId || '') + '-' + record.ts + '-' + seq;
    drainState.buffer.push({ seq: seq, record: record });
    while (drainState.buffer.length > RECORD_BUFFER_CAPACITY) {
      drainState.buffer.shift();
      drainState.droppedRecords += 1;
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
    drainState.updateSeq += 1;
    drainState.updates.push({ seq: drainState.updateSeq, update: update });
    while (drainState.updates.length > UPDATE_BUFFER_CAPACITY) {
      drainState.updates.shift();
      drainState.droppedUpdates += 1;
    }
  }

  function drainRecords(afterSeq, limit) {
    var out = [];
    var nextSeq = afterSeq;
    var hasMore = false;
    for (var i = 0; i < drainState.buffer.length; i++) {
      var entry = drainState.buffer[i];
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
    for (var i = 0; i < drainState.updates.length; i++) {
      var entry = drainState.updates[i];
      if (entry.seq <= afterUpdateSeq) continue;
      if (out.length >= limit) { hasMore = true; break; }
      out.push(entry);
      nextUpdateSeq = entry.seq;
    }
    return { updates: out, nextUpdateSeq: nextUpdateSeq, hasMore: hasMore };
  }

  function findEntry(seq) {
    for (var i = drainState.buffer.length - 1; i >= 0; i--) {
      if (drainState.buffer[i].seq === seq) return drainState.buffer[i];
    }
    return null;
  }

  function settleBySeq(seq, status, result, error) {
    var entry = findEntry(seq);
    if (!entry) return; // already evicted; nothing to settle
    var r = entry.record;
    // A record settles once: a success callback landing next to a promise
    // resolution must not append a second update frame for the same rid.
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

  var cloudAudit = {
    _hookedClouds: [],   // [{cloud, appId, origMethods}]
    _autoHookTimer: null,
    autoHookEnabled: false,

    // ── 扫描所有 frame，收集所有 wx.cloud 实例 ──
    _findAllFrames: function() {
      var frames = [];
      var seen = [];
      function tryAdd(w) {
        try {
          if (!w || !w.wx) return;
          for (var i = 0; i < seen.length; i++) { if (seen[i] === w) return; }
          seen.push(w);
          frames.push(w);
        } catch(e) {}
      }
      // In appservice context, try _global directly
      tryAdd(_global);
      var sources = [_global];
      try { if (_global.parent && _global.parent !== _global) sources.push(_global.parent); } catch(e) {}
      for (var s = 0; s < sources.length; s++) {
        try {
          var src = sources[s];
          if (src.frames) {
            for (var i = 0; i < src.frames.length; i++) {
              try { tryAdd(src.frames[i]); } catch(e) {}
            }
          }
        } catch(e) {}
      }
      return frames;
    },

    _getAppIdFromFrame: function(frame) {
      try {
        var cfg = frame.__wxConfig || {};
        if (cfg.accountInfo) {
          var ai = cfg.accountInfo;
          if (ai.appAccount && ai.appAccount.appId) return ai.appAccount.appId;
          if (ai.appId) return ai.appId;
        }
        if (cfg.appId) return cfg.appId;
        if (cfg.appid) return cfg.appid;
      } catch(e) {}
      try {
        var info = frame.wx && frame.wx.getAccountInfoSync && frame.wx.getAccountInfoSync();
        var miniProgram = info && info.miniProgram;
        if (miniProgram && miniProgram.appId) return miniProgram.appId;
      } catch(e) {}
      return '';
    },

    _getEnvFromFrame: function(frame) {
      try {
        var cfg = frame.__wxConfig || {};
        if (cfg.envList) return cfg.envList;
        if (cfg.cloud && cfg.cloud.env) return cfg.cloud.env;
        if (frame.wx.cloud && frame.wx.cloud._config) return frame.wx.cloud._config.env;
      } catch(e) {}
      return null;
    },

    // ── 环境探测 (返回所有发现的小程序) ──
    detectEnv: function() {
      var frames = this._findAllFrames();
      if (frames.length === 0) {
        return { ok: false, reason: 'no miniprogram frames found' };
      }
      var apps = [];
      for (var i = 0; i < frames.length; i++) {
        var f = frames[i];
        try {
          if (!f.wx || !f.wx.cloud) continue;
          apps.push({
            appId: this._getAppIdFromFrame(f),
            env: this._getEnvFromFrame(f),
            inited: !!(f.wx.cloud._config || f.wx.cloud.inited)
          });
        } catch(e) {}
      }
      return {
        ok: apps.length > 0,
        reason: apps.length === 0 ? 'wx.cloud not available' : undefined,
        hasCloud: apps.length > 0,
        apps: apps,
        // 兼容旧接口
        appId: apps.length > 0 ? apps[0].appId : '',
        env: apps.length > 0 ? apps[0].env : null,
        inited: apps.length > 0 ? apps[0].inited : false
      };
    },

    // ── 通用工具 ──
    _safeClone: function(obj) {
      try { return JSON.parse(JSON.stringify(obj)); } catch(e) { return String(obj); }
    },

    // `calledAt` is the moment the wrapped API was invoked. The wrappers build
    // their record at the settle point, so without it `ts` (and therefore the
    // update frame's `durationMs = settledAt - ts`) described the callback, not
    // the call: every settled frame reported ~0ms. Callers that already record
    // at call time (the direct wx.cloud hook) omit it and get Date.now().
    _record: function(type, name, appId, data, result, status, error, calledAt) {
      var r = {
        type: type, name: name, appId: appId,
        data: data, timestamp: new Date().toLocaleTimeString(),
        ts: calledAt === undefined ? Date.now() : calledAt,
        status: status || 'pending'
      };
      if (result !== undefined) r.result = result;
      if (error !== undefined) r.error = error;
      var seq = pushRecord(r);
      // Unlike wxapi (which records at call time and settles from the callback),
      // the cloud wrappers build the record at the settle point, so a record
      // carrying a final status is already settled when it is created: emit its
      // update frame here. Every rid then has exactly one frame - records the
      // direct hook created as 'pending' get theirs from settleBySeq - and the
      // shell can patch the row's status/duration without waiting for a second
      // delivery.
      if (r.status !== 'pending') pushUpdate(r);
      return seq;
    },

    // ── 通用 hook 包装器 ──
    _wrapMethod: function(cloud, method, type, nameExtract, appId, origStore) {
      var orig = cloud[method];
      if (!orig || typeof orig !== 'function') return false;
      // Idempotent: the frame scan and the direct wx.cloud hook must never
      // wrap the same method twice, or every call is captured twice.
      if (orig._wxtapHooked) return false;
      origStore[method] = orig.bind(cloud);
      var self = this;
      cloud[method] = function(options) {
        options = options || {};
        var callName = nameExtract ? nameExtract(options) : method;
        var callData = self._safeClone(options);
        delete callData.success; delete callData.fail; delete callData.complete;
        delete callData.filePath; delete callData.tempFilePath;
        // The record is built when the call settles, so the call moment has to
        // be captured here or the settled frame reports a ~0ms duration.
        var calledAt = Date.now();

        var recorded = false;
        var hasCb = !!(options.success || options.fail);
        var origSuccess = options.success;
        var origFail = options.fail;

        options.success = function(res) {
          if (!recorded) { recorded = true; self._record(type, callName, appId, callData, self._safeClone(res), 'success', undefined, calledAt); }
          if (origSuccess) origSuccess(res);
        };
        options.fail = function(err) {
          if (!recorded) { recorded = true; self._record(type, callName, appId, callData, null, 'fail', err ? (err.errMsg || JSON.stringify(err)) : 'unknown', calledAt); }
          if (origFail) origFail(err);
        };

        var ret = origStore[method](options);
        if (!hasCb && ret && typeof ret.then === 'function') {
          ret.then(function(res) {
            if (!recorded) { recorded = true; self._record(type, callName, appId, callData, self._safeClone(res), 'success', undefined, calledAt); }
          })['catch'](function(err) {
            if (!recorded) { recorded = true; self._record(type, callName, appId, callData, null, 'fail', err ? (err.errMsg || JSON.stringify(err)) : 'unknown', calledAt); }
          });
        }
        return ret;
      };
      cloud[method]._wxtapHooked = true;
      return true;
    },

    // ── Hook 数据库 ──
    _hookDatabase: function(cloud, appId, origStore) {
      var origDb = cloud.database;
      if (!origDb) return;
      origStore['database'] = origDb.bind(cloud);
      var self = this;

      cloud.database = function(opts) {
        var db = origStore['database'](opts);
        if (!db) return db;
        self._proxyDbCollection(db, appId);
        return db;
      };
    },

    _proxyDbCollection: function(db, appId) {
      var origCol = db.collection;
      if (!origCol) return;
      // Miniapps re-acquire the database handle per operation; proxying the
      // same instance twice would capture every terminal call twice.
      if (db._wxtapDbProxied) return;
      db._wxtapDbProxied = true;
      var self = this;

      db.collection = function(collName) {
        var col = origCol.call(db, collName);
        if (!col) return col;

        // Hook 终端操作
        ['add', 'get', 'update', 'remove', 'count'].forEach(function(m) {
          if (col[m]) self._wrapTerminal(col, m, 'db.' + m, collName, appId);
        });

        // Hook doc()
        if (col.doc) {
          var origDoc = col.doc.bind(col);
          col.doc = function(docId) {
            var ref = origDoc(docId);
            if (!ref) return ref;
            ['get', 'update', 'set', 'remove'].forEach(function(m) {
              if (ref[m]) self._wrapTerminal(ref, m, 'db.doc.' + m, collName + '/' + docId, appId);
            });
            return ref;
          };
        }

        // Hook where()
        if (col.where) {
          var origWhere = col.where.bind(col);
          col.where = function(cond) {
            var q = origWhere(cond);
            if (!q) return q;
            ['get', 'update', 'remove', 'count'].forEach(function(m) {
              if (q[m]) self._wrapTerminal(q, m, 'db.where.' + m, collName, appId, { where: self._safeClone(cond) });
            });
            return q;
          };
        }

        // Hook aggregate()
        if (col.aggregate) {
          var origAgg = col.aggregate.bind(col);
          col.aggregate = function() {
            var agg = origAgg();
            if (!agg || !agg.end) return agg;
            self._wrapTerminal(agg, 'end', 'db.aggregate', collName, appId);
            return agg;
          };
        }

        return col;
      };
    },

    // 包装一个终端方法（支持 callback + promise）
    _wrapTerminal: function(obj, method, type, name, appId, extraData) {
      if (obj[method] && obj[method]._wxtapHooked) return;
      var orig = obj[method].bind(obj);
      var self = this;
      obj[method] = function(opts) {
        opts = opts || {};
        var callData = extraData ? self._safeClone(extraData) : {};
        if (opts.data) callData.data = self._safeClone(opts.data);
        // Same reason as _wrapMethod: the record is written at the settle
        // point, so the call moment is captured here.
        var calledAt = Date.now();

        var recorded = false;
        var hasCb = !!(opts.success || opts.fail);
        var origSuccess = opts.success;
        var origFail = opts.fail;

        opts.success = function(res) {
          if (!recorded) { recorded = true; self._record(type, name, appId, callData, self._safeClone(res), 'success', undefined, calledAt); }
          if (origSuccess) origSuccess(res);
        };
        opts.fail = function(err) {
          if (!recorded) { recorded = true; self._record(type, name, appId, callData, null, 'fail', err ? (err.errMsg || JSON.stringify(err)) : 'unknown', calledAt); }
          if (origFail) origFail(err);
        };

        var ret = orig(opts);
        if (!hasCb && ret && typeof ret.then === 'function') {
          ret.then(function(res) {
            if (!recorded) { recorded = true; self._record(type, name, appId, callData, self._safeClone(res), 'success', undefined, calledAt); }
          })['catch'](function(err) {
            if (!recorded) { recorded = true; self._record(type, name, appId, callData, null, 'fail', err ? (err.errMsg || JSON.stringify(err)) : 'unknown', calledAt); }
          });
        }
        return ret;
      };
      obj[method]._wxtapHooked = true;
    },

    // ── 已知方法的类型和名称提取器 ──
    _knownMethods: {
      'callFunction':     { type: 'function',  ne: function(o) { return o.name || 'unknown'; } },
      'uploadFile':       { type: 'storage',   ne: function(o) { return 'uploadFile: ' + (o.cloudPath || ''); } },
      'downloadFile':     { type: 'storage',   ne: function(o) { return 'downloadFile: ' + (o.fileID || ''); } },
      'deleteFile':       { type: 'storage',   ne: function(o) { return 'deleteFile(' + (o.fileList||[]).length + ')'; } },
      'getTempFileURL':   { type: 'storage',   ne: function(o) { return 'getTempFileURL(' + (o.fileList||[]).length + ')'; } },
      'callContainer':    { type: 'container', ne: function(o) { return o.path || 'callContainer'; } },
      'connectContainer': { type: 'container', ne: function(o) { return 'connectContainer: ' + (o.service || ''); } }
    },
    _skipProps: { 'init':1, 'database':1, 'CloudID':1, 'constructor':1, 'prototype':1, '__proto__':1 },

    // ── 对一个 cloud 实例安装全部 hook ──
    _hookOneCloud: function(cloud, appId) {
      var origStore = {};
      var hookedList = [];

      // 动态枚举所有方法
      var keys = [];
      try { keys = Object.keys(cloud); } catch(e) {}
      try {
        var proto = Object.getPrototypeOf(cloud);
        if (proto) {
          var pk = Object.getOwnPropertyNames(proto);
          for (var i = 0; i < pk.length; i++) { if (keys.indexOf(pk[i]) === -1) keys.push(pk[i]); }
        }
      } catch(e) {}

      for (var i = 0; i < keys.length; i++) {
        var k = keys[i];
        if (this._skipProps[k] || k.charAt(0) === '_') continue;
        try { if (typeof cloud[k] !== 'function') continue; } catch(e) { continue; }

        var known = this._knownMethods[k];
        var type = known ? known.type : 'cloud';
        var ne = known ? known.ne : (function(name) { return function() { return name; }; })(k);

        if (this._wrapMethod(cloud, k, type, ne, appId, origStore)) {
          hookedList.push(k);
        }
      }

      // 数据库特殊处理
      this._hookDatabase(cloud, appId, origStore);

      this._hookedClouds.push({ cloud: cloud, appId: appId, origMethods: origStore });
      return hookedList;
    },

    // ── 自动扫描所有 frame 并 hook ──
    autoHookScan: function() {
      var frames = this._findAllFrames();
      var newApps = [];
      for (var i = 0; i < frames.length; i++) {
        var f = frames[i];
        try {
          if (!f.wx || !f.wx.cloud) continue;
          var cloud = f.wx.cloud;
          // 检查是否已经 hook 过这个 cloud 实例
          var alreadyHooked = false;
          for (var j = 0; j < this._hookedClouds.length; j++) {
            if (this._hookedClouds[j].cloud === cloud) { alreadyHooked = true; break; }
          }
          if (alreadyHooked) continue;

          var appId = this._getAppIdFromFrame(f);
          var methods = this._hookOneCloud(cloud, appId);
          newApps.push({ appId: appId, methods: methods });
        } catch(e) {}
      }
      return newApps;
    },

    // ── 公开 API ──
    installHook: function() {
      var newApps = this.autoHookScan();
      this._hookBridge();

      // 直接 hook 全局 wx.cloud.callFunction（最可靠的方式）
      this._directHookWxCloud();

      if (this._hookedClouds.length === 0 && !this._bridgeHooked && !this._directHooked) {
        return { ok: false, reason: 'wx.cloud not available in any frame' };
      }
      // 启动自动扫描定时器（每 3 秒扫描新 frame）
      this._startAutoScan();
      return {
        ok: true,
        totalHooked: this._hookedClouds.length,
        directHooked: !!this._directHooked,
        newApps: newApps,
        hookedMethods: newApps.length > 0 ? newApps[0].methods : []
      };
    },

    // 直接 hook wx.cloud 核心方法，不依赖 frame 扫描
    _directHooked: false,
    _directHookWxCloud: function() {
      if (this._directHooked) return;
      var self = this;
      try {
        if (typeof wx === 'undefined' || !wx.cloud || !wx.cloud.callFunction) return;
        var appId = '';
        try {
          var info = wx.getAccountInfoSync && wx.getAccountInfoSync();
          if (info && info.miniProgram) appId = info.miniProgram.appId || '';
        } catch(e) {}
        if (!appId) appId = this._getAppIdFromFrame(_global);

        var _hookOne = function(method, type, nameExtract) {
          if (!wx.cloud[method] || typeof wx.cloud[method] !== 'function') return;
          // The frame scan may already have wrapped this method; wrapping it
          // again would record every call twice.
          if (wx.cloud[method]._wxtapHooked) return;
          var orig = wx.cloud[method];
          wx.cloud[method] = function(options) {
            options = options || {};
            var callName = nameExtract(options);
            var callData = self._safeClone(options);
            delete callData.success; delete callData.fail; delete callData.complete;

            var seq = self._record(type, callName, appId, callData, null, 'pending');

            var origSuccess = options.success;
            var origFail = options.fail;
            if (origSuccess) {
              options.success = function(res) {
                settleBySeq(seq, 'success', self._safeClone(res), undefined);
                origSuccess(res);
              };
            }
            if (origFail) {
              options.fail = function(err) {
                settleBySeq(seq, 'fail', undefined, err ? (err.errMsg || JSON.stringify(err)) : 'unknown');
                origFail(err);
              };
            }

            var ret = orig.call(wx.cloud, options);

            if (ret && typeof ret.then === 'function') {
              ret.then(function(res) {
                settleIfPending(seq, 'success', self._safeClone(res), undefined);
              })['catch'](function(err) {
                settleIfPending(seq, 'fail', undefined, err ? (err.errMsg || JSON.stringify(err)) : 'unknown');
              });
            }
            return ret;
          };
          wx.cloud[method]._wxtapHooked = true;
        };

        _hookOne('callFunction', 'function', function(o) { return o.name || 'unknown'; });
        _hookOne('callContainer', 'container', function(o) { return o.path || 'callContainer'; });
        _hookOne('connectContainer', 'container', function(o) { return 'connectContainer: ' + (o.service || ''); });

        this._directHooked = true;
      } catch(e) {}
    },

    _startAutoScan: function() {
      if (this._autoHookTimer) return;
      this.autoHookEnabled = true;
      var self = this;
      this._autoHookTimer = setInterval(function() {
        if (!self.autoHookEnabled) return;
        self.autoHookScan();
        self._hookBridge();
      }, 3000);
    },

    stopAutoHook: function() {
      this.autoHookEnabled = false;
      if (this._autoHookTimer) {
        clearInterval(this._autoHookTimer);
        this._autoHookTimer = null;
      }
    },

    uninstallHook: function() {
      this.stopAutoHook();
      for (var i = 0; i < this._hookedClouds.length; i++) {
        var entry = this._hookedClouds[i];
        try {
          for (var m in entry.origMethods) {
            try { entry.cloud[m] = entry.origMethods[m]; } catch(e) {}
          }
        } catch(e) {}
      }
      this._hookedClouds = [];
      // Restore the bridge wrapper as well: leaving it installed kept
      // capturing after cloud.stop, and the next start re-drained those
      // stopped-period records as fresh traffic.
      if (this._bridgeTarget) {
        try {
          this._bridgeTarget.bridge.invoke = this._bridgeTarget.origInvoke;
          delete this._bridgeTarget.bridge._cloudAuditHooked;
        } catch(e) {}
        this._bridgeTarget = null;
        this._bridgeHooked = false;
      }
    },

    getHookedCalls: function() {
      var out = [];
      for (var i = 0; i < drainState.buffer.length; i++) {
        out.push(drainState.buffer[i].record);
      }
      return out;
    },
    clearHookedCalls: function() {
      // Both buffers drop their contents; neither sequence rewinds, so an
      // acknowledged consumer still sees everything captured from here on. The
      // drop counters describe what is (no longer) in the buffers, so they
      // restart with them.
      drainState.buffer = [];
      drainState.updates = [];
      drainState.droppedRecords = 0;
      drainState.droppedUpdates = 0;
    },

    getHookedApps: function() {
      var apps = [];
      for (var i = 0; i < this._hookedClouds.length; i++) {
        apps.push(this._hookedClouds[i].appId);
      }
      return apps;
    },

    getDiscoveredFunctions: function() {
      var map = {};
      var calls = this.getHookedCalls();
      for (var i = 0; i < calls.length; i++) {
        var c = calls[i];
        var key = (c.type || 'function') + ':' + c.appId + ':' + c.name;
        if (!map[key]) {
          map[key] = { name: c.name, type: c.type || 'function', appId: c.appId || '', params: [], count: 0 };
        }
        map[key].count++;
        if (c.data && typeof c.data === 'object') {
          var keys = Object.keys(c.data);
          for (var k = 0; k < keys.length; k++) {
            if (map[key].params.indexOf(keys[k]) === -1) map[key].params.push(keys[k]);
          }
        }
      }
      var result = [];
      for (var k in map) { result.push(map[k]); }
      return result;
    },

    // ── 补充扫描 ──
    scanCloudFunctions: function() {
      var frames = this._findAllFrames();
      var found = {};
      for (var fi = 0; fi < frames.length; fi++) {
        var f = frames[fi];
        var appId = this._getAppIdFromFrame(f);
        var appCodes = [];
        try { if (f.__wxAppCode__) appCodes.push(f.__wxAppCode__); } catch(e) {}
        for (var a = 0; a < appCodes.length; a++) {
          var code = appCodes[a];
          for (var key in code) {
            try {
              var val = code[key];
              var src = typeof val === 'string' ? val :
                        (typeof val === 'function' ? val.toString() : null);
              if (src && src.length > 20) {
                if (src.indexOf('callFunction') !== -1) this._extractCalls(src, found, appId);
                if (src.indexOf('.collection(') !== -1) this._extractDbOps(src, found, appId);
                this._extractFileOps(src, found, appId);
              }
            } catch(e) {}
          }
        }
      }
      var result = [];
      for (var key in found) {
        var displayName = key.indexOf(':') === -1 ? key : key.substring(key.indexOf(':') + 1);
        result.push({ name: displayName, type: found[key].type, appId: found[key].appId || '', params: found[key].params, count: found[key].count });
      }
      return result;
    },

    _extractCalls: function(src, found, appId) {
      var re = /callFunction\s*\(\s*\{[^}]{0,500}?name\s*:\s*["']([^"']+)["']/g;
      var m;
      while ((m = re.exec(src)) !== null) {
        var key = 'fn:' + m[1];
        if (!found[key]) found[key] = { type: 'function', appId: appId, params: [], count: 0 };
        found[key].count++;
        var after = src.substring(m.index, Math.min(m.index + 600, src.length));
        var dm = after.match(/data\s*:\s*\{([^}]{1,400})\}/);
        if (dm) {
          var fields = dm[1].match(/(\w+)\s*:/g);
          if (fields) {
            for (var i = 0; i < fields.length; i++) {
              var fn = fields[i].replace(/\s*:$/, '');
              if (fn !== 'name' && fn !== 'success' && fn !== 'fail' && fn !== 'complete'
                  && found[key].params.indexOf(fn) === -1) found[key].params.push(fn);
            }
          }
        }
      }
    },

    _extractDbOps: function(src, found, appId) {
      var re = /\.collection\s*\(\s*["']([^"']+)["']\s*\)/g;
      var m;
      while ((m = re.exec(src)) !== null) {
        var key = 'db:' + m[1];
        if (!found[key]) found[key] = { type: 'database', appId: appId, params: [], count: 0 };
        found[key].count++;
        var after = src.substring(m.index, Math.min(m.index + 300, src.length));
        ['add','get','update','remove','count','aggregate','doc','where'].forEach(function(op) {
          if (after.indexOf('.' + op + '(') !== -1 && found[key].params.indexOf(op) === -1)
            found[key].params.push(op);
        });
      }
    },

    _extractFileOps: function(src, found, appId) {
      ['uploadFile','downloadFile','deleteFile','getTempFileURL'].forEach(function(m) {
        if (src.indexOf(m) !== -1) {
          var key = 'storage:' + m;
          if (!found[key]) found[key] = { type: 'storage', appId: appId, params: [], count: 0 };
          found[key].count++;
        }
      });
    },

    // ── 手动调用云函数 ──
    callFunction: function(name, data) {
      // 从已 hook 的 cloud 中找一个可用的 callFunction
      var caller = null;
      for (var i = 0; i < this._hookedClouds.length; i++) {
        var entry = this._hookedClouds[i];
        if (entry.origMethods['callFunction']) {
          caller = entry.origMethods['callFunction'];
          break;
        }
      }
      if (!caller) {
        // 没 hook 过则尝试直接找
        var frames = this._findAllFrames();
        for (var i = 0; i < frames.length; i++) {
          try {
            if (frames[i].wx && frames[i].wx.cloud && frames[i].wx.cloud.callFunction) {
              caller = frames[i].wx.cloud.callFunction.bind(frames[i].wx.cloud);
              break;
            }
          } catch(e) {}
        }
      }
      if (!caller) return Promise.resolve({ ok: false, reason: 'wx.cloud not available' });

      return new Promise(function(resolve) {
        var t = setTimeout(function() {
          resolve({ ok: true, status: 'timeout', error: '调用超时(10s)' });
        }, 10000);
        caller({
          name: name, data: data || {},
          success: function(res) {
            clearTimeout(t);
            try { resolve({ ok: true, status: 'success', result: JSON.parse(JSON.stringify(res && res.result ? res.result : res)) }); }
            catch(e) { resolve({ ok: true, status: 'success', result: String(res) }); }
          },
          fail: function(err) {
            clearTimeout(t);
            resolve({ ok: true, status: 'fail', error: err ? (err.errMsg || JSON.stringify(err)) : 'unknown' });
          }
        });
      });
    },

    // ── Hook WeixinJSBridge.invoke 捕获底层 operateWXData 调用 ──
    _bridgeHooked: false,
    _bridgeTarget: null,
    _hookBridge: function() {
      if (this._bridgeHooked) return;
      var self = this;
      var frames = this._findAllFrames();
      for (var fi = 0; fi < frames.length; fi++) {
        var f = frames[fi];
        try {
          var bridge = f.WeixinJSBridge;
          if (!bridge || !bridge.invoke || bridge._cloudAuditHooked) continue;
          bridge._cloudAuditHooked = true;
          var origInvoke = bridge.invoke;
          this._bridgeTarget = { bridge: bridge, origInvoke: origInvoke };
          bridge.invoke = function(method, params, callback) {
            if (method === 'operateWXData' || method === 'operateCloudFunction') {
              var callData = self._safeClone(params || {});
              var appId = callData.app_id || callData.appId || '';
              var apiName = '';
              try {
                var d = typeof callData.data === 'string' ? JSON.parse(callData.data) : callData.data;
                apiName = d.api_name || d.name || method;
              } catch(e) { apiName = method; }

              // The bridge records when the platform answers, so the invoke
              // moment is what the frame's duration has to be measured from.
              var calledAt = Date.now();
              var wrappedCb = function(res) {
                self._record('function', apiName, appId, callData, self._safeClone(res), 'success', undefined, calledAt);
                if (callback) callback(res);
              };
              return origInvoke.call(bridge, method, params, wrappedCb);
            }
            return origInvoke.call(bridge, method, params, callback);
          };
          this._bridgeHooked = true;
        } catch(e) {}
      }
    }
  };

  // seq+drain surface for the Core's hook.drain
  cloudAudit.drain = function(afterSeq, limit, afterUpdateSeq, updateLimit) {
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
      droppedRecords: drainState.droppedRecords,
      droppedUpdates: drainState.droppedUpdates
    };
  };
  // Preferred install name; installHook is an alternate name for install
  cloudAudit.install = cloudAudit.installHook;

  _global.cloudAudit = cloudAudit;
  // Also assign window.cloudAudit when window differs from the global
  if (typeof window !== 'undefined' && window !== _global) {
    window.cloudAudit = cloudAudit;
  }
})();
