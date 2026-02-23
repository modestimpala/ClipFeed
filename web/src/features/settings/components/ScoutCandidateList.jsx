import React, { useEffect, useState } from 'react';
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

export function ScoutCandidateList() {
  const [tab, setTab] = useState('ingested');
  const [candidates, setCandidates] = useState([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    setLoading(true);
    api.getScoutCandidates(tab)
      .then((data) => setCandidates(data.candidates || []))
      .catch(() => setCandidates([]))
      .finally(() => setLoading(false));
  }, [tab]);

  function handleApprove(id) {
    setCandidates((prev) => prev.filter((c) => c.id !== id));
    api.approveCandidate(id).catch(() => {});
  }

  return (
    <div className="scout-candidates">
      <div className="scout-pill-toggle">
        <button
          className={`scout-pill ${tab === 'ingested' ? 'active' : ''}`}
          onClick={() => setTab('ingested')}
        >
          Ingested
        </button>
        <button
          className={`scout-pill ${tab === 'rejected' ? 'active' : ''}`}
          onClick={() => setTab('rejected')}
        >
          Rejected
        </button>
      </div>

      {loading ? (
        <div className="scout-empty">Loading…</div>
      ) : candidates.length === 0 ? (
        <div className="scout-empty">
          {tab === 'ingested' ? 'No ingested candidates yet.' : 'No rejected candidates.'}
        </div>
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
                  <span>{timeAgo(c.created_at)}</span>
                </div>
              </div>
              <div className="scout-candidate-right">
                <span className={`scout-score-badge ${tab === 'ingested' ? 'green' : 'red'}`}>
                  {c.llm_score != null ? Number(c.llm_score).toFixed(1) : '–'}
                </span>
                {tab === 'rejected' && (
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
