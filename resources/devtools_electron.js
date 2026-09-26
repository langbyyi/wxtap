const { app, BrowserWindow } = require('electron')

// 设置中文语言
app.commandLine.appendSwitch('lang', 'zh-CN')

// argv[2] 是 devtools://devtools/bundled/inspector.html?ws=127.0.0.1:<端口>
const url = process.argv[2]
if (!url || !url.startsWith('devtools://')) {
  console.error('DevTools 地址无效')
  process.exit(1)
}

app.whenReady().then(() => {
  const win = new BrowserWindow({
    width: 1200,
    height: 800,
    title: 'DevTools',
    show: true,
    backgroundColor: '#ffffff',
    webPreferences: {
      nodeIntegration: false,
      contextIsolation: true,
      sandbox: false,
    },
  })

  win.loadURL(url)
  win.webContents.on('did-fail-load', (_event, _code, desc, failedURL) => {
    if (failedURL === url) win.setTitle(`DevTools 没有打开：${desc}`)
  })
  win.setMenuBarVisibility(false)

  // Cmd+R / Ctrl+R / F5 = 重新连接（重新加载 URL）
  win.webContents.on('before-input-event', (event, input) => {
    if ((input.meta || input.control) && input.key === 'r') {
      event.preventDefault()
      win.loadURL(url)
    }
    if (input.key === 'F5') {
      event.preventDefault()
      win.loadURL(url)
    }
  })

  win.on('closed', () => {
    app.quit()
  })
})

app.on('window-all-closed', () => {
  app.quit()
})
