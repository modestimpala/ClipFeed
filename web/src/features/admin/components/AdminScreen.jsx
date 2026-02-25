import React, { useEffect, useState, useRef } from 'react';
import { api } from '../../../shared/api/clipfeedApi';

/* ── tiny inline icons (avoids polluting the shared icon set) ── */
const AdminIcons = {
  Zap: () => <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><polygon points="13 2 3 14 12 14 11 22 21 10 12 10 13 2"/></svg>,
  Database: () => <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M21 12c0 1.66-4 3-9 3s-9-1.34-9-3"/><path d="M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5"/></svg>,
  Film: () => <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="2" y="2" width="20" height="20" rx="2.18" ry="2.18"/><line x1="7" y1="2" x2="7" y2="22"/><line x1="17" y1="2" x2="17" y2="22"/><line x1="2" y1="12" x2="22" y2="12"/><line x1="2" y1="7" x2="7" y2="7"/><line x1="2" y1="17" x2="7" y2="17"/><line x1="17" y1="17" x2="22" y2="17"/><line x1="17" y1="7" x2="22" y2="7"/></svg>,
  Cpu: () => <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="4" y="4" width="16" height="16" rx="2" ry="2"/><rect x="9" y="9" width="6" height="6"/><line x1="9" y1="1" x2="9" y2="4"/><line x1="15" y1="1" x2="15" y2="4"/><line x1="9" y1="20" x2="9" y2="23"/><line x1="15" y1="20" x2="15" y2="23"/><line x1="20" y1="9" x2="23" y2="9"/><line x1="20" y1="14" x2="23" y2="14"/><line x1="1" y1="9" x2="4" y2="9"/><line x1="1" y1="14" x2="4" y2="14"/></svg>,
  Brain: () => <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M12 2a7 7 0 0 0-7 7c0 2.38 1.19 4.47 3 5.74V17a2 2 0 0 0 2 2h4a2 2 0 0 0 2-2v-2.26c1.81-1.27 3-3.36 3-5.74a7 7 0 0 0-7-7z"/><line x1="10" y1="22" x2="14" y2="22"/></svg>,
  Activity: () => <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><polyline points="22 12 18 12 15 21 9 3 6 12 2 12"/></svg>,
};

function fmt(n) {
  if (n == null) return '0';
  return Number(n).toLocaleString();
}

/* ── login ── */
function AdminLogin({ onLogin, onBack }) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);

  async function handleSubmit(e) {
    e.preventDefault();
    setError('');
    setLoading(true);
    try {
      const data = await api.adminLogin(username, password);
      sessionStorage.setItem('clipfeed_admin_token', data.token);
      onLogin();
    } catch (err) {
      setError(err.error || 'Invalid credentials');
    } finally {
      setLoading(false);
    }
  }

  return (
    <div className="admin-login-screen">
      <button className="admin-back-btn" onClick={onBack}>&larr; Back to App</button>
      <div className="auth-logo">Admin<span>Portal</span></div>
      <form className="auth-form" onSubmit={handleSubmit}>
        <input className="auth-input" placeholder="Admin Username" value={username} onChange={(e) => setUsername(e.target.value)} autoCapitalize="none" />
        <input className="auth-input" type="password" placeholder="Admin Password" value={password} onChange={(e) => setPassword(e.target.value)} />
        {error && <div className="auth-error">{error}</div>}
        <button className="auth-submit" type="submit" disabled={loading}>
          {loading ? 'Authenticating...' : 'Login'}
        </button>
      </form>
    </div>
  );
}

/* ── stat row ── */
function StatRow({ label, value, statusClass, sub }) {
  return (
    <div className="admin-stat-row">
      <span className="admin-stat-label">{label}</span>
      <span className="admin-stat-right">
        <span className={`admin-stat-value ${statusClass || ''}`}>{value}</span>
        {sub && <span className="admin-stat-sub">{sub}</span>}
      </span>
    </div>
  );
}

/* ── hero metric ── */
function HeroMetric({ label, value, sub, color }) {
  return (
    <div className="admin-hero-metric" style={{ '--hero-color': color }}>
      <div className="admin-hero-value">{value}</div>
      <div className="admin-hero-label">{label}</div>
      {sub && <div className="admin-hero-sub">{sub}</div>}
    </div>
  );
}

