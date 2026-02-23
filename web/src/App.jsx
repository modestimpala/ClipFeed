import React, { useState, useEffect, useRef, useCallback } from 'react';
import { api } from './api';

const Icons = {
  Home: () => (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M3 9l9-7 9 7v11a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/><polyline points="9 22 9 12 15 12 15 22"/>
    </svg>
  ),
  Plus: () => (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round">
      <line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/>
    </svg>
  ),
  Bookmark: ({ filled }) => (
    <svg viewBox="0 0 24 24" fill={filled ? 'currentColor' : 'none'} stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M19 21l-7-5-7 5V5a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2z"/>
    </svg>
  ),
  Heart: ({ filled }) => (
    <svg viewBox="0 0 24 24" fill={filled ? 'currentColor' : 'none'} stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M20.84 4.61a5.5 5.5 0 0 0-7.78 0L12 5.67l-1.06-1.06a5.5 5.5 0 0 0-7.78 7.78l1.06 1.06L12 21.23l7.78-7.78 1.06-1.06a5.5 5.5 0 0 0 0-7.78z"/>
    </svg>
  ),
  Share: () => (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <circle cx="18" cy="5" r="3"/><circle cx="6" cy="12" r="3"/><circle cx="18" cy="19" r="3"/>
      <line x1="8.59" y1="13.51" x2="15.42" y2="17.49"/><line x1="15.41" y1="6.51" x2="8.59" y2="10.49"/>
    </svg>
  ),
  Settings: () => (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <circle cx="12" cy="12" r="3"/>
      <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 0-2.83 2 2 0 0 1 2.83 0l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 0 2 2 0 0 1 0 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/>
    </svg>
  ),
  Layers: () => (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <polygon points="12 2 2 7 12 12 22 7 12 2"/><polyline points="2 17 12 22 22 17"/><polyline points="2 12 12 17 22 12"/>
    </svg>
  ),
  User: () => (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/>
    </svg>
  ),
  X: () => (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
      <line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/>
    </svg>
  ),
};

// --- Auth Screen ---
function AuthScreen({ onAuth, onSkip }) {
  const [mode, setMode] = useState('login');
  const [username, setUsername] = useState('');
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);

  async function handleSubmit(e) {
    e.preventDefault();
    setError('');
    setLoading(true);
    try {
      const data = mode === 'register'
        ? await api.register(username, email, password)
        : await api.login(username, password);
      api.setToken(data.token);
      onAuth(data);
    } catch (err) {
      setError(err.error || 'Something went wrong');
    } finally {
      setLoading(false);
    }
  }

  return (
    <div className="auth-screen">
      <div className="auth-logo">Clip<span>Feed</span></div>
      <div className="auth-subtitle">Your feed, your rules</div>
      <form className="auth-form" onSubmit={handleSubmit}>
        <input className="auth-input" placeholder="Username" value={username} onChange={(e) => setUsername(e.target.value)} autoCapitalize="none" />
        {mode === 'register' && (
          <input className="auth-input" type="email" placeholder="Email" value={email} onChange={(e) => setEmail(e.target.value)} />
        )}
        <input className="auth-input" type="password" placeholder="Password" value={password} onChange={(e) => setPassword(e.target.value)} />
        {error && <div className="auth-error">{error}</div>}
        <button className="auth-submit" type="submit" disabled={loading}>
          {loading ? 'Loading...' : mode === 'login' ? 'Sign In' : 'Create Account'}
        </button>
      </form>
      <button className="auth-toggle" onClick={() => setMode(mode === 'login' ? 'register' : 'login')}>
        {mode === 'login' ? <>No account? <span>Sign up</span></> : <>Have an account? <span>Sign in</span></>}
      </button>
      <button className="auth-skip" onClick={onSkip}>Browse without an account</button>
    </div>
  );
}

