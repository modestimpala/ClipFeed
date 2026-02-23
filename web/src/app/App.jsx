import React, { useState } from 'react';
import { AuthScreen } from '../features/auth/components/AuthScreen';
import { FeedScreen } from '../features/feed/components/FeedScreen';
import { IngestModal } from '../features/ingest/components/IngestModal';
import { JobsScreen } from '../features/jobs/components/JobsScreen';
import { SavedScreen } from '../features/saved/components/SavedScreen';
import { SettingsScreen } from '../features/settings/components/SettingsScreen';
import { api } from '../shared/api/clipfeedApi';
import { Icons } from '../shared/ui/icons';
import { InstallPrompt } from '../shared/ui/InstallPrompt';

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
