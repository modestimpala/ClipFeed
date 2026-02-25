const API_BASE = window.__CONFIG__?.API_BASE || '/api';
const STORAGE_BASE = window.__CONFIG__?.STORAGE_BASE || '';

// Guard against 401-reload loops: if we reloaded for a 401 within the last
// few seconds, don't reload again -- just clear the token and let the React
// auth state handle it.
const RELOAD_GUARD_KEY = 'clipfeed_401_reload_ts';
const RELOAD_GUARD_MS = 5000;

function safeReloadFor401() {
  try {
    const last = Number(sessionStorage.getItem(RELOAD_GUARD_KEY) || 0);
    if (Date.now() - last < RELOAD_GUARD_MS) {
      // Already reloaded recently -- don't loop.
      return;
    }
    sessionStorage.setItem(RELOAD_GUARD_KEY, String(Date.now()));
  } catch {
    // sessionStorage unavailable -- skip reload to be safe.
    return;
  }
  window.location.reload();
}

export function resolveStorageUrl(path) {
  if (!path) return path;
  if (path.startsWith('http')) return path;
  return `${STORAGE_BASE}${path}`;
}

export function getToken() {
  try {
    return localStorage.getItem('clipfeed_token');
  } catch {
    return null;
  }
}

export function setToken(token) {
  try {
    localStorage.setItem('clipfeed_token', token);
  } catch {
    // Storage unavailable (e.g. Safari private browsing)
  }
}

export function clearToken() {
  try {
    localStorage.removeItem('clipfeed_token');
  } catch {
    // Storage unavailable
  }
}

export async function request(method, path, body = null, { token: overrideToken } = {}) {
  const headers = { 'Content-Type': 'application/json' };
  const token = overrideToken || getToken();
  if (token) headers.Authorization = `Bearer ${token}`;

  const controller = new AbortController();
  const timeoutId = setTimeout(() => controller.abort(), 30000);

  const opts = { method, headers, signal: controller.signal };
  if (body) opts.body = JSON.stringify(body);

  try {
    const res = await fetch(`${API_BASE}${path}`, opts);
    let data;
    try {
      data = await res.json();
    } catch (e) {
      data = { error: 'Failed to parse response' };
    }

    if (!res.ok) {
      if (res.status === 401 && token && !path.startsWith('/admin') && !overrideToken) {
        clearToken();
        safeReloadFor401();
      }
      throw { status: res.status, ...data };
    }
    return data;
  } finally {
    clearTimeout(timeoutId);
  }
}