// --- Clip Card ---
const ClipCard = React.forwardRef(function ClipCard({ clip, isActive, onInteract }, ref) {
  const videoRef = useRef(null);
  const [playing, setPlaying] = useState(false);
  const [progress, setProgress] = useState(0);
  const [liked, setLiked] = useState(false);
  const [saved, setSaved] = useState(false);
  const [streamUrl, setStreamUrl] = useState(null);
  const startTimeRef = useRef(null);

  useEffect(() => {
    if (!isActive || !clip) return;
    let cancelled = false;
    api.getStreamUrl(clip.id).then((data) => {
      if (!cancelled) setStreamUrl(data.url);
    }).catch(() => {});
    return () => { cancelled = true; };
  }, [clip?.id, isActive]);

  useEffect(() => {
    const video = videoRef.current;
    if (!video) return;

    if (isActive && streamUrl) {
      video.src = streamUrl;
      video.play().then(() => {
        setPlaying(true);
        startTimeRef.current = Date.now();
        onInteract?.(clip.id, 'view');
      }).catch(() => setPlaying(false));
    } else {
      video.pause();
      video.removeAttribute('src');
      video.load();
      setPlaying(false);
      setProgress(0);
      if (startTimeRef.current && clip) {
        const watched = (Date.now() - startTimeRef.current) / 1000;
        const pct = clip.duration_seconds > 0 ? watched / clip.duration_seconds : 0;
        if (pct >= 0.9) onInteract?.(clip.id, 'watch_full', watched, Math.min(pct, 1));
        startTimeRef.current = null;
      }
    }
  }, [isActive, streamUrl]);

  useEffect(() => {
    const video = videoRef.current;
    if (!video) return;
    const onTime = () => { if (video.duration) setProgress((video.currentTime / video.duration) * 100); };
    const onEnd = () => { video.currentTime = 0; video.play().catch(() => {}); };
    video.addEventListener('timeupdate', onTime);
    video.addEventListener('ended', onEnd);
    return () => { video.removeEventListener('timeupdate', onTime); video.removeEventListener('ended', onEnd); };
  }, []);

  function togglePlay() {
    const video = videoRef.current;
    if (!video) return;
    if (video.paused) { video.play().then(() => setPlaying(true)); }
    else { video.pause(); setPlaying(false); }
  }

  function handleLike(e) {
    e.stopPropagation();
    setLiked(!liked);
    onInteract?.(clip.id, liked ? 'dislike' : 'like');
  }

  function handleSave(e) {
    e.stopPropagation();
    const willSave = !saved;
    setSaved(willSave);
    if (willSave) api.saveClip(clip.id).catch(() => setSaved(false));
    else api.unsaveClip(clip.id).catch(() => setSaved(true));
  }

  if (!clip) return <div className="clip-card" />;

  return (
    <div className="clip-card" ref={ref} data-clip-id={clip.id} onClick={togglePlay}>
      <video ref={videoRef} playsInline preload="none" loop />
      <div className="clip-overlay">
        <div className="clip-info">
          <div className="clip-title">{clip.title}</div>
          <div className="clip-source">
            {clip.platform && <span className="platform-badge">{clip.platform}</span>}
            {clip.channel_name && <span>{clip.channel_name}</span>}
            <span>{Math.round(clip.duration_seconds)}s</span>
          </div>
        </div>
      </div>
      <div className="clip-actions">
        <button className={`action-btn ${liked ? 'active' : ''}`} onClick={handleLike}><Icons.Heart filled={liked} /></button>
        <button className={`action-btn ${saved ? 'active' : ''}`} onClick={handleSave}><Icons.Bookmark filled={saved} /></button>
        <button className="action-btn" onClick={(e) => e.stopPropagation()}><Icons.Share /></button>
      </div>
      <div className="clip-progress"><div className="clip-progress-bar" style={{ width: `${progress}%` }} /></div>
    </div>
  );
});

// --- Feed ---
function FeedScreen() {
  const [clips, setClips] = useState([]);
  const [activeId, setActiveId] = useState(null);
  const viewportRef = useRef(null);
  const cardRefs = useRef(new Map());

  useEffect(() => {
    api.getFeed().then((data) => {
      const loaded = data.clips || [];
      setClips(loaded);
      if (loaded.length > 0) setActiveId(loaded[0].id);
    }).catch(console.error);
  }, []);

  useEffect(() => {
    const viewport = viewportRef.current;
    if (!viewport || clips.length === 0) return;

    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (entry.isIntersecting) {
            setActiveId(entry.target.dataset.clipId);
          }
        }
      },
      { root: viewport, threshold: 0.6 }
    );

    for (const el of cardRefs.current.values()) {
      if (el) observer.observe(el);
    }

    return () => observer.disconnect();
  }, [clips]);

  const setCardRef = useCallback((clipId, el) => {
    if (el) cardRefs.current.set(clipId, el);
    else cardRefs.current.delete(clipId);
  }, []);

  function handleInteract(clipId, action, duration, percentage) {
    if (api.getToken()) api.interact(clipId, action, duration, percentage).catch(() => {});
  }

  if (!clips.length) {
    return (
      <div className="empty-state">
        <h2>No clips yet</h2>
        <p>Add a video URL to get started. Paste a link from YouTube, TikTok, or any supported platform.</p>
      </div>
    );
  }

  return (
    <div className="feed-container">
      <div className="feed-viewport" ref={viewportRef}>
        {clips.map((clip) => (
          <ClipCard
            key={clip.id}
            clip={clip}
            isActive={clip.id === activeId}
            onInteract={handleInteract}
            ref={(el) => setCardRef(clip.id, el)}
          />
        ))}
      </div>
    </div>
  );
}

