import React, { useState, useEffect, lazy, Suspense } from 'react';
import { api } from '../shared/api/clipfeedApi';
import { Icons } from '../shared/ui/icons';
import { InstallPrompt } from '../shared/ui/InstallPrompt';
import { ToastContainer } from '../shared/ui/toast';

// Feature screens are lazy-loaded so each tab's JS and CSS are fetched only on
// first visit, splitting the ~300 kB initial bundle into per-feature chunks.
const AuthScreen     = lazy(() => import('../features/auth/components/AuthScreen').then(m => ({ default: m.AuthScreen })));
const FeedScreen     = lazy(() => import('../features/feed/components/FeedScreen').then(m => ({ default: m.FeedScreen })));
const IngestModal    = lazy(() => import('../features/ingest/components/IngestModal').then(m => ({ default: m.IngestModal })));
const JobsScreen     = lazy(() => import('../features/jobs/components/JobsScreen').then(m => ({ default: m.JobsScreen })));
const SavedScreen    = lazy(() => import('../features/saved/components/SavedScreen').then(m => ({ default: m.SavedScreen })));
const SettingsScreen = lazy(() => import('../features/settings/components/SettingsScreen').then(m => ({ default: m.SettingsScreen })));
const AdminScreen    = lazy(() => import('../features/admin/components/AdminScreen').then(m => ({ default: m.AdminScreen })));

export default function App() {
  const [authed, setAuthed] = useState(!!api.getToken());
  const [tab, setTab] = useState('feed');
  const [showIngest, setShowIngest] = useState(false);

  // Track which tabs have been visited. Once a tab mounts it stays mounted and
  // is hidden with display:none instead of being unmounted, so scroll position,
  // loaded data, and sub-tab state survive tab switches.
  const [visited, setVisited] = useState(() => new Set(['feed']));

  // Very basic routing for hidden admin page
  const [route, setRoute] = useState(window.location.pathname);

  useEffect(() => {
    const handlePop = () => setRoute(window.location.pathname);
    window.addEventListener('popstate', handlePop);
    return () => window.removeEventListener('popstate', handlePop);
  }, []);

  // Validate token on mount -- clear stale/invalid tokens (e.g. admin-only JWTs)
  useEffect(() => {
    if (!api.getToken()) return;
    api.getProfile().catch((err) => {
      if (err?.status === 404 || err?.status === 401) {
        api.clearToken();
        setAuthed(false);
      }
    });
  }, []);

  function navigate(path) {
    window.history.pushState({}, '', path);
    setRoute(path);
  }

  if (route === '/admin') {
    return (
      <Suspense fallback={null}>
        <AdminScreen onBack={() => navigate('/')} />
      </Suspense>
    );
  }

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

  function handleTab(newTab) {
    setTab(newTab);
    setVisited(prev => {
      if (prev.has(newTab)) return prev;
      const next = new Set(prev);
      next.add(newTab);
      return next;
    });
  }

  if (!authed) {
    return (
      <Suspense fallback={null}>
        <AuthScreen onAuth={handleAuth} onSkip={handleSkip} />
      </Suspense>
    );
  }

  return (
    <>
      <Suspense fallback={null}>
        {visited.has('feed') && (
          <div style={{ display: tab !== 'feed' ? 'none' : 'block', height: '100%' }}>
            <FeedScreen />
          </div>
        )}
        {visited.has('jobs') && (
          <div style={{ display: tab !== 'jobs' ? 'none' : 'block', height: '100%' }}>
            <JobsScreen />
          </div>
        )}
        {visited.has('saved') && (
          <div style={{ display: tab !== 'saved' ? 'none' : 'block', height: '100%' }}>
            <SavedScreen />
          </div>
        )}
        {visited.has('settings') && (
          <div style={{ display: tab !== 'settings' ? 'none' : 'block', height: '100%' }}>
            <SettingsScreen onLogout={api.getToken() ? handleLogout : null} />
          </div>
        )}
        {showIngest && <IngestModal onClose={() => setShowIngest(false)} />}
      </Suspense>

      <InstallPrompt />
      <ToastContainer />

      <nav className="nav-bar">
        <button className={`nav-btn ${tab === 'feed' ? 'active' : ''}`} onClick={() => handleTab('feed')}>
          <Icons.Home /><span>Feed</span>
        </button>
        <button className={`nav-btn ${tab === 'jobs' ? 'active' : ''}`} onClick={() => handleTab('jobs')}>
          <Icons.Layers /><span>Queue</span>
        </button>
        <button className="nav-add-btn" onClick={() => setShowIngest(true)}>
          <Icons.Plus />
        </button>
        <button className={`nav-btn ${tab === 'saved' ? 'active' : ''}`} onClick={() => handleTab('saved')}>
          <Icons.Bookmark filled={tab === 'saved'} /><span>Saved</span>
        </button>
        <button className={`nav-btn ${tab === 'settings' ? 'active' : ''}`} onClick={() => handleTab('settings')}>
          <Icons.Settings /><span>Tuning</span>
        </button>
      </nav>
    </>
  );
}
