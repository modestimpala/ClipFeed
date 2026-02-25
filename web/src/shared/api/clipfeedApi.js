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

  getCookieStatus: () =>
    request('GET', '/me/cookies'),

  deleteCookie: (platform) =>
    request('DELETE', `/me/cookies/${platform}`),

  getConfig: () => request('GET', '/config'),

  // Scout
  getScoutSources: () => request('GET', '/scout/sources'),
  createScoutSource: ({ source_type, identifier, check_interval_hours }) =>
    request('POST', '/scout/sources', { source_type, platform: 'youtube', identifier, check_interval_hours }),
  updateScoutSource: (id, updates) => request('PATCH', `/scout/sources/${id}`, updates),
  deleteScoutSource: (id) => request('DELETE', `/scout/sources/${id}`),
  triggerScoutSource: (id) => request('POST', `/scout/sources/${id}/trigger`),
  getScoutCandidates: (status) => request('GET', `/scout/candidates?status=${encodeURIComponent(status)}`),
  approveCandidate: (id) => request('POST', `/scout/candidates/${id}/approve`),
  getScoutProfile: () => request('GET', '/scout/profile'),

  // Collections
  getCollections: () => request('GET', '/collections'),
  createCollection: (title, description) =>
    request('POST', '/collections', { title, description }),
  deleteCollection: (id) => request('DELETE', `/collections/${id}`),
  getCollectionClips: (id) => request('GET', `/collections/${id}/clips`),
  addToCollection: (collectionId, clipId) =>
    request('POST', `/collections/${collectionId}/clips`, { clip_id: clipId }),
  removeFromCollection: (collectionId, clipId) =>
    request('DELETE', `/collections/${collectionId}/clips/${clipId}`),

  // Admin
  adminLogin: (username, password) => request('POST', '/admin/login', { username, password }),
  getAdminStatus: () => request('GET', '/admin/status'),
  getAdminLLMLogs: () => request('GET', '/admin/llm_logs'),
  clearFailedJobs: () => request('POST', '/admin/clear-failed'),
};