// --- Saved Screen ---
function SavedScreen() {
  const [clips, setClips] = useState([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    api.getSaved()
      .then((data) => setClips(data.clips || []))
      .catch(console.error)
      .finally(() => setLoading(false));
  }, []);

  function handleRemove(clipId) {
    api.unsaveClip(clipId)
      .then(() => setClips((prev) => prev.filter((c) => c.id !== clipId)))
      .catch(console.error);
  }

  if (loading) {
    return (
      <div className="saved-screen">
        <div className="settings-title">Saved</div>
        <div style={{ color: 'var(--text-dim)', fontSize: 14, padding: 20, textAlign: 'center' }}>Loading...</div>
      </div>
    );
  }

  if (!clips.length) {
    return (
      <div className="saved-screen">
        <div className="settings-title">Saved</div>
        <div className="empty-state" style={{ height: 'auto', padding: '40px 0' }}>
          <h2>Nothing saved yet</h2>
          <p>Tap the bookmark icon on any clip to save it here.</p>
        </div>
      </div>
    );
  }

  return (
    <div className="saved-screen">
      <div className="settings-title">Saved</div>
      <div className="saved-grid">
        {clips.map((clip) => (
          <div key={clip.id} className="saved-card">
            <div className="saved-thumb">
              {clip.thumbnail_key && (
                <img
                  src={`/storage/${clip.thumbnail_key}`}
                  alt={clip.title}
                  loading="lazy"
                />
              )}
              <div className="saved-duration">{Math.round(clip.duration_seconds)}s</div>
            </div>
            <div className="saved-info">
              <div className="saved-title">{clip.title}</div>
              {clip.topics && clip.topics.length > 0 && (
                <div className="saved-topics">
                  {clip.topics.slice(0, 3).map((t) => (
                    <span key={t} className="saved-topic-tag">{t}</span>
                  ))}
                </div>
              )}
            </div>
            <button className="saved-remove" onClick={() => handleRemove(clip.id)} title="Remove">
              <Icons.X />
            </button>
          </div>
        ))}
      </div>
    </div>
  );
}

// --- Ingest Modal ---
function IngestModal({ onClose }) {
  const [url, setUrl] = useState('');
  const [loading, setLoading] = useState(false);
  const [result, setResult] = useState(null);

  async function handleSubmit(e) {
    e.preventDefault();
    if (!url.trim()) return;
    setLoading(true);
    try {
      const data = await api.ingest(url);
      setResult(data);
      setUrl('');
    } catch (err) {
      setResult({ error: err.error || 'Failed to submit' });
    } finally {
      setLoading(false);
    }
  }

  return (
    <div className="modal-overlay" onClick={(e) => e.target === e.currentTarget && onClose()}>
      <div className="modal-sheet">
        <div className="modal-handle" />
        <div className="modal-title">Add Content</div>
        <div className="ingest-platforms">
          <span className="platform-tag">YouTube</span>
          <span className="platform-tag">Vimeo</span>
          <span className="platform-tag">TikTok</span>
          <span className="platform-tag">Instagram</span>
          <span className="platform-tag">X/Twitter</span>
          <span className="platform-tag">Direct URL</span>
        </div>
        <form onSubmit={handleSubmit}>
          <input
            className="ingest-input"
            placeholder="Paste video URL..."
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            autoFocus
          />
          <button className="ingest-submit" type="submit" disabled={loading}>
            {loading ? 'Submitting...' : 'Process Video'}
          </button>
        </form>
        {result && !result.error && (
          <div style={{ marginTop: 16, color: 'var(--success)', fontSize: 14 }}>
            Queued for processing (Job: {result.job_id?.slice(0, 8)}...)
          </div>
        )}
        {result?.error && (
          <div style={{ marginTop: 16, color: 'var(--accent)', fontSize: 14 }}>
            {result.error}
          </div>
        )}
      </div>
    </div>
  );
}

// --- Jobs Screen ---
function timeAgo(dateStr) {
  if (!dateStr) return '';
  const seconds = Math.floor((Date.now() - new Date(dateStr).getTime()) / 1000);
  if (seconds < 60) return 'just now';
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  return `${days}d ago`;
}

