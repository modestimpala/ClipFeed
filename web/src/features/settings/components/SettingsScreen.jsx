import React, { useEffect, useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { useInstallPrompt } from '../../../shared/hooks/useInstallPrompt';
import { CookieSection } from './CookieSection';
import { TopicWeights } from './TopicWeights';
import { ScoutScreen } from './ScoutScreen';
import '../scout.css';

export function SettingsScreen({ onLogout }) {
  const { canInstall, showIOSGuide, installed, promptInstall } = useInstallPrompt();
  const [subscreen, setSubscreen] = useState(null);
  const [aiEnabled, setAiEnabled] = useState(false);

  const [prefs, setPrefs] = useState({
    exploration_rate: 0.3,
    dedupe_seen_24h: true,
    min_clip_seconds: 5,
    max_clip_seconds: 120,
    autoplay: true,
    topic_weights: {},
    scout_threshold: 6.0,
  });

  useEffect(() => {
    api.getProfile()
      .then((data) => {
        if (data.preferences) {
          setPrefs((prev) => ({ ...prev, ...data.preferences }));
        }
      })
      .catch(() => {});
    api.getConfig()
      .then((data) => setAiEnabled(!!data.ai_enabled))
      .catch(() => {});
  }, []);

  function handleChange(key, value) {
    const updated = { ...prefs, [key]: value };
    setPrefs(updated);
    api.updatePreferences(updated).catch(console.error);
  }

  if (subscreen === 'scout') {
    return (
      <ScoutScreen
        onBack={() => setSubscreen(null)}
        threshold={prefs.scout_threshold}
        onThresholdChange={(v) => handleChange('scout_threshold', v)}
      />
    );
  }

  return (
    <div className="settings-screen">
      <div className="settings-title">Settings</div>

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
          <div className="slider-hint-row">
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

        <div className="setting-row">
          <span className="setting-label">Hide clips seen in last 24h</span>
          <button
            className={`toggle-switch ${prefs.dedupe_seen_24h ? 'on' : ''}`}
            onClick={() => handleChange('dedupe_seen_24h', !prefs.dedupe_seen_24h)}
          >
            <div className="toggle-knob" />
          </button>
        </div>
      </div>

      <div className="settings-section">
        <h3>Topic Preferences</h3>
        <TopicWeights
          currentWeights={prefs.topic_weights}
          onWeightsChange={(tw) => setPrefs((prev) => ({ ...prev, topic_weights: tw }))}
        />
      </div>

      {aiEnabled && (
      <div className="settings-section">
        <h3>Discovery</h3>
        <div className="scout-nav-row" onClick={() => setSubscreen('scout')}>
          <span className="scout-nav-label">Content Scout</span>
          <span className="scout-nav-chevron">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <polyline points="9 18 15 12 9 6" />
            </svg>
          </span>
        </div>
      </div>
      )}

      {api.getToken() && <CookieSection />}

      <div className="settings-section">
        <h3>Playback</h3>
        <div className="setting-row">
          <span className="setting-label">Autoplay</span>
          <button
            className={`toggle-switch ${prefs.autoplay ? 'on' : ''}`}
            onClick={() => handleChange('autoplay', !prefs.autoplay)}
          >
            <div className="toggle-knob" />
          </button>
        </div>
      </div>

      <div className="settings-section">
        <h3>App</h3>
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
          <button className="sign-out-btn" onClick={onLogout}>
            Sign Out
          </button>
        )}
      </div>
    </div>
  );
}
