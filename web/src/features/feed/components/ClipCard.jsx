import React, { useEffect, useRef, useState, useCallback } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { Icons } from '../../../shared/ui/icons';
import { videoCache } from '../../../shared/utils/videoCache';

function formatDuration(s) {
  const m = Math.floor(s / 60);
  const sec = Math.round(s % 60);
  return m > 0 ? `${m}m ${sec}s` : `${sec}s`;
}

function formatDate(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' });
}

function platformLabel(url) {
  if (!url) return null;
  try {
    const host = new URL(url).hostname.replace('www.', '');
    if (host.includes('youtube') || host.includes('youtu.be')) return 'YouTube';
    if (host.includes('tiktok')) return 'TikTok';
    if (host.includes('instagram')) return 'Instagram';
    if (host.includes('twitter') || host.includes('x.com')) return 'X';
    if (host.includes('reddit')) return 'Reddit';
    return host;
  } catch { return null; }
}

export const ClipCard = React.forwardRef(function ClipCard({ 
  clip, 
  isActive, 
  shouldRenderVideo,
  isMuted,
  onUnmute,
  onRequireMute,
  onInteract 
}, ref) {
  const videoRef = useRef(null);
  const [playing, setPlaying] = useState(false);
  const [progress, setProgress] = useState(0);
  const [liked, setLiked] = useState(false);
  const [saved, setSaved] = useState(false);
  const [streamUrl, setStreamUrl] = useState(null);
  const [showInfo, setShowInfo] = useState(false);
  const startTimeRef = useRef(null);

  // Fetch stream URL — use cached blob instantly, otherwise stream immediately
  // and kick off a background blob fetch for next time
  useEffect(() => {
    if (!shouldRenderVideo || !clip) return;
    let cancelled = false;

    // Check blob cache first (instant if preloaded)
    const cached = videoCache.getCachedUrl(clip.id);
    if (cached) {
      setStreamUrl(cached);
      return;
    }

    // Not cached — fetch the presigned URL and use it for immediate streaming
    api.getStreamUrl(clip.id)
      .then((data) => {
        if (cancelled || !data.url) return;
        // Set the network URL right away so playback starts immediately
        setStreamUrl(data.url);
        // Background: download the blob for future instant playback
        videoCache.fetchAndCache(clip.id, data.url).then((blobUrl) => {
          if (!cancelled && blobUrl) setStreamUrl(blobUrl);
        });
      })
      .catch(() => {});

    return () => { cancelled = true; };
  }, [clip?.id, shouldRenderVideo]);

  // Cleanup: release iOS hardware decoder on unmount
  useEffect(() => {
    const video = videoRef.current;
    return () => {
      if (video) {
        video.pause();
        video.removeAttribute('src');
        video.load();
      }
    };
  }, []);

  // Handle playback & source assignment
  useEffect(() => {
    const video = videoRef.current;
    if (!video) return;

    let onReady = null;

    if (isActive && streamUrl) {
      const startPlayback = () => {
        video.muted = isMuted;
        const playPromise = video.play();
        if (playPromise !== undefined) {
          playPromise.then(() => {
            setPlaying(true);
            startTimeRef.current = Date.now();
            onInteract?.(clip.id, 'view');
          }).catch((e) => {
            console.warn("Autoplay prevented:", e);
            if (!isMuted) {
              onRequireMute?.();
              return;
            }
            setPlaying(false);
          });
        }
      };

      video.src = streamUrl;
      video.load();

      if (video.readyState >= 2) {
        startPlayback();
      } else {
        onReady = () => {
          video.removeEventListener('loadeddata', onReady);
          startPlayback();
        };
        video.addEventListener('loadeddata', onReady);
      }
    } else {
      video.pause();
      // Aggressively free iOS hardware decoders when inactive
      if (!isActive) {
        video.removeAttribute('src');
        video.load();
      }
      setPlaying(false);
      setProgress(0);
      if (startTimeRef.current && clip) {
        const watched = (Date.now() - startTimeRef.current) / 1000;
        const pct = clip.duration_seconds > 0 ? watched / clip.duration_seconds : 0;
        if (pct >= 0.9) onInteract?.(clip.id, 'watch_full', watched, Math.min(pct, 1));
        startTimeRef.current = null;
      }
    }

    return () => {
      if (onReady) {
        video.removeEventListener('loadeddata', onReady);
      }
    };
  }, [isActive, streamUrl, clip, onInteract, isMuted, onRequireMute]);

  useEffect(() => {
    if (!isActive) setShowInfo(false);
  }, [isActive]);

  // Progress tracking & loop handling
  useEffect(() => {
    const video = videoRef.current;
    if (!video) return;
    const onTime = () => { if (video.duration) setProgress((video.currentTime / video.duration) * 100); };
    const onEnd = () => {
      video.currentTime = 0;
      const playPromise = video.play();
      if (playPromise !== undefined) playPromise.catch(() => {});
    };
    video.addEventListener('timeupdate', onTime);
    video.addEventListener('ended', onEnd);
    return () => {
      video.removeEventListener('timeupdate', onTime);
      video.removeEventListener('ended', onEnd);
    };
  }, []);

  function handleVideoClick() {
    if (showInfo) {
      setShowInfo(false);
      return;
    }
    const video = videoRef.current;
    if (!video) return;

    if (isMuted) {
      // Set muted=false directly on DOM node inside the click handler
      // so iOS WebKit treats it as a synchronous user gesture for audio
      video.muted = false;
      onUnmute();
    }

    if (video.paused) {
      video.play().then(() => setPlaying(true)).catch(() => setPlaying(false));
    } else {
      video.pause();
      setPlaying(false);
    }
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

  const handleToggleInfo = useCallback((e) => {
    e.stopPropagation();
    setShowInfo((v) => !v);
  }, []);

  const handleOpenSource = useCallback((e) => {
    e.stopPropagation();
    if (clip?.source_url) window.open(clip.source_url, '_blank', 'noopener');
  }, [clip?.source_url]);

  if (!clip) return <div className="clip-card" />;

  const topics = clip.topics || [];
  const tags = clip.tags || [];
  const sourceHost = platformLabel(clip.source_url);

  return (
    <div className="clip-card" ref={ref} data-clip-id={clip.id} onClick={handleVideoClick}>
      
      {/* Always keep <video> in DOM to preserve iOS unmute gesture tokens.
         Toggle visibility via CSS instead of unmounting. */}
      <video
        ref={videoRef}
        style={{ display: shouldRenderVideo ? 'block' : 'none', width: '100%', height: '100%', objectFit: 'cover' }}
        playsInline
        webkit-playsinline="true"
        disablePictureInPicture
        disableRemotePlayback
        preload="none"
        loop
        muted={isMuted}
        poster={clip.thumbnail_key ? `/storage/${clip.thumbnail_key}` : undefined}
      />
      {!shouldRenderVideo && (
        <div className="video-placeholder" style={{ width: '100%', height: '100%', background: '#000', position: 'absolute', top: 0, left: 0 }} />
      )}

      {isActive && isMuted && (
        <div className="unmute-overlay" style={{
          position: 'absolute', top: '50%', left: '50%', transform: 'translate(-50%, -50%)',
          background: 'rgba(0,0,0,0.6)', padding: '12px 24px', borderRadius: '30px',
          color: 'white', fontWeight: 'bold', pointerEvents: 'none', zIndex: 10
        }}>
          Tap to Unmute
        </div>
      )}

      <div className="clip-overlay">
        <div className="clip-info">
          <div className="clip-title">{clip.title}</div>
          <div className="clip-source">
            {clip.platform && <span className="platform-badge">{clip.platform}</span>}
            {clip.channel_name && <span>{clip.channel_name}</span>}
            <span>{formatDuration(clip.duration_seconds)}</span>
          </div>
        </div>
      </div>

      <div className="clip-actions">
        <button className={`action-btn ${liked ? 'active' : ''}`} onClick={handleLike}><Icons.Heart filled={liked} /></button>
        <button className={`action-btn ${saved ? 'active' : ''}`} onClick={handleSave}><Icons.Bookmark filled={saved} /></button>
        <button className={`action-btn ${showInfo ? 'active' : ''}`} onClick={handleToggleInfo}><Icons.Info /></button>
        {clip.source_url && (
          <button className="action-btn" onClick={handleOpenSource}><Icons.ExternalLink /></button>
        )}
        <button className="action-btn" onClick={(e) => e.stopPropagation()}><Icons.Share /></button>
      </div>

      {showInfo && (
        <div className="clip-info-panel" onClick={(e) => e.stopPropagation()}>
          <div className="clip-info-panel-handle" onClick={handleToggleInfo}>
            <div className="clip-info-panel-bar" />
          </div>

          <div className="clip-info-panel-body">
            <h3 className="clip-info-panel-title">{clip.title}</h3>

            {clip.description && (
              <p className="clip-info-panel-desc">{clip.description}</p>
            )}

            <div className="clip-info-panel-meta">
              {clip.channel_name && (
                <div className="meta-row">
                  <span className="meta-label">Channel</span>
                  <span className="meta-value">{clip.channel_name}</span>
                </div>
              )}
              {clip.platform && (
                <div className="meta-row">
                  <span className="meta-label">Platform</span>
                  <span className="meta-value">{clip.platform}</span>
                </div>
              )}
              <div className="meta-row">
                <span className="meta-label">Duration</span>
                <span className="meta-value">{formatDuration(clip.duration_seconds)}</span>
              </div>
              {clip.created_at && (
                <div className="meta-row">
                  <span className="meta-label">Added</span>
                  <span className="meta-value">{formatDate(clip.created_at)}</span>
                </div>
              )}
            </div>

            {topics.length > 0 && (
              <div className="clip-info-panel-tags">
                <span className="meta-label">Topics</span>
                <div className="tag-list">
                  {topics.map((t) => <span key={t} className="info-tag">{t}</span>)}
                </div>
              </div>
            )}

            {tags.length > 0 && (
              <div className="clip-info-panel-tags">
                <span className="meta-label">Tags</span>
                <div className="tag-list">
                  {tags.map((t) => <span key={t} className="info-tag tag-secondary">{t}</span>)}
                </div>
              </div>
            )}

            {clip.source_url && (
              <button className="source-link-btn" onClick={handleOpenSource}>
                <Icons.ExternalLink />
                <span>Open on {sourceHost || 'source'}</span>
              </button>
            )}
          </div>
        </div>
      )}

      <div className="clip-progress"><div className="clip-progress-bar" style={{ width: `${progress}%` }} /></div>
    </div>
  );
});
