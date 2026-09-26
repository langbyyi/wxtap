package ipc

// VConsoleExpression builds the JS that toggles wx.setEnableDebug (the
// vConsole debug panel) through the navigator hook's frame handle. The
// expression returns a Promise that settles with the page-side verdict, so
// the caller can tell "setEnableDebug succeeded" from "the call was refused
// or nothing was there to call": 发起调用即返回 ok 的旧写法会在引擎未就绪时
// 谎报成功。The shape matches engine.vconsole.
func VConsoleExpression(enable bool) string {
	val := "false"
	if enable {
		val = "true"
	}
	return "(function(){return new Promise(function(resolve){try{var nav=window.nav;if(!nav||!nav.wxFrame||!nav.wxFrame.wx){resolve(JSON.stringify({err:'no wxFrame'}));return}nav.wxFrame.wx.setEnableDebug({enableDebug:" + val + ",success:function(){resolve(JSON.stringify({ok:true}))},fail:function(e){resolve(JSON.stringify({err:(e&&e.errMsg)||String(e)}))}})}catch(e){resolve(JSON.stringify({err:e.message}))}})})()"
}
