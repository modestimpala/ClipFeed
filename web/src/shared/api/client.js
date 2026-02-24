const API_BASE = '/api';

export function getToken() {
  return localStorage.getItem('clipfeed_token');
}

export function setToken(token) {
  localStorage.setItem('clipfeed_token', token);
}

export function clearToken() {
  localStorage.removeItem('clipfeed_token');
}

export async function request(method, path, body = null) {
  const headers = { 'Content-Type': 'application/json' };
  const token = getToken();
  if (token) headers.Authorization = `Bearer ${token}`;

  const controller = new AbortController();
  const timeoutId = setTimeout(() => controller.abort(), 30000);

  const opts = { method, headers, signal: controller.signal };
  if (body) opts.body = JSON.stringify(body);

  try {
    const res = await fetch(`${API_BASE}${path}`, opts);
    const data = await res.json();

    if (!res.ok) throw { status: res.status, ...data };
    return data;
  } finally {
    clearTimeout(timeoutId);
  }
}
