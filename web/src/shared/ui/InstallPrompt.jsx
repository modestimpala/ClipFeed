import React from 'react';
import { useInstallPrompt } from '../hooks/useInstallPrompt';

export function InstallPrompt() {
  const { canInstall, showIOSGuide, bannerVisible, promptInstall, dismiss } =
    useInstallPrompt();

  if (!bannerVisible) return null;

  return (
    <div className="install-banner">
      <button className="install-banner-close" onClick={dismiss} aria-label="Dismiss">
        <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
          <path d="M4 4l8 8M12 4l-8 8" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
        </svg>
      </button>

      <div className="install-banner-icon">
        <svg width="24" height="24" viewBox="0 0 24 24" fill="none">
          <path d="M12 2v13m0 0l-4-4m4 4l4-4" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
          <path d="M4 17v2a2 2 0 002 2h12a2 2 0 002-2v-2" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      </div>

      <div className="install-banner-body">
        <strong className="install-banner-title">Install ClipFeed</strong>

        {canInstall && (
          <>
            <p className="install-banner-desc">
              Add to your home screen for a full-screen, instant-launch experience.
            </p>
            <button className="install-banner-btn" onClick={promptInstall}>
              Install App
            </button>
          </>
        )}

        {showIOSGuide && (
          <p className="install-banner-desc">
            Tap{' '}
            <svg className="install-banner-share-icon" width="15" height="15" viewBox="0 0 24 24" fill="none">
              <path d="M12 2v13m0-13l-4 4m4-4l4 4" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
              <path d="M4 14v5a2 2 0 002 2h12a2 2 0 002-2v-5" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
            </svg>{' '}
            <strong>Share</strong>, then <strong>Add to Home Screen</strong>.
          </p>
        )}
      </div>
    </div>
  );
}
