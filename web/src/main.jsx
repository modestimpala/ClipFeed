import React from 'react';
import { createRoot } from 'react-dom/client';
import './shared/tokens.css';
import './shared/base.css';
import './shared/empty-state.css';
import './app/nav.css';
import './features/auth/auth.css';
import './features/feed/feed.css';
import './features/ingest/ingest.css';
import './features/jobs/jobs.css';
import './features/saved/saved.css';
import './features/settings/settings.css';
import App from './app/App';

createRoot(document.getElementById('root')).render(<App />);
