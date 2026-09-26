/*
 * WxTap Core navigator hook.
 *
 * Injectable IIFE exposing window.nav. The re-inject guard supports switching
 * miniprograms. The navigator is a control hook (page config, navigation,
 * redirect guard) — it has no record stream, so hook.drain rejects it on the
 * Core side. Request capture is owned by the wxapi hook.
 *
 * Exposed API (invoked through CDP Runtime.evaluate):
 *   window.nav.allPages / tabBarPages / config / wxFrame
 *   window.nav.goTo(url) / switchTo(url) / _safeNavigate(url) / back(delta) / current()
 *   window.nav.pageStack()
 *   window.nav.enableRedirectGuard() / disableRedirectGuard()
 *   window.nav.getBlockedRedirects() / isRedirectGuardOn()
 */
(function() {
  // 防跳转拦截记录的上限：循环触发的强制跳转不会无限吃内存/拖垮回传。
  var REDIRECT_LOG_MAX = 200;
  // 允许重新注入以支持切换小程序
  if (window._wxtapNavReady && window.nav && window.nav.wxFrame) {
    // 检测 wxFrame 是否仍有效
    try {
      var testConfig = window.nav.wxFrame.__wxConfig;
      if (testConfig && testConfig.pages) return; // 仍有效，跳过
    } catch(e) {}
  }
  window._wxtapNavReady = true;

  class WxTapNavigator {
    constructor() {
      this.wxFrame = null;
      this.config = null;
      this.allPages = [];
      this.tabBarPages = [];
      this._redirectGuard = false;
      this._blockedRedirects = [];
      this.init();
    }

    init() {
      if (!this.detectMiniProgramEnvironment()) {
        // 抛出去让 install 失败：留一个 wxFrame=null 的 window.nav 只会让之后每个
        // 导航调用在页面里抛出难懂的 TypeError，而 Go/界面还以为 hook 装好了。
        throw new Error('未检测到小程序环境：找不到持有 wx 的页面 frame');
      }
      this.loadConfiguration();
    }

    detectMiniProgramEnvironment() {
      if (typeof wx !== 'undefined' && typeof getCurrentPages !== 'undefined') {
        this.wxFrame = window;
        return true;
      }
      if (typeof window !== 'undefined' && window.frames) {
        for (let i = 0; i < window.frames.length; i++) {
          try {
            const frame = window.frames[i];
            if (frame.wx && frame.__wxConfig) {
              this.wxFrame = frame;
              return true;
            }
          } catch (e) {}
        }
      }
      try {
        if (window.parent && window.parent.frames) {
          for (let i = 0; i < window.parent.frames.length; i++) {
            try {
              const frame = window.parent.frames[i];
              if (frame.wx && frame.__wxConfig) {
                this.wxFrame = frame;
                return true;
              }
            } catch (e) {}
          }
        }
      } catch (e) {}
      return false;
    }

    loadConfiguration() {
      this.config = this.wxFrame.__wxConfig;
      this.allPages = [].concat(this.config.pages || []);

      // 补充分包页面
      var subPkgs = this.config.subPackages || this.config.subpackages || [];
      var allPages = this.allPages;
      subPkgs.forEach(function(pkg) {
        (pkg.pages || []).forEach(function(page) {
          var fullPath = pkg.root + '/' + page;
          if (allPages.indexOf(fullPath) === -1) {
            allPages.push(fullPath);
          }
        });
      });

      // 去重
      var seen = {};
      this.allPages = this.allPages.filter(function(p) {
        if (seen[p]) return false;
        seen[p] = true;
        return true;
      });

      if (this.config.tabBar && this.config.tabBar.list) {
        this.tabBarPages = this.config.tabBar.list.map(function(tab) {
          return tab.pagePath.replace('.html', '');
        });
      }
    }

    // 获取原始导航方法（绕过防跳转 hook）
    _getNav(method) {
      if (this._redirectGuard) {
        if (method === 'redirectTo' && this._origRedirectTo) return this._origRedirectTo;
        if (method === 'reLaunch' && this._origReLaunch) return this._origReLaunch;
        if (method === 'navigateTo' && this._origNavigateTo) return this._origNavigateTo;
        // 工具自己的跳转/后退不能被自己的守卫拦下
        if (method === 'switchTab' && this._origSwitchTab) return this._origSwitchTab;
        if (method === 'navigateBack' && this._origNavigateBack) return this._origNavigateBack;
      }
      return this.wxFrame.wx[method];
    }

    // 导航动作一律返回 Promise<{ok, err}>：wx 的 success/fail 是回调，只有等它落定
    // 才知道到底跳没跳成。以前失败被吞掉，界面照样提示「已跳转」，自动遍历也永远
    // 计不到失败数。Go 侧用 runtime.evaluate 的 awaitPromise 取这个结果。
    _navPromise(run) {
      return new Promise(function(resolve) {
        var settled = false;
        function settle(ok, err) {
          if (settled) return;
          settled = true;
          resolve({ ok: ok, err: err || '' });
        }
        try {
          run(function(err) { settle(false, err); }, function() { settle(true, ''); });
        } catch (e) {
          settle(false, e && e.message ? e.message : String(e));
        }
      });
    }

    goTo(url) {
      var self = this;
      var isTabBar = this.tabBarPages.some(function(page) {
        return page === url || page === url.replace('/', '') || ('/' + page) === url;
      });
      return this._navPromise(function(onFail, onSuccess) {
        var options = {
          url: url.startsWith('/') ? url : '/' + url,
          success: onSuccess,
          fail: function(e) { onFail((e && e.errMsg) || 'navigateTo failed'); }
        };
        if (isTabBar) {
          // 走 _getNav：切 tab 也属于「工具自己的跳转」，守卫开启时必须绕过它，
          // 否则会被自己拦下——守卫的假 success 会让这里报 ok 而页面根本没动。
          self._getNav('switchTab').call(self.wxFrame.wx, options);
        } else {
          self._getNav('navigateTo').call(self.wxFrame.wx, options);
        }
      });
    }

    // 显式切 tab：goTo 按 tabBar 列表自行判断，switchTo 则是调用方明确要求
    // switchTab。目标不在 tabBar 里时 wx 会回 fail，必须如实报错，而不是退化
    // 成 navigateTo——那是一次语义不同的导航。
    switchTo(route) {
      var self = this;
      return this._navPromise(function(onFail, onSuccess) {
        self._getNav('switchTab').call(self.wxFrame.wx, {
          url: route.startsWith('/') ? route : '/' + route,
          success: onSuccess,
          fail: function(e) { onFail((e && e.errMsg) || 'switchTab failed'); }
        });
      });
    }

    _safeNavigate(pageUrl) {
      var self = this;
      return this._navPromise(function(onFail, onSuccess) {
        var url = pageUrl.startsWith('/') ? pageUrl : '/' + pageUrl;
        self._getNav('reLaunch').call(self.wxFrame.wx, {
          url: url,
          success: onSuccess,
          fail: function(first) {
            self._getNav('switchTab').call(self.wxFrame.wx, {
              url: url,
              success: onSuccess,
              fail: function(second) {
                self._getNav('redirectTo').call(self.wxFrame.wx, {
                  url: url,
                  success: onSuccess,
                  fail: function(third) {
                    onFail('reLaunch/switchTab/redirectTo 全部失败：' + ((third && third.errMsg) || (second && second.errMsg) || (first && first.errMsg) || ''));
                  }
                });
              }
            });
          }
        });
      });
    }

    redirectTo(route) {
      var self = this;
      return this._navPromise(function(onFail, onSuccess) {
        // 走 _getNav：防跳转拦的是页面自己的调用，我们自己的导航必须绕过它，
        // 否则开启拦截后连「返回/重定向」都会被自己的守卫吃掉。
        self._getNav('redirectTo').call(self.wxFrame.wx, {
          url: route.startsWith('/') ? route : '/' + route,
          success: onSuccess,
          fail: function(e) { onFail((e && e.errMsg) || 'redirectTo failed'); }
        });
      });
    }

    back(delta) {
      var self = this;
      delta = delta || 1;
      return this._navPromise(function(onFail, onSuccess) {
        // 走 _getNav 拿原始方法：守卫开启时工具自己的后退不能被自己拦截
        self._getNav('navigateBack').call(self.wxFrame.wx, {
          delta: delta,
          success: onSuccess,
          fail: function(e) { onFail((e && e.errMsg) || 'navigateBack failed'); }
        });
      });
    }

    // 刷新当前页：带查询参数 reLaunch，失败降级 redirectTo。原来这段 IIFE 写在 Go 里
    // 且不等回调，失败被当成成功。
    refreshPage() {
      var self = this;
      var refreshed = '';
      return this._navPromise(function(onFail, onSuccess) {
        var frame = self.wxFrame;
        if (!frame || !frame.getCurrentPages) { onFail('no nav'); return; }
        var pages = frame.getCurrentPages();
        if (!pages || !pages.length) { onFail('no page'); return; }
        var cur = pages[pages.length - 1];
        var route = cur.route || cur.__route__ || '';
        if (!route) { onFail('no route'); return; }
        refreshed = route;
        var url = '/' + route;
        var opts = cur.options || {};
        var qs = Object.keys(opts).map(function(k) { return k + '=' + opts[k]; }).join('&');
        if (qs) url += '?' + qs;
        frame.wx.reLaunch({
          url: url,
          success: function() { onSuccess(); },
          fail: function() {
            frame.wx.redirectTo({
              url: url,
              success: function() { onSuccess(); },
              fail: function(e) { onFail((e && e.errMsg) || 'reLaunch/redirectTo failed'); }
            });
          }
        });
      }).then(function(result) {
        result.route = refreshed;
        return result;
      });
    }

    current() {
      try {
        if (this.wxFrame.getCurrentPages) {
          var pages = this.wxFrame.getCurrentPages();
          if (pages.length > 0) {
            var cur = pages[pages.length - 1];
            return cur.route || cur.__route__ || '';
          }
        }
        return '';
      } catch (e) {
        return '';
      }
    }

    // 运行时页面栈：getCurrentPages() 每一层的路由与查询参数。
    // 与 allPages（配置里的页面全集）不同，这里只有真正入栈的页面。
    pageStack() {
      try {
        if (!this.wxFrame || !this.wxFrame.getCurrentPages) return [];
        var pages = this.wxFrame.getCurrentPages() || [];
        var out = [];
        for (var i = 0; i < pages.length; i++) {
          var page = pages[i] || {};
          out.push({
            route: page.route || page.__route__ || '',
            params: this._queryOf(page.options)
          });
        }
        return out;
      } catch (e) {
        return [];
      }
    }

    // options 里的值只保留可直接展示的标量：页面对象上的复杂值序列化后
    // 既撑大 drain 载荷，也没有展示价值。
    _queryOf(options) {
      var out = {};
      if (!options || typeof options !== 'object') return out;
      var keys = Object.keys(options);
      for (var i = 0; i < keys.length && i < 32; i++) {
        var value = options[keys[i]];
        var kind = typeof value;
        if (kind === 'string' || kind === 'number' || kind === 'boolean') {
          out[keys[i]] = String(value);
        }
      }
      return out;
    }

    enableRedirectGuard() {
      if (this._redirectGuard) return {ok:true, already:true};
      this._redirectGuard = true;
      this._blockedRedirects = [];
      var wx = this.wxFrame.wx;
      var self = this;
      this._origRedirectTo = wx.redirectTo;
      this._origReLaunch = wx.reLaunch;
      this._origNavigateTo = wx.navigateTo;
      this._origSwitchTab = wx.switchTab;
      this._origNavigateBack = wx.navigateBack;
      // 拦截记录要有界：被守卫拦下的页面若在循环里反复调 wx.redirectTo
      //（跳转死循环正是这个守卫的目标场景），无限追加会拖垮 getBlockedRedirects
      // 的一次性全量回传。保留最新的 REDIRECT_LOG_MAX 条。
      var pushBlocked = function(entry) {
        var log = self._blockedRedirects;
        if (log.length >= REDIRECT_LOG_MAX) log.shift();
        log.push(entry);
      };
      // hook redirectTo — 拦截强制跳转，调用 success 防止页面卡死
      wx.redirectTo = function(options) {
        var url = (options && options.url) || '';
        pushBlocked({type:'redirectTo', url:url, time:new Date().toLocaleTimeString()});
        console.warn('[防跳转] 已拦截 redirectTo:', url);
        if (options && options.success) options.success({errMsg:'redirectTo:ok'});
        if (options && options.complete) options.complete({errMsg:'redirectTo:ok'});
      };
      // hook reLaunch — 拦截强制重启
      wx.reLaunch = function(options) {
        var url = (options && options.url) || '';
        pushBlocked({type:'reLaunch', url:url, time:new Date().toLocaleTimeString()});
        console.warn('[防跳转] 已拦截 reLaunch:', url);
        if (options && options.success) options.success({errMsg:'reLaunch:ok'});
        if (options && options.complete) options.complete({errMsg:'reLaunch:ok'});
      };
      // hook navigateTo — 拦截跳转新页面
      wx.navigateTo = function(options) {
        var url = (options && options.url) || '';
        pushBlocked({type:'navigateTo', url:url, time:new Date().toLocaleTimeString()});
        console.warn('[防跳转] 已拦截 navigateTo:', url);
        if (options && options.success) options.success({errMsg:'navigateTo:ok'});
        if (options && options.complete) options.complete({errMsg:'navigateTo:ok'});
      };
      // hook switchTab — 拦截切 tab：强制踢回首页/主 tab 的常用手段，漏掉它
      // 守卫对最常见的强制跳转是透明的
      wx.switchTab = function(options) {
        var url = (options && options.url) || '';
        pushBlocked({type:'switchTab', url:url, time:new Date().toLocaleTimeString()});
        console.warn('[防跳转] 已拦截 switchTab:', url);
        if (options && options.success) options.success({errMsg:'switchTab:ok'});
        if (options && options.complete) options.complete({errMsg:'switchTab:ok'});
      };
      // hook navigateBack — 拦截程序化后退：登录页把人"退"出去同样是无授权跳转
      wx.navigateBack = function(options) {
        pushBlocked({type:'navigateBack', url:'', time:new Date().toLocaleTimeString()});
        console.warn('[防跳转] 已拦截 navigateBack');
        if (options && options.success) options.success({errMsg:'navigateBack:ok'});
        if (options && options.complete) options.complete({errMsg:'navigateBack:ok'});
      };
      return {ok:true};
    }

    disableRedirectGuard() {
      if (!this._redirectGuard) return;
      this._redirectGuard = false;
      var wx = this.wxFrame.wx;
      if (this._origRedirectTo) wx.redirectTo = this._origRedirectTo;
      if (this._origReLaunch) wx.reLaunch = this._origReLaunch;
      if (this._origNavigateTo) wx.navigateTo = this._origNavigateTo;
      if (this._origSwitchTab) wx.switchTab = this._origSwitchTab;
      if (this._origNavigateBack) wx.navigateBack = this._origNavigateBack;
      this._origRedirectTo = null;
      this._origReLaunch = null;
      this._origNavigateTo = null;
      this._origSwitchTab = null;
      this._origNavigateBack = null;
    }

    getBlockedRedirects() {
      return this._blockedRedirects || [];
    }

    isRedirectGuardOn() {
      return !!this._redirectGuard;
    }
  }

  try {
    window.nav = new WxTapNavigator();
  } catch (e) {
    console.error('Navigator init failed:', e.message);
    throw e;
  }
})();
