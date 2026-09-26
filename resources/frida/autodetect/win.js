"use strict";

// Runs before the main hook. It intentionally emits only offsets and never
// serializes module bytes, process paths, or scanned string contents.
//
// Every scan is queued as one scanSync task drained through chained
// setTimeout: script.load() must return fast (Frida aborts the load ack after
// ~30s, leaving the agent stuck mid-scan and every later attach timing out),
// and between tasks the agent thread stays responsive to unload and messages.
//
// Detection strategy (verified against a 25560 file-image analysis):
// - OnLoadStart: its body lea's the compiler signature string
//   "virtual void applet::AppletIndexContainer::OnLoadStart(bool…" and the
//   build-machine source path of applet_index_container.cc; the intersection
//   of both reference sets is exactly one .pdata function.
// - CDPFilter: no string anchor reaches it. The single function that lea's
//   "SendToClientFilter" calls it, so its candidates are that caller's E8
//   call targets that are .pdata entries with exactly one lea of their own.
//   The hook keeps all candidates; its runtime guard (patch only when the
//   value at [arg]+8 equals 6) makes wrong candidates inert.
(function () {
  var attempts = 0;
  var timer = null;
  var queue = [];
  var taskTotal = 0;
  var taskDone = 0;
  var maxScanBytes = 8 * 1024 * 1024;

  var module = null;
  var sections = null;
  var functions = null;
  var anchors = null;
  var attributed = null;
  var refIndex = null;
  var candidates = null;

  var finish = function (payload) {
    if (timer !== null) clearTimeout(timer);
    timer = null;
    send(payload);
  };
  var schedule = function (delay) { timer = setTimeout(pump, delay); };
  var fail = function (message) { finish({ type: "wmpf-offsets-error", error: message }); };

  var hex = function (s) {
    return Array.prototype.map.call(s, function (c) {
      return ("0" + c.charCodeAt(0).toString(16)).slice(-2);
    }).join(" ");
  };
  var scan = function (section, pattern) {
    var hits = [];
    // enumerateSections() exposes {name, address, size}; "base" belongs to
    // enumerateRanges() and would silently fail every scan via the catch.
    try {
      Memory.scanSync(section.address, section.size, pattern).forEach(function (hit) {
        hits.push(hit.address);
      });
    } catch (_) {}
    return hits;
  };
  // Memory.scanSync is synchronous. Split large PE regions so an unknown
  // build cannot monopolize the Frida agent long enough for the host timeout
  // to fire before later progress messages are delivered.
  var enqueueScan = function (region, pattern, onHit) {
    var address = region.address;
    var remaining = region.size;
    while (remaining > 0) {
      var size = Math.min(remaining, maxScanBytes);
      (function (chunkAddress, chunkSize) {
        queue.push(function () {
          scan({ address: chunkAddress, size: chunkSize }, pattern).forEach(onHit);
        });
      })(address, size);
      address = address.add(size);
      remaining -= size;
    }
  };

  var ensureModule = function () {
    module = Process.findModuleByName("flue.dll");
    if (module) return true;
    if (++attempts < 120) { schedule(500); return false; }
    fail("flue.dll 未加载；请先在微信中打开小程序");
    return false;
  };

  var prepare = function () {
    sections = module.enumerateSections();
    functions = [];
    var pdata = sections.filter(function (s) { return s.name === ".pdata"; })[0];
    if (pdata) {
      var bytes = pdata.address.readByteArray(pdata.size);
      if (bytes) {
        var view = new DataView(bytes);
        for (var i = 0; i + 12 <= view.byteLength; i += 12) {
          var begin = view.getUint32(i, true), end = view.getUint32(i + 4, true);
          if (begin && begin < end && end <= module.size) functions.push([begin, end]);
        }
      }
    }
    attributed = {};
  };

  var functionRange = function (entry) {
    for (var i = 0; i < functions.length; i++) {
      if (functions[i][0] === entry) return functions[i];
    }
    return null;
  };

  var functionEntry = function (address) {
    var offset = address.sub(module.base).toUInt32();
    for (var i = 0; i < functions.length; i++) {
      if (offset >= functions[i][0] && offset < functions[i][1]) return functions[i][0];
    }
    return null;
  };

  var entriesFor = function (name) {
    return Object.keys(attributed)
      .filter(function (key) { return attributed[key].names[name]; })
      .map(function (key) { return attributed[key].entry; });
  };

  // `address` is a match address already unpacked by scan().
  var attributeLea = function (address, form) {
    var target;
    try {
      target = address.add(form.end).add(address.add(form.dispAt).readS32());
    } catch (_) {
      return;
    }
    var name = refIndex[target.toString()];
    if (name === undefined) return;
    var entry = functionEntry(address);
    if (entry === null) return;
    var key = entry.toString();
    if (attributed[key] === undefined) attributed[key] = { entry: entry, names: {} };
    attributed[key].names[name] = true;
  };

  var buildQueue = function () {
    // Needles must match the FULL literal start: the code lea's the beginning
    // of each string, and a mid-string hit would never equal the lea target.
    anchors = [
      { name: "loadFile", needle: "..\\..\\flue\\browser\\applet\\applet_index_container.cc", refs: [] },
      { name: "loadName", needle: "virtual void applet::AppletIndexContainer::OnLoadStart(bool", refs: [] },
      { name: "filterName", needle: "SendToClientFilter", refs: [] }
    ];
    var nonText = sections.filter(function (s) { return s.name !== ".text" && s.name !== ".pdata"; });
    // Anchor-string location is split into bounded scan tasks.
    nonText.forEach(function (section) {
      anchors.forEach(function (anchor) {
        enqueueScan(section, hex(anchor.needle), function (hit) { anchor.refs.push(hit); });
      });
    });
    // After location completes, index refs by address string: LEA matching
    // then costs one lookup per .text hit instead of a scan per anchor
    // (hundreds of thousands of hits flow through attributeLea).
    queue.push(function () {
      refIndex = {};
      anchors.forEach(function (anchor) {
        anchor.refs.forEach(function (ref) {
          refIndex[ref.toString()] = anchor.name;
        });
      });
    });
    // One text pass serves every anchor: scan the fixed bytes `8d <modrm>` of
    // every rip-relative `lea` (this Frida build rejects "??" wildcard
    // patterns) and attribute each reference to the owning .pdata function.
    // Scanning from the 8d opcode (disp at +2, rip at +6 relative to it)
    // covers ALL REX variants at once: 48/4c/44-8d prefixes sit before the
    // opcode, so rip == opcode+6 for every encoding. Random "8d xx" byte
    // pairs decode to junk targets that just miss the ref index.
    var modrmRip = ["05", "0d", "15", "1d", "25", "2d", "35", "3d"];
    var leaForms = modrmRip.map(function (m) { return { pattern: "8d " + m, dispAt: 2, end: 6 }; });
    module.enumerateRanges("r-x").forEach(function (range) {
      leaForms.forEach(function (form) {
        // enumerateRanges() entries do carry {base, size} (unlike sections).
        enqueueScan({ address: range.base, size: range.size }, form.pattern, function (hit) {
          attributeLea(hit, form);
        });
      });
    });
    // Candidate discovery for CDPFilter (single synchronous pass over the one
    // SendToClientFilter referrer's body — a few KB, cheap after the .text
    // scans): its E8 call targets that are .pdata entries owning exactly one
    // rip-relative lea of their own.
    queue.push(function () {
      candidates = [];
      var filterCallers = entriesFor("filterName");
      if (filterCallers.length !== 1) return; // complete() reports the failure
      var range = functionRange(filterCallers[0]);
      if (range === null) return;
      var bytes = module.base.add(range[0]).readByteArray(range[1] - range[0]);
      if (!bytes) return;
      var view = new DataView(bytes);
      var base = range[0];
      var targets = [];
      for (var i = 0; i + 5 <= view.byteLength; i++) {
        if (view.getUint8(i) !== 0xe8) continue;
        var target = base + i + 5 + view.getInt32(i + 1, true);
        // The referrer may call the same target several times; keep each
        // entry once so the hook does not attach twice to one offset.
        if (functionRange(target) !== null && targets.indexOf(target) === -1) targets.push(target);
      }
      // Keep entries whose body decodes exactly one rip-relative lea; random
      // E8 matches landing mid-function are already dropped by functionRange.
      targets.forEach(function (entry) {
        var fr = functionRange(entry);
        var body = module.base.add(fr[0]).readByteArray(fr[1] - fr[0]);
        if (!body) return;
        var bv = new DataView(body);
        var leaCount = 0;
        for (var j = 0; j + 6 <= bv.byteLength; j++) {
          if (bv.getUint8(j) !== 0x8d) continue;
          if ((bv.getUint8(j + 1) & 0xc7) !== 0x05) continue;
          leaCount += 1;
        }
        if (leaCount === 1) candidates.push(entry);
      });
    });
    taskTotal = queue.length;
  };

  var complete = function () {
    var loadFile = entriesFor("loadFile");
    var loadName = entriesFor("loadName");
    var intersect = function (left, right) { return left.filter(function (value) { return right.indexOf(value) !== -1; }); };
    var load = intersect(loadFile, loadName);
    if (load.length !== 1) {
      fail("non-unique load anchors: loadFile=" + loadFile.length + ", loadName=" + loadName.length + ", intersect=" + load.length);
      return;
    }
    if (candidates === null || candidates.length === 0) {
      fail("no CDPFilter candidates from the SendToClientFilter referrer");
      return;
    }
    // New WMPF builds use this stable six-hop layout. The Node side validates
    // every value and refuses to hook if the detector does not produce it.
    finish({ type: "wmpf-offsets", config: { loadStart: load[0], cdpFilterCandidates: candidates, sceneOffsets: [64, 1536, 8, 1472, 16, 456] }, moduleSize: module.size });
  };

  var pump = function () {
    if (module === null) {
      // First run (and retries while flue.dll is not loaded yet): resolve the
      // module, then build the task queue once.
      if (!ensureModule()) return; // ensureModule scheduled the retry or failed
      try {
        prepare();
        buildQueue();
      } catch (e) {
        fail("检测脚本异常：" + (e && e.message ? e.message : String(e)));
        return;
      }
    }
    if (queue.length === 0) { complete(); return; }
    var task = queue.shift();
    try {
      task();
    } catch (e) {
      fail("检测脚本异常：" + (e && e.message ? e.message : String(e)));
      return;
    }
    taskDone++;
    // Progress lets the host log liveness; the Node side ignores unknown
    // payload types, so this stays compatible.
    if (taskDone === 1 || taskDone % 10 === 0) {
      send({ type: "wmpf-offsets-progress", done: taskDone, total: taskTotal });
    }
    schedule(0);
  };

  schedule(0);
})();
