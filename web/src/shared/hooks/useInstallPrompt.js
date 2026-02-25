import { useCallback, useEffect, useRef, useState } from 'react';

const DISMISS_KEY = 'clipfeed_install_dismissed';
const DISMISS_DAYS = 14;

function isIOS() {
  return /iPad|iPhone|iPod/.test(navigator.userAgent) && !window.MSStream;
}

function isStandalone() {
  return (
    window.matchMedia('(display-mode: standalone)').matches ||
    navigator.standalone === true
  );
}

function wasDismissedRecently() {
  try {
    const ts = localStorage.getItem(DISMISS_KEY);
    if (!ts) return false;
    const elapsed = Date.now() - Number(ts);
    return elapsed < DISMISS_DAYS * 24 * 60 * 60 * 1000;
  } catch {
    return false;
  }
}

export function useInstallPrompt() {
  const deferredPrompt = useRef(null);
  const [canInstall, setCanInstall] = useState(false);
  const [showIOSGuide, setShowIOSGuide] = useState(false);
  const [installed, setInstalled] = useState(false);
  const [dismissed, setDismissed] = useState(true);

  useEffect(() => {
    if (isStandalone()) {
      setInstalled(true);
      return;
    }

    if (wasDismissedRecently()) {
      setDismissed(true);
    } else {
      setDismissed(false);
    }

    if (isIOS()) {
      setShowIOSGuide(true);
      return;
    }

    function onBeforeInstall(e) {
      e.preventDefault();
      deferredPrompt.current = e;
      setCanInstall(true);
    }

    function onAppInstalled() {
      deferredPrompt.current = null;
      setCanInstall(false);
      setInstalled(true);
    }

    window.addEventListener('beforeinstallprompt', onBeforeInstall);
    window.addEventListener('appinstalled', onAppInstalled);
    return () => {
      window.removeEventListener('beforeinstallprompt', onBeforeInstall);
      window.removeEventListener('appinstalled', onAppInstalled);
    };
  }, []);

  const promptInstall = useCallback(async () => {
    if (!deferredPrompt.current) return;
    deferredPrompt.current.prompt();
    const { outcome } = await deferredPrompt.current.userChoice;
    if (outcome === 'accepted') {
      setInstalled(true);
    }
    deferredPrompt.current = null;
    setCanInstall(false);
  }, []);

  const dismiss = useCallback(() => {
    setDismissed(true);
    try {
      localStorage.setItem(DISMISS_KEY, String(Date.now()));
    } catch { /* quota exceeded -- ignore */ }
  }, []);

  const bannerVisible =
    !installed && !dismissed && (canInstall || showIOSGuide);

  return {
    canInstall,
    showIOSGuide,
    installed,
    bannerVisible,
    promptInstall,
    dismiss,
  };
}
