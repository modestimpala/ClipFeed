const API_BASE = '/api';

function getToken() {
  return localStorage.getItem('clipfeed_token');
}

function setToken(token) {
  localStorage.setItem('clipfeed_token', token);
}

function clearToken() {
  localStorage.removeItem('clipfeed_token');
}

async function request(method, path, body = null) {
  const headers = { 'Content-Type': 'application/json' };
  const token = getToken();
  if (token) headers['Authorization'] = `Bearer ${token}`;

  const opts = { method, headers };
  if (body) opts.body = JSON.stringify(body);

  const res = await fetch(`${API_BASE}${path}`, opts);
  const data = await res.json();

  if (!res.ok) throw { status: res.status, ...data };
  return data;
}

export const api = {
  getToken,
  setToken,
  clearToken,

  register: (username, email, password) =>
    request('POST', '/auth/register', { username, email, password }),

  login: (username, password) =>
    request('POST', '/auth/login', { username, password }),

  getFeed: () => request('GET', '/feed'),

  getClip: (id) => request('GET', `/clips/${id}`),

  getStreamUrl: (id) => request('GET', `/clips/${id}/stream`),

  interact: (clipId, action, watchDuration = 0, watchPercentage = 0) =>
    request('POST', `/clips/${clipId}/interact`, {
      action,
      watch_duration_seconds: watchDuration,
      watch_percentage: watchPercentage,
    }),

  saveClip: (id) => request('POST', `/clips/${id}/save`),
  unsaveClip: (id) => request('DELETE', `/clips/${id}/save`),

  ingest: (url) => request('POST', '/ingest', { url }),

  getProfile: () => request('GET', '/me'),
  updatePreferences: (prefs) => request('PUT', '/me/preferences', prefs),
  getSaved: () => request('GET', '/me/saved'),
  getHistory: () => request('GET', '/me/history'),

  getJobs: () => request('GET', '/jobs'),

  search: (q) => request('GET', `/search?q=${encodeURIComponent(q)}`),

  setCookie: (platform, cookieStr) =>
    request('PUT', `/me/cookies/${platform}`, { cookie_str: cookieStr }),

  deleteCookie: (platform) =>
    request('DELETE', `/me/cookies/${platform}`),
};
