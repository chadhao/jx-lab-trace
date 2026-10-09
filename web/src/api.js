// web/src/api.js —— 极简 fetch 封装（同源 cookie 会话，不引额外依赖）。
const BASE = ''

async function request(method, path, body) {
  const opt = { method, headers: {} }
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json'
    opt.body = JSON.stringify(body)
  }
  const resp = await fetch(BASE + path, opt)
  let data = null
  const text = await resp.text()
  if (text) {
    try {
      data = JSON.parse(text)
    } catch (e) {
      data = { error: text }
    }
  }
  if (!resp.ok) {
    const err = new Error((data && data.error) || `HTTP ${resp.status}`)
    err.status = resp.status
    err.data = data
    throw err
  }
  return data
}

async function upload(path, formData) {
  const resp = await fetch(BASE + path, { method: 'POST', body: formData })
  let data = null
  const text = await resp.text()
  if (text) {
    try {
      data = JSON.parse(text)
    } catch (e) {
      data = { error: text }
    }
  }
  if (!resp.ok) {
    const err = new Error((data && data.error) || `HTTP ${resp.status}`)
    err.status = resp.status
    err.data = data
    throw err
  }
  return data
}

export const api = {
  get: (p) => request('GET', p),
  post: (p, b) => request('POST', p, b),
  put: (p, b) => request('PUT', p, b),
  patch: (p, b) => request('PATCH', p, b),
  del: (p) => request('DELETE', p),
  upload: (p, fd) => upload(p, fd),
}
