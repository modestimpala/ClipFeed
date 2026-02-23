import React, { useEffect, useState, useCallback } from 'react';
import { api } from '../../../shared/api/clipfeedApi';

function timeAgo(iso) {
  if (!iso) return '';
  const diff = (Date.now() - new Date(iso).getTime()) / 1000;
  if (diff < 60) return 'just now';
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
  return `${Math.floor(diff / 86400)}d ago`;
}

function truncate(str, len) {
  if (!str) return '';
  return str.length > len ? str.slice(0, len) + '…' : str;
}

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

  // Auto-refresh pending tab every 5s
  useEffect(() => {
    if (tab !== 'pending') return;
    const id = setInterval(fetchCandidates, 5000);
    return () => clearInterval(id);
  }, [tab, fetchCandidates]);

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
      <div className="scout-pill-toggle">
        {TABS.map((t) => (
          <button
            key={t}
            className={`scout-pill ${tab === t ? 'active' : ''}`}
            onClick={() => setTab(t)}
          >
            {t.charAt(0).toUpperCase() + t.slice(1)}
          </button>
        ))}
      </div>

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
