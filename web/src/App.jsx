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
function ClipCard({ clip, isActive, onInteract }) {
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
    <div className="clip-card" onClick={togglePlay}>
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
}

// --- Feed ---
function FeedScreen() {
  const [clips, setClips] = useState([]);
  const [activeIndex, setActiveIndex] = useState(0);
  const viewportRef = useRef(null);

  useEffect(() => {
    api.getFeed().then((data) => setClips(data.clips || [])).catch(console.error);
  }, []);

  useEffect(() => {
    const viewport = viewportRef.current;
    if (!viewport) return;
    let ticking = false;
    function onScroll() {
      if (!ticking) {
        requestAnimationFrame(() => {
          setActiveIndex(Math.round(viewport.scrollTop / viewport.clientHeight));
          ticking = false;
        });
        ticking = true;
      }
    }
    viewport.addEventListener('scroll', onScroll, { passive: true });
    return () => viewport.removeEventListener('scroll', onScroll);
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
        {clips.map((clip, i) => (
          <ClipCard key={clip.id} clip={clip} isActive={i === activeIndex} onInteract={handleInteract} />
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
      {jobs.map((job) => (
        <div key={job.id} className="job-card">
          <div className={`job-status ${job.status}`} />
          <div className="job-info">
            <div className="job-type">{job.job_type}</div>
            <div className="job-time">{job.status} - {new Date(job.created_at).toLocaleString()}</div>
          </div>
        </div>
      ))}
    </div>
  );
}

// --- Cookie Section (inside Settings) ---
function CookieSection() {
  const [tiktokCookie, setTiktokCookie] = useState('');
  const [instagramCookie, setInstagramCookie] = useState('');
  const [status, setStatus] = useState({});

  async function handleSave(platform, cookieStr) {
    if (!cookieStr.trim()) return;
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
      if (platform === 'tiktok') setTiktokCookie('');
      if (platform === 'instagram') setInstagramCookie('');
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
        Paste cookie headers from your browser DevTools (Network tab → request headers → Cookie)
        to enable authenticated downloads from TikTok and Instagram.
      </p>

      {[
        { platform: 'tiktok', label: 'TikTok', value: tiktokCookie, setter: setTiktokCookie },
        { platform: 'instagram', label: 'Instagram', value: instagramCookie, setter: setInstagramCookie },
      ].map(({ platform, label, value, setter }) => (
        <div key={platform} className="cookie-platform-row">
          <div className="cookie-platform-label">{label}</div>
          <textarea
            className="cookie-textarea"
            rows={3}
            placeholder={`Paste ${label} cookie string here...`}
            value={value}
            onChange={(e) => setter(e.target.value)}
          />
          <div className="cookie-actions-row">
            <button
              className="cookie-save-btn"
              onClick={() => handleSave(platform, value)}
              disabled={!value.trim()}
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
