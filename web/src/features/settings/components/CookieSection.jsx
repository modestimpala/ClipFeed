import React, { useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';

const COOKIE_PLATFORMS = [
  { platform: 'youtube', label: 'YouTube' },
  { platform: 'tiktok', label: 'TikTok' },
  { platform: 'instagram', label: 'Instagram' },
];

export function CookieSection() {
  const [cookies, setCookies] = useState({});
  const [status, setStatus] = useState({});

  async function handleSave(platform) {
    const cookieStr = cookies[platform];
    if (!cookieStr?.trim()) return;
    try {
      await api.setCookie(platform, cookieStr);
      setStatus((s) => ({ ...s, [platform]: 'saved' }));
      setTimeout(() => setStatus((s) => ({ ...s, [platform]: null })), 2000);
    } catch {
      setStatus((s) => ({ ...s, [platform]: 'error' }));
    }
  }

  async function handleDelete(platform) {
    try {
      await api.deleteCookie(platform);
      setCookies((c) => ({ ...c, [platform]: '' }));
      setStatus((s) => ({ ...s, [platform]: 'cleared' }));
      setTimeout(() => setStatus((s) => ({ ...s, [platform]: null })), 2000);
    } catch {
      setStatus((s) => ({ ...s, [platform]: 'error' }));
    }
  }

  return (
    <div className="settings-section">
      <h3>Platform Cookies</h3>
      <p style={{ fontSize: 12, color: 'var(--text-muted)', marginBottom: 12, lineHeight: 1.5 }}>
        Paste cookie headers from your browser DevTools (Network tab &rarr; request headers &rarr; Cookie)
        to enable authenticated downloads. YouTube cookies help bypass age-gating and rate limits.
      </p>

      {COOKIE_PLATFORMS.map(({ platform, label }) => (
        <div key={platform} className="cookie-platform-row">
          <div className="cookie-platform-label">{label}</div>
          <textarea
            className="cookie-textarea"
            rows={3}
            placeholder={`Paste ${label} cookie string here...`}
            value={cookies[platform] || ''}
            onChange={(e) => setCookies((c) => ({ ...c, [platform]: e.target.value }))}
          />
          <div className="cookie-actions-row">
            <button
              className="cookie-save-btn"
              onClick={() => handleSave(platform)}
              disabled={!(cookies[platform] || '').trim()}
            >
              Save
            </button>
            <button
              className="cookie-clear-btn"
              onClick={() => handleDelete(platform)}
            >
              Clear
            </button>
            {status[platform] && (
              <span className={`cookie-status ${status[platform] === 'error' ? 'cookie-status-error' : 'cookie-status-ok'}`}>
                {status[platform] === 'saved' ? 'Saved!' : status[platform] === 'cleared' ? 'Cleared' : 'Error'}
              </span>
            )}
          </div>
        </div>
      ))}
    </div>
  );
}
