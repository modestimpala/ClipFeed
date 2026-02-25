import React, { useEffect, useState, useRef } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { Icons } from '../../../shared/ui/icons';

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
      // We store the admin token in a separate local storage key to avoid clobbering the main user token
      localStorage.setItem('clipfeed_admin_token', data.token);
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
        <input 
          className="auth-input" 
          placeholder="Admin Username" 
          value={username} 
          onChange={(e) => setUsername(e.target.value)} 
          autoCapitalize="none" 
        />
        <input 
          className="auth-input" 
          type="password" 
          placeholder="Admin Password" 
          value={password} 
          onChange={(e) => setPassword(e.target.value)} 
        />
        {error && <div className="auth-error">{error}</div>}
        <button className="auth-submit" type="submit" disabled={loading}>
          {loading ? 'Authenticating...' : 'Login'}
        </button>
      </form>
    </div>
  );
}

function StatRow({ label, value, statusClass }) {
  return (
    <div className="admin-stat-row">
      <span className="admin-stat-label">{label}</span>
      <span className={`admin-stat-value ${statusClass || ''}`}>{value}</span>
    </div>
  );
}

function SimpleBarChart({ data, title }) {
  if (!data || data.length === 0) {
    return <div className="admin-chart-empty">No data for {title}</div>;
  }
  
  const max = Math.max(...data.map(d => d.count), 1);

  return (
    <div className="admin-chart">
      <div className="admin-chart-title">{title}</div>
      <div className="admin-chart-bars">
        {data.map((d, i) => {
          const heightPct = (d.count / max) * 100;
          return (
            <div key={i} className="admin-chart-bar-container" title={`${d.date}: ${d.count}`}>
              <div className="admin-chart-bar-value">{d.count}</div>
              <div className="admin-chart-bar" style={{ height: `${heightPct}%` }} />
              <div className="admin-chart-label">{d.date.slice(5)}</div>
            </div>
          );
        })}
      </div>
    </div>
  );
}

function LLMLogsModal({ onClose }) {
  const [logs, setLogs] = useState([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    // Override the token temporarily
    const originalToken = api.getToken();
    const adminToken = localStorage.getItem('clipfeed_admin_token');
    if (adminToken) api.setToken(adminToken);

    api.getAdminLLMLogs()
      .then(data => setLogs(data.logs || []))
      .catch(console.error)
      .finally(() => {
        setLoading(false);
        if (originalToken) api.setToken(originalToken);
        else api.clearToken();
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

export function AdminScreen({ onBack }) {
  const [authed, setAuthed] = useState(!!localStorage.getItem('clipfeed_admin_token'));
  const [stats, setStats] = useState(null);
  const [error, setError] = useState(null);
  const [showLogs, setShowLogs] = useState(false);
  const timerRef = useRef(null);

  function loadStats() {
    // Override the token temporarily for this request
    const originalToken = api.getToken();
    const adminToken = localStorage.getItem('clipfeed_admin_token');
    if (adminToken) {
      api.setToken(adminToken);
    }

    api.getAdminStatus()
      .then((data) => {
        setStats(data);
        setError(null);
      })
      .catch((err) => {
        if (err.status === 401) {
          handleLogout();
        } else {
          setError(err.error || 'Failed to load status');
        }
      })
      .finally(() => {
        // Restore original token
        if (originalToken) {
          api.setToken(originalToken);
        } else {
          api.clearToken();
        }
      });
  }

  useEffect(() => {
    if (!authed) return;

    loadStats();
    timerRef.current = setInterval(loadStats, 5000);

    return () => {
      if (timerRef.current) clearInterval(timerRef.current);
    };
  }, [authed]);

  function handleLogout() {
    localStorage.removeItem('clipfeed_admin_token');
    setAuthed(false);
    setStats(null);
  }

  if (!authed) {
    return <AdminLogin onLogin={() => setAuthed(true)} onBack={onBack} />;
  }

  if (!stats) {
    return <div className="loading-text">Loading system status...</div>;
  }

  return (
    <div className="admin-screen">
      <div className="admin-header">
        <h1 className="admin-title">System Status</h1>
        <div style={{ display: 'flex', gap: '12px', alignItems: 'center' }}>
          <button className="admin-logout-btn" onClick={onBack}>Exit</button>
          <button className="admin-logout-btn" onClick={handleLogout}>Logout</button>
        </div>
      </div>

      {error && <div className="admin-error">{error}</div>}

      <div className="admin-grid">
        <div className="admin-card">
          <h3>Queue</h3>
          <StatRow label="Processing" value={stats.queue?.running || 0} statusClass="processing" />
          <StatRow label="Queued" value={stats.queue?.queued || 0} />
          <StatRow label="Completed" value={stats.queue?.complete || 0} statusClass="ready" />
          <StatRow label="Failed" value={stats.queue?.failed || 0} statusClass="failed" />
        </div>

        <div className="admin-card">
          <h3>Content</h3>
          <StatRow label="Ready" value={stats.content?.ready || 0} statusClass="ready" />
          <StatRow label="Processing" value={stats.content?.processing || 0} statusClass="processing" />
          <StatRow label="Failed" value={stats.content?.failed || 0} statusClass="failed" />
          <StatRow label="Storage" value={`${(stats.content?.storage_gb || 0).toFixed(2)} GB`} />
        </div>

        <div className="admin-card">
          <h3>Database</h3>
          <StatRow label="Total Users" value={stats.database?.total_users || 0} />
          <StatRow label="Interactions" value={stats.database?.total_interactions || 0} />
          <StatRow label="DB Size" value={`${(stats.database?.size_mb || 0).toFixed(2)} MB`} />
        </div>

        <div className="admin-card">
          <h3>System</h3>
          <StatRow label="Memory (Alloc)" value={`${stats.system?.memory_mb || 0} MB`} />
          <StatRow label="Goroutines" value={stats.system?.goroutines || 0} />
          <StatRow label="OS Threads" value={stats.system?.os_threads || 0} />
          <StatRow label="Go Version" value={stats.system?.go_version || 'unknown'} />
        </div>

        <div className="admin-card admin-card-clickable" onClick={() => setShowLogs(true)}>
          <h3>AI / LLM Status</h3>
          <StatRow label="Evaluated Candidates" value={stats.ai?.scout_evaluated || 0} />
          <StatRow label="Approved Candidates" value={stats.ai?.scout_approved || 0} statusClass="ready" />
          <StatRow label="Avg Scout Score" value={(stats.ai?.avg_scout_llm_score || 0).toFixed(1)} />
          <StatRow label="Generated Summaries" value={stats.ai?.clip_summaries || 0} />
          <div className="admin-card-hint">Click to view LLM logs &rarr;</div>
        </div>
      </div>

      <div className="admin-grid" style={{ marginTop: '16px' }}>
        <div className="admin-card" style={{ gridColumn: '1 / -1' }}>
          <h3>Activity (Last 7 Days)</h3>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(300px, 1fr))', gap: '24px', marginTop: '12px' }}>
            <SimpleBarChart title="Clips Ingested" data={stats.graphs?.clips_7d} />
            <SimpleBarChart title="User Interactions" data={stats.graphs?.interactions_7d} />
          </div>
        </div>
      </div>

      {showLogs && <LLMLogsModal onClose={() => setShowLogs(false)} />}
    </div>
  );
}
