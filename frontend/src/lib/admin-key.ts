/* 後台管理密鑰的本機儲存：與舊版 auth.js 完全相同的格式（enc:xor: / enc:v1:），
   確保 SPA 上線後既有瀏覽器 session 不需重新登入。 */

const ENC = new TextEncoder()
const DEC = new TextDecoder()
const SECRET = 'zcode2api-admin'
const XOR_PREFIX = 'enc:xor:'
const AES_PREFIX = 'enc:v1:'
const STORE_KEY = 'zcode2api_admin_key'

function toB64(bytes: Uint8Array): string {
  let s = ''
  bytes.forEach((v) => (s += String.fromCharCode(v)))
  return btoa(s)
}

function fromB64(s: string): Uint8Array {
  const bin = atob(s)
  const bytes = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
  return bytes
}

function xor(data: Uint8Array, key: Uint8Array): Uint8Array {
  const out = new Uint8Array(data.length)
  for (let i = 0; i < data.length; i++) out[i] = data[i] ^ key[i % key.length]
  return out
}

async function deriveKey(salt: Uint8Array): Promise<CryptoKey> {
  const keyMaterial = await crypto.subtle.importKey('raw', ENC.encode(SECRET), 'PBKDF2', false, ['deriveKey'])
  return crypto.subtle.deriveKey(
    { name: 'PBKDF2', salt: salt as BufferSource, iterations: 100000, hash: 'SHA-256' },
    keyMaterial,
    { name: 'AES-GCM', length: 256 },
    false,
    ['encrypt', 'decrypt'],
  )
}

async function encrypt(plain: string): Promise<string> {
  if (!plain) return ''
  if (!crypto?.subtle) {
    return XOR_PREFIX + toB64(xor(ENC.encode(plain), ENC.encode(SECRET)))
  }
  const salt = crypto.getRandomValues(new Uint8Array(16))
  const iv = crypto.getRandomValues(new Uint8Array(12))
  const key = await deriveKey(salt)
  const cipher = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, key, ENC.encode(plain))
  return `${AES_PREFIX}${toB64(salt)}:${toB64(iv)}:${toB64(new Uint8Array(cipher))}`
}

async function decrypt(stored: string): Promise<string> {
  if (!stored) return ''
  if (stored.startsWith(XOR_PREFIX)) {
    return DEC.decode(xor(fromB64(stored.slice(XOR_PREFIX.length)), ENC.encode(SECRET)))
  }
  if (!stored.startsWith(AES_PREFIX) || !crypto?.subtle) return ''
  const parts = stored.split(':')
  if (parts.length !== 5) return ''
  const key = await deriveKey(fromB64(parts[2]))
  try {
    const plain = await crypto.subtle.decrypt(
      { name: 'AES-GCM', iv: fromB64(parts[3]) as BufferSource },
      key,
      fromB64(parts[4]) as BufferSource,
    )
    return DEC.decode(plain)
  } catch {
    return ''
  }
}

export const adminKey = {
  async get(): Promise<string> {
    const stored = localStorage.getItem(STORE_KEY) || ''
    if (!stored) return ''
    try {
      return await decrypt(stored)
    } catch {
      localStorage.removeItem(STORE_KEY)
      return ''
    }
  },
  async set(value: string): Promise<void> {
    if (!value) {
      localStorage.removeItem(STORE_KEY)
      return
    }
    localStorage.setItem(STORE_KEY, (await encrypt(value)) || '')
  },
  clear(): void {
    localStorage.removeItem(STORE_KEY)
  },
}

/* 登出並回到登入頁 */
export function adminLogout(): void {
  adminKey.clear()
  location.href = '/admin/login'
}