function formatDuration(startStr, endStr) {
  if (!startStr) return null;
  const end = endStr ? new Date(endStr) : new Date();
  const seconds = Math.floor((end - new Date(startStr)) / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  const secs = seconds % 60;
  return `${minutes}m ${secs}s`;
}

function displayUrl(url) {
  if (!url) return null;
  try {
    const u = new URL(url);
    const path = u.pathname.length > 30 ? u.pathname.slice(0, 30) + '…' : u.pathname;
    return u.hostname.replace('www.', '') + path;
  } catch { return url.length > 50 ? url.slice(0, 50) + '…' : url; }
}

function summarizeError(error) {
  if (!error) return null;
  if (error.includes('403: Forbidden')) return 'Access denied (403) — try adding cookies in Tuning → Platform Cookies';
  if (error.includes('404')) return 'Video not found (404) — link may be broken or removed';
  if (error.includes('429')) return 'Rate limited — too many requests, will retry';
  if (error.includes('nsig extraction failed')) return 'Download blocked — try adding cookies in Tuning → Platform Cookies';
  if (error.includes('Unsupported URL')) return 'URL not supported — try a different link';
  if (error.includes('Video unavailable')) return 'Video unavailable — may be deleted or private';
  const firstLine = error.split('\n').pop().trim();
  return firstLine.length > 120 ? firstLine.slice(0, 120) + '…' : firstLine;
}

const STATUS_LABELS = { queued: 'Queued', running: 'Processing', complete: 'Done', failed: 'Failed' };

function JobCard({ job }) {
  const [expanded, setExpanded] = useState(false);
  const errorSummary = summarizeError(job.error);
  const elapsed = formatDuration(job.started_at, job.completed_at);

  return (
    <div className={`job-card ${job.status === 'failed' ? 'job-card-failed' : ''}`} onClick={() => job.error && setExpanded(!expanded)}>
      <div className="job-card-header">
        <div className={`job-status ${job.status}`} />
        <div className="job-info">
          <div className="job-title-row">
            <span className="job-type">{job.title || displayUrl(job.url) || job.job_type}</span>
            {job.platform && <span className="job-platform">{job.platform}</span>}
          </div>
          <div className="job-meta">
            <span className={`job-status-label ${job.status}`}>{STATUS_LABELS[job.status] || job.status}</span>
            <span className="job-meta-sep" />
            <span>{timeAgo(job.created_at)}</span>
            {elapsed && <><span className="job-meta-sep" /><span>{elapsed}</span></>}
            {job.status === 'failed' && job.attempts > 0 && (
              <><span className="job-meta-sep" /><span>attempt {job.attempts}/{job.max_attempts}</span></>
            )}
          </div>
        </div>
      </div>
      {errorSummary && (
        <div className={`job-error ${expanded ? 'job-error-expanded' : ''}`}>
          <div className="job-error-summary">{errorSummary}</div>
          {expanded && job.error !== errorSummary && (
            <pre className="job-error-detail">{job.error}</pre>
          )}
        </div>
      )}
    </div>
  );
}

function JobsScreen() {
  const [jobs, setJobs] = useState([]);

  useEffect(() => {
    api.getJobs().then((data) => setJobs(data.jobs || [])).catch(console.error);
    const interval = setInterval(() => {
      api.getJobs().then((data) => setJobs(data.jobs || [])).catch(() => {});
    }, 5000);
    return () => clearInterval(interval);
  }, []);

  return (
    <div className="jobs-screen">
      <div className="settings-title">Processing Queue</div>
      {jobs.length === 0 && (
        <div style={{ color: 'var(--text-dim)', fontSize: 14, padding: 20, textAlign: 'center' }}>
          No jobs yet. Submit a video URL to get started.
        </div>
      )}
      {jobs.map((job) => <JobCard key={job.id} job={job} />)}
    </div>
  );
}

// --- Cookie Section (inside Settings) ---
const COOKIE_PLATFORMS = [
  { platform: 'youtube', label: 'YouTube' },
  { platform: 'tiktok', label: 'TikTok' },
  { platform: 'instagram', label: 'Instagram' },
];

function CookieSection() {
  const [cookies, setCookies] = useState({});
  const [status, setStatus] = useState({});

  async function handleSave(platform) {
    const cookieStr = cookies[platform];
    if (!cookieStr?.trim()) return;
    try {
      await api.setCookie(platform, cookieStr);
      setStatus((s) => ({ ...s, [platform]: 'saved' }));
      setTimeout(() => setStatus((s) => ({ ...s, [platform]: null })), 2000);
    } catch {
      setStatus((s) => ({ ...s, [platform]: 'error' }));
    }
  }

  async function handleDelete(platform) {
    try {
      await api.deleteCookie(platform);
      setCookies((c) => ({ ...c, [platform]: '' }));
      setStatus((s) => ({ ...s, [platform]: 'cleared' }));
      setTimeout(() => setStatus((s) => ({ ...s, [platform]: null })), 2000);
    } catch {
      setStatus((s) => ({ ...s, [platform]: 'error' }));
    }
  }

  return (
    <div className="settings-section">
      <h3>Platform Cookies</h3>
      <p style={{ fontSize: 12, color: 'var(--text-muted)', marginBottom: 12, lineHeight: 1.5 }}>
        Paste cookie headers from your browser DevTools (Network tab &rarr; request headers &rarr; Cookie)
        to enable authenticated downloads. YouTube cookies help bypass age-gating and rate limits.
      </p>

      {COOKIE_PLATFORMS.map(({ platform, label }) => (
        <div key={platform} className="cookie-platform-row">
          <div className="cookie-platform-label">{label}</div>
          <textarea
            className="cookie-textarea"
            rows={3}
            placeholder={`Paste ${label} cookie string here...`}
            value={cookies[platform] || ''}
            onChange={(e) => setCookies((c) => ({ ...c, [platform]: e.target.value }))}
          />
          <div className="cookie-actions-row">
            <button
              className="cookie-save-btn"
              onClick={() => handleSave(platform)}
              disabled={!(cookies[platform] || '').trim()}
            >
              Save
            </button>
            <button
              className="cookie-clear-btn"
              onClick={() => handleDelete(platform)}
            >
              Clear
            </button>
            {status[platform] && (
              <span className={`cookie-status ${status[platform] === 'error' ? 'cookie-status-error' : 'cookie-status-ok'}`}>
                {status[platform] === 'saved' ? 'Saved!' : status[platform] === 'cleared' ? 'Cleared' : 'Error'}
              </span>
            )}
          </div>
        </div>
      ))}
    </div>
  );
}

// --- Settings Screen ---
function SettingsScreen({ onLogout }) {
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
            type="range" min="0" max="1" step="0.05"
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
            type="range" min="5" max="60" step="5"
            value={prefs.min_clip_seconds}
            onChange={(e) => handleChange('min_clip_seconds', parseInt(e.target.value))}
          />
        </div>

        <div className="slider-row">
          <div className="slider-header">
            <span className="slider-label">Max Clip Length</span>
            <span className="slider-value">{prefs.max_clip_seconds}s</span>
          </div>
          <input
            type="range" min="30" max="300" step="15"
            value={prefs.max_clip_seconds}
            onChange={(e) => handleChange('max_clip_seconds', parseInt(e.target.value))}
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

// --- Main App ---
export default function App() {
  const [authed, setAuthed] = useState(!!api.getToken());
  const [tab, setTab] = useState('feed');
  const [showIngest, setShowIngest] = useState(false);

  function handleAuth() {
    setAuthed(true);
  }

  function handleSkip() {
    setAuthed(true);
  }

  function handleLogout() {
    api.clearToken();
    setAuthed(false);
    setTab('feed');
  }

  if (!authed) {
    return <AuthScreen onAuth={handleAuth} onSkip={handleSkip} />;
  }

  return (
    <>
      {tab === 'feed' && <FeedScreen />}
      {tab === 'jobs' && <JobsScreen />}
      {tab === 'saved' && <SavedScreen />}
      {tab === 'settings' && <SettingsScreen onLogout={api.getToken() ? handleLogout : null} />}

      {showIngest && <IngestModal onClose={() => setShowIngest(false)} />}

      <nav className="nav-bar">
        <button className={`nav-btn ${tab === 'feed' ? 'active' : ''}`} onClick={() => setTab('feed')}>
          <Icons.Home /><span>Feed</span>
        </button>
        <button className={`nav-btn ${tab === 'jobs' ? 'active' : ''}`} onClick={() => setTab('jobs')}>
          <Icons.Layers /><span>Queue</span>
        </button>
        <button className="nav-add-btn" onClick={() => setShowIngest(true)}>
          <Icons.Plus />
        </button>
        <button className={`nav-btn ${tab === 'saved' ? 'active' : ''}`} onClick={() => setTab('saved')}>
          <Icons.Bookmark filled={tab === 'saved'} /><span>Saved</span>
        </button>
        <button className={`nav-btn ${tab === 'settings' ? 'active' : ''}`} onClick={() => setTab('settings')}>
          <Icons.Settings /><span>Tuning</span>
        </button>
      </nav>
    </>
  );
}
