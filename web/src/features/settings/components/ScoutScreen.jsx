import React, { useEffect, useState, useCallback } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { Icons } from '../../../shared/ui/icons';
import { Tabs } from '../../../shared/ui/Tabs';
import { ScoutSourceCard } from './ScoutSourceCard';
import { AddScoutSourceForm } from './AddScoutSourceForm';
import { ScoutCandidateList } from './ScoutCandidateList';

export function ScoutScreen({ onBack, threshold, onThresholdChange, autoIngest, onAutoIngestChange }) {
  const [activeTab, setActiveTab] = useState('config');
  const [sources, setSources] = useState([]);
  const [loading, setLoading] = useState(true);
  const [profile, setProfile] = useState(null);
  const [profileLoading, setProfileLoading] = useState(true);

  const fetchSources = useCallback(() => {
    setLoading(true);
    api.getScoutSources()
      .then((data) => setSources(data.sources || []))
      .catch(() => setSources([]))
      .finally(() => setLoading(false));
  }, []);

  const fetchProfile = useCallback(() => {
    setProfileLoading(true);
    api.getScoutProfile()
      .then((data) => setProfile(data))
      .catch(() => setProfile(null))
      .finally(() => setProfileLoading(false));
  }, []);

  useEffect(() => { fetchSources(); fetchProfile(); }, [fetchSources, fetchProfile]);

  function handleUpdateSource(id, updates) {
    setSources((prev) =>
      prev.map((s) => (s.id === id ? { ...s, ...updates } : s))
    );
    api.updateScoutSource(id, updates).catch(() => fetchSources());
  }

  function handleDeleteSource(id) {
    setSources((prev) => prev.filter((s) => s.id !== id));
    api.deleteScoutSource(id).catch(() => fetchSources());
  }

  const hasProfile = profile && (
    (profile.topics && profile.topics.length > 0) ||
    (profile.channels && profile.channels.length > 0)
  );

  return (
    <div className="scout-screen">
      <div className="scout-header">
        <button className="scout-back-btn" onClick={onBack}>
          <Icons.ChevronLeft />
        </button>
        <span className="scout-header-title">Content Scout</span>
      </div>

      <Tabs
        tabs={[
          { key: 'config', label: 'Config' },
          { key: 'candidates', label: 'Candidates' },
        ]}
        activeTab={activeTab}
        onChange={setActiveTab}
      />

      {activeTab === 'config' ? (
        <div className="scout-config">
          {/* Interest Profile */}
          <div className="scout-section">
            <h3>Your Interest Profile</h3>
            <p className="scout-section-hint">
              Scout uses your likes, saves, and topic preferences to find content you'll enjoy.
            </p>
            {profileLoading ? (
              <div className="scout-empty">Loading profile…</div>
            ) : !hasProfile ? (
              <div className="scout-profile-empty">
                <span className="scout-profile-empty-icon">🎯</span>
                <p>No interest data yet. Like and save clips to teach Scout your taste.</p>
              </div>
            ) : (
              <div className="scout-profile">
                {profile.topics && profile.topics.length > 0 && (
                  <div className="scout-profile-group">
                    <span className="scout-profile-label">Topics you enjoy</span>
                    <div className="scout-profile-tags">
                      {profile.topics.map((t) => (
                        <span key={t.name} className="scout-profile-tag" title={`${t.interaction_count} interactions`}>
                          {t.name}
                        </span>
                      ))}
                    </div>
                  </div>
                )}
                {profile.channels && profile.channels.length > 0 && (
                  <div className="scout-profile-group">
                    <span className="scout-profile-label">Favorite channels</span>
                    <div className="scout-profile-tags">
                      {profile.channels.map((c) => (
                        <span key={c.name} className="scout-profile-tag scout-profile-tag--channel">
                          {c.name}
                        </span>
                      ))}
                    </div>
                  </div>
                )}
                {profile.stats && (profile.stats.likes > 0 || profile.stats.saves > 0) && (
                  <div className="scout-profile-stats">
                    {profile.stats.likes > 0 && <span>{profile.stats.likes} likes</span>}
                    {profile.stats.saves > 0 && <span>{profile.stats.saves} saves</span>}
                    {profile.stats.views > 0 && <span>{profile.stats.views} views</span>}
                  </div>
                )}
              </div>
            )}
          </div>

          {/* Auto-ingest controls */}
          <div className="scout-section">
            <h3>Auto-ingest</h3>
            <div className="setting-row">
              <div className="setting-label-group">
                <span className="setting-label">Auto-ingest approved content</span>
                <span className="setting-sublabel">
                  {autoIngest
                    ? 'Videos scoring above threshold are ingested automatically'
                    : 'Approved videos await your manual review'}
                </span>
              </div>
              <button
                className={`toggle-switch ${autoIngest ? 'on' : ''}`}
                onClick={() => onAutoIngestChange(!autoIngest)}
              >
                <div className="toggle-knob" />
              </button>
            </div>

            <div className="slider-row">
              <div className="slider-header">
                <span className="slider-label">Score threshold</span>
                <span className="slider-value">{threshold.toFixed(1)} / 10</span>
              </div>
              <input
                type="range"
                min="0"
                max="10"
                step="0.5"
                value={threshold}
                onChange={(e) => onThresholdChange(parseFloat(e.target.value))}
              />
              <div className="slider-hint-row">
                <span>Ingest everything</span>
                <span>Only the best</span>
              </div>
            </div>
          </div>

          <div className="scout-section">
            <h3>Sources</h3>
            {loading ? (
              <div className="scout-empty">Loading…</div>
            ) : sources.length === 0 ? (
              <div className="scout-empty">No sources yet. Add one to start scouting.</div>
            ) : (
              sources.map((s) => (
                <ScoutSourceCard
                  key={s.id}
                  source={s}
                  onUpdate={handleUpdateSource}
                  onDelete={handleDeleteSource}
                  onTrigger={fetchSources}
                />
              ))
            )}
            <AddScoutSourceForm onCreated={fetchSources} />
          </div>
        </div>
      ) : (
        <ScoutCandidateList />
      )}
    </div>
  );
}
