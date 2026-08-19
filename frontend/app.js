(() => {
  const form = document.querySelector('#config-form')
  const save = document.querySelector('#save')
  const state = document.querySelector('#state')
  const message = document.querySelector('#message')
  const pid = document.querySelector('#pid')
  const key = document.querySelector('#key')
  const gateway = document.querySelector('#gateway_url')
  const paymentType = document.querySelector('#payment_type')

  const pluginID = decodeURIComponent(location.pathname.split('/plugins/')[1]?.split('/')[0] || 'epay')
  const endpoint = `/api/admin/plugins/${encodeURIComponent(pluginID)}/frontend-config`

  function csrf() {
    const item = document.cookie.split('; ').find((part) => part.startsWith('levis_csrf='))
    return item ? decodeURIComponent(item.slice('levis_csrf='.length)) : ''
  }

  function show(text, kind) {
    message.textContent = text || ''
    message.className = `message ${kind || ''}`
  }

  async function request(url, options) {
    const response = await fetch(url, {
      credentials: 'same-origin',
      headers: { Accept: 'application/json', ...(options?.body ? { 'Content-Type': 'application/json' } : {}), ...(options?.headers || {}) },
      ...options,
    })
    const body = await response.json().catch(() => ({}))
    if (!response.ok) throw new Error(body.message || '请求失败')
    return body.data ?? body
  }

  async function load() {
    try {
      const data = await request(endpoint)
      pid.value = data.pid || ''
      gateway.value = data.gateway_url || 'https://dash.natriumgroup.com'
      paymentType.value = data.payment_type || 'alipay'
      key.placeholder = data.key_set ? '已保存，留空则保留当前密钥' : '请输入商户密钥'
      state.textContent = data.key_set ? '已配置密钥' : '待配置'
      state.dataset.ready = data.key_set ? 'true' : 'false'
    } catch (error) {
      state.textContent = '读取失败'
      show(error.message || '无法读取配置', 'error')
    }
  }

  form.addEventListener('submit', async (event) => {
    event.preventDefault()
    save.disabled = true
    show('')
    try {
      await request(endpoint, {
        method: 'PUT',
        headers: { 'X-CSRF-Token': csrf() },
        body: JSON.stringify({ values: {
          pid: pid.value.trim(),
          key: key.value,
          gateway_url: gateway.value.trim(),
          payment_type: paymentType.value,
        } }),
      })
      key.value = ''
      key.placeholder = '已保存，留空则保留当前密钥'
      state.textContent = '已保存'
      show('配置已保存，正在运行的插件已同步更新。', 'success')
    } catch (error) {
      show(error.message || '保存失败', 'error')
    } finally {
      save.disabled = false
    }
  })

  load()
})()
