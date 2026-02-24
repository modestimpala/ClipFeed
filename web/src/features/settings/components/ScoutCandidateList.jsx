import React, { useEffect, useState, useCallback } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { Tabs } from '../../../shared/ui/Tabs';
import { timeAgo, truncate } from '../../../shared/utils/formatters';

const TABS = ['pending', 'ingested', 'rejected'];
const EMPTY_MSG = {
  pending: 'No pending candidates. Trigger a source check to discover new ones.',
  ingested: 'No ingested candidates yet.',
  rejected: 'No rejected candidates.',
};

export function ScoutCandidateList() {
  const [tab, setTab] = useState('pending');
  const [candidates, setCandidates] = useState([]);
  const [loading, setLoading] = useState(true);

  const fetchCandidates = useCallback(() => {
    setLoading(true);
    api.getScoutCandidates(tab)
      .then((data) => setCandidates(data.candidates || []))
      .catch(() => setCandidates([]))
      .finally(() => setLoading(false));
  }, [tab]);

  useEffect(() => { fetchCandidates(); }, [fetchCandidates]);

  // Auto-refresh pending tab with adaptive backoff
  useEffect(() => {
    if (tab !== 'pending') return;

    let cancelled = false;
    let timeoutId = null;
    let delayMs = 5000;

    const isVisible = () => document.visibilityState === 'visible';

    const schedule = (nextDelay) => {
      if (cancelled) return;
      timeoutId = setTimeout(tick, nextDelay);
    };

    const tick = async () => {
      try {
        const data = await api.getScoutCandidates('pending');
        if (cancelled) return;
        setCandidates(data.candidates || []);
        setLoading(false);

        delayMs = isVisible() ? 5000 : 30000;
      } catch {
        if (cancelled) return;
        setLoading(false);
        delayMs = Math.min(delayMs * 2, 60000);
      } finally {
        schedule(delayMs);
      }
    };

    const onVisibilityChange = () => {
      if (!isVisible()) return;
      if (timeoutId) clearTimeout(timeoutId);
      delayMs = 5000;
      tick();
    };

    document.addEventListener('visibilitychange', onVisibilityChange);
    tick();

    return () => {
      cancelled = true;
      if (timeoutId) clearTimeout(timeoutId);
      document.removeEventListener('visibilitychange', onVisibilityChange);
    };
  }, [tab]);

  function handleApprove(id) {
    setCandidates((prev) => prev.filter((c) => c.id !== id));
    api.approveCandidate(id).catch(() => {});
  }

  function scoreBadgeClass(status) {
    if (status === 'ingested') return 'green';
    if (status === 'rejected') return 'red';
    return 'neutral';
  }

  return (
    <div className="scout-candidates">
      <Tabs
        tabs={TABS.map((t) => ({ key: t, label: t.charAt(0).toUpperCase() + t.slice(1) }))}
        activeTab={tab}
        onChange={setTab}
      />

      {loading ? (
        <div className="scout-empty">Loading…</div>
      ) : candidates.length === 0 ? (
        <div className="scout-empty">{EMPTY_MSG[tab]}</div>
      ) : (
        <div className="scout-candidate-list">
          {candidates.map((c) => (
            <div
              key={c.id}
              className="scout-candidate-card"
              onClick={() => c.url && window.open(c.url, '_blank', 'noopener')}
            >
              <div className="scout-candidate-info">
                <div className="scout-candidate-title">{truncate(c.title, 60)}</div>
                <div className="scout-candidate-meta">
                  {c.channel_name && <span>{c.channel_name}</span>}
                  {c.duration_seconds != null && (
                    <span>{Math.round(c.duration_seconds / 60)}m</span>
                  )}
                  <span>{timeAgo(c.created_at)}</span>
                </div>
              </div>
              <div className="scout-candidate-right">
                <span className={`scout-score-badge ${scoreBadgeClass(c.status)}`}>
                  {c.llm_score != null ? Number(c.llm_score).toFixed(1) : '–'}
                </span>
                {(tab === 'rejected' || tab === 'pending') && c.status !== 'ingested' && (
                  <button
                    className="scout-ingest-btn"
                    onClick={(e) => { e.stopPropagation(); handleApprove(c.id); }}
                  >
                    Ingest
                  </button>
                )}
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