/* ── progress bar for queue ── */
function QueueBar({ queued, running, complete, failed }) {
  const total = queued + running + complete + failed || 1;
  const pct = (n) => ((n / total) * 100).toFixed(1) + '%';
  return (
    <div className="admin-queue-bar" title={`${complete} complete · ${running} running · ${queued} queued · ${failed} failed`}>
      <div className="admin-queue-seg complete" style={{ width: pct(complete) }} />
      <div className="admin-queue-seg running" style={{ width: pct(running) }} />
      <div className="admin-queue-seg queued" style={{ width: pct(queued) }} />
      <div className="admin-queue-seg failed" style={{ width: pct(failed) }} />
    </div>
  );
}

/* ── bar chart ── */
function SimpleBarChart({ data, title, accent }) {
  if (!data || data.length === 0) {
    return <div className="admin-chart-empty">No data for {title}</div>;
  }
  const max = Math.max(...data.map(d => d.count), 1);
  const total = data.reduce((s, d) => s + d.count, 0);

  return (
    <div className="admin-chart">
      <div className="admin-chart-header">
        <div className="admin-chart-title">{title}</div>
        <div className="admin-chart-total">{fmt(total)} total</div>
      </div>
      <div className="admin-chart-bars">
        {data.map((d, i) => {
          const heightPct = (d.count / max) * 100;
          return (
            <div key={i} className="admin-chart-bar-container" title={`${d.date}: ${d.count}`}>
              <div className="admin-chart-bar-value">{d.count}</div>
              <div className="admin-chart-bar" style={{ height: `${heightPct}%`, background: accent || 'var(--accent)' }} />
              <div className="admin-chart-label">{d.date.slice(5)}</div>
            </div>
          );
        })}
      </div>
    </div>
  );
}

/* ── LLM logs modal ── */
function LLMLogsModal({ onClose }) {
  const [logs, setLogs] = useState([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    const adminToken = sessionStorage.getItem('clipfeed_admin_token');

    api.getAdminLLMLogs(adminToken)
      .then(data => setLogs(data.logs || []))
      .catch(console.error)
      .finally(() => {
        setLoading(false);
      });
  }, []);

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="admin-logs-modal" onClick={e => e.stopPropagation()}>
        <div className="admin-logs-header">
          <h2>LLM Interaction Logs</h2>
          <button className="admin-logout-btn" onClick={onClose}>Close</button>
        </div>
        <div className="admin-logs-list">
          {loading ? (
            <div className="loading-text">Loading logs...</div>
          ) : logs.length === 0 ? (
            <div className="admin-chart-empty">No LLM logs found.</div>
          ) : (
            logs.map(log => (
              <div key={log.id} className="admin-log-entry">
                <div className="admin-log-meta">
                  <span className="admin-log-badge">{log.system}</span>
                  <span>{log.model}</span>
                  <span>{log.duration_ms}ms</span>
                  <span>{new Date(log.created_at).toLocaleString()}</span>
                </div>
                <div className="admin-log-section">
                  <div className="admin-log-label">Prompt</div>
                  <pre className="admin-log-content">{log.prompt}</pre>
                </div>
                {log.error ? (
                  <div className="admin-log-section error">
                    <div className="admin-log-label">Error</div>
                    <pre className="admin-log-content">{log.error}</pre>
                  </div>
                ) : (
                  <div className="admin-log-section">
                    <div className="admin-log-label">Response</div>
                    <pre className="admin-log-content">{log.response}</pre>
                  </div>
                )}
              </div>
            ))
          )}
        </div>
      </div>
    </div>
  );
}

/* ── failures modal ── */
function FailuresModal({ failures, onClear, clearing, onClose }) {
  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="admin-failures-modal" onClick={e => e.stopPropagation()}>
        <div className="admin-failures-header">
          <h2>{failures.length} Failed Job{failures.length !== 1 ? 's' : ''}</h2>
          <button className="admin-header-btn" onClick={onClose}>Close</button>
        </div>
        {failures.length === 0 ? (
          <div className="admin-chart-empty">No failures right now.</div>
        ) : (
          <div className="admin-failures-list">
            {failures.map(f => (
              <div key={f.id} className="admin-failure-card">
                <div className="admin-failure-name">{f.title || f.url || f.id}</div>
                <div className="admin-failure-error">{f.error || 'Unknown error'}</div>
                <div className="admin-failure-meta">
                  Attempt {f.attempts}/3
                  {f.failed_at ? ` · ${new Date(f.failed_at).toLocaleString()}` : ''}
                </div>
              </div>
            ))}
          </div>
        )}
        {failures.length > 0 && (
          <button className="admin-clear-btn modal" onClick={onClear} disabled={clearing}>
            {clearing ? 'Clearing...' : 'Clear All Failed Jobs'}
          </button>
        )}
      </div>
    </div>
  );
}

