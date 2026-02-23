import React, { useEffect, useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';

const COOKIE_PLATFORMS = [
  { platform: 'youtube', label: 'YouTube' },
  { platform: 'tiktok', label: 'TikTok' },
  { platform: 'instagram', label: 'Instagram' },
];

export function CookieSection() {
  const [cookies, setCookies] = useState({});
  const [status, setStatus] = useState({});
  const [savedStatus, setSavedStatus] = useState({});
  const [editing, setEditing] = useState({});
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState(false);

  useEffect(() => {
    let active = true;
    api.getCookieStatus()
      .then((data) => {
        if (!active) return;
        setSavedStatus(data?.platforms || {});
      })
      .catch(() => {
        if (!active) return;
        setLoadError(true);
      })
      .finally(() => {
        if (!active) return;
        setLoading(false);
      });
    return () => {
      active = false;
    };
  }, []);

  async function handleSave(platform) {
    const cookieStr = cookies[platform];
    if (!cookieStr?.trim()) return;
    try {
      await api.setCookie(platform, cookieStr);
      setSavedStatus((prev) => ({
        ...prev,
        [platform]: {
          saved: true,
          updated_at: new Date().toISOString(),
        },
      }));
      setEditing((prev) => ({ ...prev, [platform]: false }));
      setCookies((c) => ({ ...c, [platform]: '' }));
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
      setEditing((prev) => ({ ...prev, [platform]: false }));
      setSavedStatus((prev) => ({
        ...prev,
        [platform]: {
          saved: false,
          updated_at: null,
        },
      }));
      setStatus((s) => ({ ...s, [platform]: 'cleared' }));
      setTimeout(() => setStatus((s) => ({ ...s, [platform]: null })), 2000);
    } catch {
      setStatus((s) => ({ ...s, [platform]: 'error' }));
    }
  }

  return (
    <div className="settings-section">
      <h3>Platform Cookies</h3>
      <p className="cookie-help-text">
        Cookie values are never shown after save. We only display whether a cookie is stored for each platform.
      </p>
      <p className="cookie-help-text">
        To update a cookie, paste a new value and save again.
      </p>

      {loading && <div className="cookie-meta">Loading cookie status…</div>}
      {!loading && loadError && <div className="cookie-meta cookie-meta-error">Could not load saved status.</div>}

      {COOKIE_PLATFORMS.map(({ platform, label }) => (
        <div key={platform} className="cookie-platform-row">
          <div className="cookie-platform-head">
            <div className="cookie-platform-label">{label}</div>
            {savedStatus?.[platform]?.saved ? (
              <span className="cookie-pill cookie-pill-saved">Saved</span>
            ) : (
              <span className="cookie-pill">Not saved</span>
            )}
          </div>

          {!editing[platform] ? (
            <div className="cookie-meta">
              {savedStatus?.[platform]?.saved
                ? 'Cookie is stored. Use Update to replace it.'
                : 'No cookie stored for this platform.'}
            </div>
          ) : (
            <textarea
              className="cookie-textarea"
              rows={3}
              placeholder={`Paste ${label} cookie string here...`}
              value={cookies[platform] || ''}
              onChange={(e) => setCookies((c) => ({ ...c, [platform]: e.target.value }))}
            />
          )}

          <div className="cookie-actions-row">
            {!editing[platform] ? (
              <button
                className="cookie-save-btn"
                onClick={() => setEditing((prev) => ({ ...prev, [platform]: true }))}
              >
                {savedStatus?.[platform]?.saved ? 'Update' : 'Add'}
              </button>
            ) : (
              <>
                <button
                  className="cookie-save-btn"
                  onClick={() => handleSave(platform)}
                  disabled={!(cookies[platform] || '').trim()}
                >
                  Save
                </button>
                <button
                  className="cookie-clear-btn"
                  onClick={() => {
                    setEditing((prev) => ({ ...prev, [platform]: false }));
                    setCookies((c) => ({ ...c, [platform]: '' }));
                  }}
                >
                  Cancel
                </button>
              </>
            )}
            <button
              className="cookie-clear-btn"
              onClick={() => handleDelete(platform)}
              disabled={!savedStatus?.[platform]?.saved}
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
