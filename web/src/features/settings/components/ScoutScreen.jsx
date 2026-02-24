import React, { useEffect, useState, useCallback } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { Icons } from '../../../shared/ui/icons';
import { Tabs } from '../../../shared/ui/Tabs';
import { ScoutSourceCard } from './ScoutSourceCard';
import { AddScoutSourceForm } from './AddScoutSourceForm';
import { ScoutCandidateList } from './ScoutCandidateList';

export function ScoutScreen({ onBack, threshold, onThresholdChange }) {
  const [activeTab, setActiveTab] = useState('config');
  const [sources, setSources] = useState([]);
  const [loading, setLoading] = useState(true);

  const fetchSources = useCallback(() => {
    setLoading(true);
    api.getScoutSources()
      .then((data) => setSources(data.sources || []))
      .catch(() => setSources([]))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => { fetchSources(); }, [fetchSources]);

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
          <div className="scout-section">
            <h3>Auto-ingest Threshold</h3>
            <div className="slider-row">
              <div className="slider-header">
                <span className="slider-label">Auto-ingest if score &ge;</span>
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