/* ── health status ── */
function getHealthStatus(stats) {
  const failedJobs = stats.queue?.failed || 0;
  const failedClips = stats.content?.failed || 0;
  const running = stats.queue?.running || 0;
  const queued = stats.queue?.queued || 0;

  if (failedJobs > 10)
    return { level: 'error', label: 'Job Failures', detail: `${failedJobs} jobs exhausted all retries` };
  if (failedClips > 10)
    return { level: 'error', label: 'Clip Failures', detail: `${failedClips} clips failed processing` };
  if (failedJobs > 0)
    return { level: 'warn', label: `${failedJobs} Failed Job${failedJobs > 1 ? 's' : ''}`, detail: 'Retries exhausted — review or clear' };
  if (failedClips > 0)
    return { level: 'warn', label: `${failedClips} Failed Clip${failedClips > 1 ? 's' : ''}`, detail: 'Clips stuck in failed state' };
  if (running > 0 || queued > 0)
    return { level: 'active', label: 'Processing', detail: `${running} running, ${queued} queued` };
  return { level: 'ok', label: 'All Systems Normal', detail: 'No issues' };
}

/* ── main screen ── */
export function AdminScreen({ onBack }) {
  const [authed, setAuthed] = useState(!!sessionStorage.getItem('clipfeed_admin_token'));
  const [stats, setStats] = useState(null);
  const [error, setError] = useState(null);
  const [showLogs, setShowLogs] = useState(false);
  const [showFailures, setShowFailures] = useState(false);
  const [lastRefresh, setLastRefresh] = useState(null);
  const [clearing, setClearing] = useState(false);
  const timerRef = useRef(null);

  function loadStats() {
    const adminToken = sessionStorage.getItem('clipfeed_admin_token');

    api.getAdminStatus(adminToken)
      .then((data) => {
        setStats(data);
        setError(null);
        setLastRefresh(new Date());
      })
      .catch((err) => {
        if (err.status === 401) handleLogout();
        else setError(err.error || 'Failed to load status');
      });
  }

  useEffect(() => {
    if (!authed) return;
    loadStats();
    timerRef.current = setInterval(loadStats, 5000);
    return () => { if (timerRef.current) clearInterval(timerRef.current); };
  }, [authed]);

  function handleLogout() {
    sessionStorage.removeItem('clipfeed_admin_token');
    setAuthed(false);
    setStats(null);
  }

  async function handleClearFailed() {
    if (clearing) return;
    setClearing(true);
    const adminToken = sessionStorage.getItem('clipfeed_admin_token');
    try {
      await api.clearFailedJobs(adminToken);
      loadStats();
    } catch (err) {
      console.error('Failed to clear jobs:', err);
    } finally {
      setClearing(false);
    }
  }

  if (!authed) return <AdminLogin onLogin={() => setAuthed(true)} onBack={onBack} />;

  if (!stats) {
    return (
      <div className="admin-screen" style={{ display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
        <div className="admin-loading-spinner" />
      </div>
    );
  }

  const health = getHealthStatus(stats);

  return (
    <div className="admin-screen">
      {/* ── header ── */}
      <div className="admin-header">
        <div className="admin-header-left">
          <h1 className="admin-title">System Status</h1>
          <div className={`admin-health-pill ${health.level}`}>
            <span className="admin-health-dot" />
            {health.label}
          </div>
        </div>
        <div className="admin-header-right">
          {lastRefresh && (
            <span className="admin-refresh-text">
              Updated {lastRefresh.toLocaleTimeString()}
            </span>
          )}
          <button className="admin-header-btn" onClick={onBack}>Exit</button>
          <button className="admin-header-btn" onClick={handleLogout}>Logout</button>
        </div>
      </div>

      {error && <div className="admin-error">{error}</div>}

      {/* ── hero metrics ── */}
      <div className="admin-hero-row">
        <HeroMetric label="Ready Clips" value={fmt(stats.content?.ready)} color="var(--success)" />
        <HeroMetric label="Users" value={fmt(stats.database?.total_users)} color="var(--accent)" />
        <HeroMetric label="Active Queue" value={fmt((stats.queue?.running || 0) + (stats.queue?.queued || 0))} color="var(--warning)" sub={`${fmt(stats.queue?.running)} running`} />
        <HeroMetric label="Storage" value={`${(stats.content?.storage_gb || 0).toFixed(1)} GB`} color="var(--accent-secondary)" sub={`DB ${(stats.database?.size_mb || 0).toFixed(1)} MB`} />
      </div>

      {/* ── detail cards ── */}
      <div className="admin-grid">
        <div className="admin-card accent-warning">
          <h3><AdminIcons.Zap /> Queue</h3>
          <QueueBar
            queued={stats.queue?.queued || 0}
            running={stats.queue?.running || 0}
            complete={stats.queue?.complete || 0}
            failed={stats.queue?.failed || 0}
          />
          <StatRow label="Running" value={fmt(stats.queue?.running)} statusClass="processing" />
          <StatRow label="Queued" value={fmt(stats.queue?.queued)} />
          <StatRow label="Completed" value={fmt(stats.queue?.complete)} statusClass="ready" />
          <StatRow label="Failed" value={fmt(stats.queue?.failed)} statusClass={stats.queue?.failed > 0 ? 'failed' : ''} />
          {stats.queue?.rejected > 0 && (
            <StatRow label="Rejected" value={fmt(stats.queue?.rejected)} sub="validation" />
          )}
          {stats.queue?.failed > 0 && (
            <div className="admin-failed-actions">
              <button className="admin-failures-link" onClick={() => setShowFailures(true)}>
                {stats.queue.failed} failure{stats.queue.failed !== 1 ? 's' : ''} &rarr;
              </button>
              <button className="admin-clear-btn" onClick={handleClearFailed} disabled={clearing}>
                {clearing ? 'Clearing...' : 'Clear All'}
              </button>
            </div>
          )}
        </div>

        <div className="admin-card accent-success">
          <h3><AdminIcons.Film /> Content</h3>
          <StatRow label="Ready" value={fmt(stats.content?.ready)} statusClass="ready" />
          <StatRow label="Processing" value={fmt(stats.content?.processing)} statusClass="processing" />
          <StatRow label="Failed" value={fmt(stats.content?.failed)} statusClass={stats.content?.failed > 0 ? 'failed' : ''} />
          <StatRow label="Expired" value={fmt(stats.content?.expired)} />
          <StatRow label="Evicted" value={fmt(stats.content?.evicted)} />
          <StatRow label="Storage" value={`${(stats.content?.storage_gb || 0).toFixed(2)} GB`} />
        </div>

        <div className="admin-card accent-accent">
          <h3><AdminIcons.Database /> Database</h3>
          <StatRow label="Users" value={fmt(stats.database?.total_users)} />
          <StatRow label="Interactions" value={fmt(stats.database?.total_interactions)} />
          <StatRow label="DB Size" value={`${(stats.database?.size_mb || 0).toFixed(2)} MB`} />
        </div>

        <div className="admin-card accent-dim">
          <h3><AdminIcons.Cpu /> Runtime</h3>
          <StatRow label="Memory" value={`${stats.system?.memory_mb || 0} MB`} />
          <StatRow label="Goroutines" value={fmt(stats.system?.goroutines)} />
          <StatRow label="Threads" value={stats.system?.os_threads || 0} />
          <StatRow label="Go" value={stats.system?.go_version || '—'} />
        </div>

        <div className="admin-card accent-purple admin-card-clickable" onClick={() => setShowLogs(true)}>
          <h3><AdminIcons.Brain /> AI / LLM</h3>
          <StatRow label="Evaluated" value={fmt(stats.ai?.scout_evaluated)} />
          <StatRow label="Approved" value={fmt(stats.ai?.scout_approved)} statusClass="ready" />
          <StatRow label="Avg Score" value={(stats.ai?.avg_scout_llm_score || 0).toFixed(1)} />
          <StatRow label="Summaries" value={fmt(stats.ai?.clip_summaries)} />
          <div className="admin-card-hint">View LLM logs &rarr;</div>
        </div>
      </div>

      {/* ── charts ── */}
      <div className="admin-charts-section">
        <h3 className="admin-section-title"><AdminIcons.Activity /> Activity &mdash; Last 7 Days</h3>
        <div className="admin-charts-grid">
          <SimpleBarChart title="Clips Ingested" data={stats.graphs?.clips_7d} accent="var(--accent)" />
          <SimpleBarChart title="User Interactions" data={stats.graphs?.interactions_7d} accent="var(--accent-secondary)" />
        </div>
      </div>

      {showLogs && <LLMLogsModal onClose={() => setShowLogs(false)} />}
      {showFailures && (
        <FailuresModal
          failures={stats.recent_failures || []}
          onClear={handleClearFailed}
          clearing={clearing}
          onClose={() => setShowFailures(false)}
        />
      )}
    </div>
  );
}
