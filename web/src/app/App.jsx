import React, { useState, useEffect } from 'react';
import { AuthScreen } from '../features/auth/components/AuthScreen';
import { FeedScreen } from '../features/feed/components/FeedScreen';
import { IngestModal } from '../features/ingest/components/IngestModal';
import { JobsScreen } from '../features/jobs/components/JobsScreen';
import { SavedScreen } from '../features/saved/components/SavedScreen';
import { SettingsScreen } from '../features/settings/components/SettingsScreen';
import { AdminScreen } from '../features/admin/components/AdminScreen';
import { api } from '../shared/api/clipfeedApi';
import { Icons } from '../shared/ui/icons';
import { InstallPrompt } from '../shared/ui/InstallPrompt';

export default function App() {
  const [authed, setAuthed] = useState(!!api.getToken());
  const [tab, setTab] = useState('feed');
  const [showIngest, setShowIngest] = useState(false);
  
  // Very basic routing for hidden admin page
  const [route, setRoute] = useState(window.location.pathname);

  useEffect(() => {
    const handlePop = () => setRoute(window.location.pathname);
    window.addEventListener('popstate', handlePop);
    return () => window.removeEventListener('popstate', handlePop);
  }, []);

  // Validate token on mount — clear stale/invalid tokens (e.g. admin-only JWTs)
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
    return <AdminScreen onBack={() => navigate('/')} />;
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

      <InstallPrompt />

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
