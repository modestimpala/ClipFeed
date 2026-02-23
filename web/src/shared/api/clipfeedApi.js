import { clearToken, getToken, request, setToken } from './client';

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
  getTopics: () => request('GET', '/topics'),
  updatePreferences: (prefs) => request('PUT', '/me/preferences', prefs),
  getSaved: () => request('GET', '/me/saved'),
  getHistory: () => request('GET', '/me/history'),

  getJobs: () => request('GET', '/jobs'),

  search: (q) => request('GET', `/search?q=${encodeURIComponent(q)}`),

  setCookie: (platform, cookieStr) =>
    request('PUT', `/me/cookies/${platform}`, { cookie_str: cookieStr }),

  deleteCookie: (platform) =>
    request('DELETE', `/me/cookies/${platform}`),

  // Scout
  getScoutSources: () => request('GET', '/scout/sources'),
  createScoutSource: ({ source_type, identifier, check_interval_hours }) =>
    request('POST', '/scout/sources', { source_type, platform: 'youtube', identifier, check_interval_hours }),
  updateScoutSource: (id, updates) => request('PATCH', `/scout/sources/${id}`, updates),
  deleteScoutSource: (id) => request('DELETE', `/scout/sources/${id}`),
  getScoutCandidates: (status) => request('GET', `/scout/candidates?status=${encodeURIComponent(status)}`),
  approveCandidate: (id) => request('POST', `/scout/candidates/${id}/approve`),
};
