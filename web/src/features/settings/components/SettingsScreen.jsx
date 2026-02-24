import React, { useEffect, useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { Icons } from '../../../shared/ui/icons';
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
    diversity_mix: 0.5,
    trending_boost: true,
    freshness_bias: 0.5,
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
      <div className="screen-title">Settings</div>

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
        <h3>Algorithm</h3>

        <div className="slider-row">
          <div className="slider-header">
            <span className="slider-label">Feed Diversity</span>
            <span className="slider-value">{Math.round(prefs.diversity_mix * 100)}%</span>
          </div>
          <input
            type="range"
            min="0"
            max="1"
            step="0.05"
            value={prefs.diversity_mix}
            onChange={(e) => handleChange('diversity_mix', parseFloat(e.target.value))}
          />
          <div className="slider-hint-row">
            <span>Best matches first</span>
            <span>Mix it up</span>
          </div>
          <div className="slider-description">Prevents your feed from being dominated by one topic or channel</div>
        </div>

        <div className="slider-row">
          <div className="slider-header">
            <span className="slider-label">Freshness</span>
            <span className="slider-value">
              {prefs.freshness_bias <= 0.2 ? 'Archive' : prefs.freshness_bias <= 0.4 ? 'Relaxed' : prefs.freshness_bias <= 0.6 ? 'Balanced' : prefs.freshness_bias <= 0.8 ? 'Fresh' : 'Breaking'}
            </span>
          </div>
          <input
            type="range"
            min="0"
            max="1"
            step="0.1"
            value={prefs.freshness_bias}
            onChange={(e) => handleChange('freshness_bias', parseFloat(e.target.value))}
          />
          <div className="slider-hint-row">
            <span>Old content is fine</span>
            <span>Only the latest</span>
          </div>
          <div className="slider-description">How quickly older clips fade from your feed</div>
        </div>

        <div className="setting-row">
          <div className="setting-label-group">
            <span className="setting-label">Trending Boost</span>
            <span className="setting-sublabel">Surface clips gaining traction</span>
          </div>
          <button
            className={`toggle-switch ${prefs.trending_boost ? 'on' : ''}`}
            onClick={() => handleChange('trending_boost', !prefs.trending_boost)}
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
            <Icons.ChevronRight />
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
