import React, { useEffect, useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { useInstallPrompt } from '../../../shared/hooks/useInstallPrompt';
import { CookieSection } from './CookieSection';

export function SettingsScreen({ onLogout }) {
  const { canInstall, showIOSGuide, installed, promptInstall } = useInstallPrompt();

  const [prefs, setPrefs] = useState({
    exploration_rate: 0.3,
    min_clip_seconds: 5,
    max_clip_seconds: 120,
    autoplay: true,
  });

  useEffect(() => {
    api.getProfile().catch(() => {});
  }, []);

  function handleChange(key, value) {
    const updated = { ...prefs, [key]: value };
    setPrefs(updated);
    api.updatePreferences(updated).catch(console.error);
  }

  return (
    <div className="settings-screen">
      <div className="settings-title">Algorithm Controls</div>

      <div className="settings-section">
        <h3>Feed Tuning</h3>

        <div className="slider-row">
          <div className="slider-header">
            <span className="slider-label">Discovery vs Comfort</span>
            <span className="slider-value">{Math.round(prefs.exploration_rate * 100)}%</span>
          </div>
          <input
            type="range"
            min="0"
            max="1"
            step="0.05"
            value={prefs.exploration_rate}
            onChange={(e) => handleChange('exploration_rate', parseFloat(e.target.value))}
          />
          <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 11, color: 'var(--text-muted)', marginTop: 6 }}>
            <span>More of what I like</span>
            <span>Surprise me</span>
          </div>
        </div>

        <div className="slider-row">
          <div className="slider-header">
            <span className="slider-label">Min Clip Length</span>
            <span className="slider-value">{prefs.min_clip_seconds}s</span>
          </div>
          <input
            type="range"
            min="5"
            max="60"
            step="5"
            value={prefs.min_clip_seconds}
            onChange={(e) => handleChange('min_clip_seconds', parseInt(e.target.value, 10))}
          />
        </div>

        <div className="slider-row">
          <div className="slider-header">
            <span className="slider-label">Max Clip Length</span>
            <span className="slider-value">{prefs.max_clip_seconds}s</span>
          </div>
          <input
            type="range"
            min="30"
            max="300"
            step="15"
            value={prefs.max_clip_seconds}
            onChange={(e) => handleChange('max_clip_seconds', parseInt(e.target.value, 10))}
          />
        </div>
      </div>

      {api.getToken() && <CookieSection />}

      <div className="settings-section">
        <h3>Account</h3>
        <div className="setting-row">
          <span className="setting-label">Autoplay</span>
          <button
            style={{
              background: prefs.autoplay ? 'var(--accent)' : 'var(--bg-surface)',
              border: 'none', borderRadius: 12, width: 44, height: 24, cursor: 'pointer',
              position: 'relative', transition: 'background 0.2s',
            }}
            onClick={() => handleChange('autoplay', !prefs.autoplay)}
          >
            <div style={{
              width: 18, height: 18, borderRadius: '50%', background: 'white',
              position: 'absolute', top: 3,
              left: prefs.autoplay ? 23 : 3, transition: 'left 0.2s',
            }} />
          </button>
        </div>

        <div className="install-row">
          <span className="install-row-label">Install App</span>
          {installed ? (
            <span className="install-row-badge">
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none">
                <path d="M5 13l4 4L19 7" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" />
              </svg>
              Installed
            </span>
          ) : canInstall ? (
            <button className="install-row-btn" onClick={promptInstall}>Install</button>
          ) : showIOSGuide ? (
            <span className="install-ios-hint">
              Use Safari Share &rarr; Add to Home Screen
            </span>
          ) : (
            <span className="install-ios-hint">Open in mobile browser</span>
          )}
        </div>

        {onLogout && (
          <button
            style={{
              width: '100%', padding: 14, background: 'var(--bg-surface)',
              border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)',
              color: 'var(--accent)', fontFamily: 'var(--font)', fontSize: 15,
              fontWeight: 500, cursor: 'pointer', marginTop: 8,
            }}
            onClick={onLogout}
          >
            Sign Out
          </button>
        )}
      </div>
    </div>
  );
}
