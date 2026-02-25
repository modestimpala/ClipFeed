import React, { useEffect, useRef, useState, useCallback } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { Icons } from '../../../shared/ui/icons';
import { CollectionPicker } from '../../../shared/ui/CollectionPicker';
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
  onToggleMute,
  onInteract 
}, ref) {
  const videoRef = useRef(null);
  const [playing, setPlaying] = useState(false);
  const [progress, setProgress] = useState(0);
  const [liked, setLiked] = useState(!!clip?.is_liked);
  const [saved, setSaved] = useState(!!clip?.is_saved);
  const [streamUrl, setStreamUrl] = useState(null);
  const [showInfo, setShowInfo] = useState(false);
  const [showCollections, setShowCollections] = useState(false);
  const startTimeRef = useRef(null);
  const viewFiredRef = useRef(null);

  // Fetch stream URL — use cached blob instantly, otherwise stream directly
  useEffect(() => {
    if (!shouldRenderVideo || !clip) return;
    let cancelled = false;

    const cached = videoCache.getCachedUrl(clip.id);
    if (cached) {
      setStreamUrl(cached);
      return;
    }

    api.getStreamUrl(clip.id)
      .then((data) => {
        if (cancelled || !data.url) return;
        setStreamUrl(data.url);
        // Cache quietly in background for revisits, but don't swap src mid-playback
        videoCache.fetchAndCache(clip.id, data.url).catch(() => {});
      })
      .catch(() => {});
    return () => { cancelled = true; };
  }, [clip?.id, shouldRenderVideo]);

  useEffect(() => {
    const video = videoRef.current;
    if (!video) return;

    if (isActive && streamUrl) {
      const startPlayback = () => {
        video.play().then(() => {
          setPlaying(true);
          startTimeRef.current = Date.now();
          if (viewFiredRef.current !== clip.id) {
            viewFiredRef.current = clip.id;
            onInteract?.(clip.id, 'view');
          }
        }).catch((e) => {
          console.warn("Autoplay prevented:", e);
          setPlaying(false);
        });
      };

      video.src = streamUrl;
      // iOS requires waiting for data before play() to avoid black frames
      if (video.readyState >= 2) {
        startPlayback();
      } else {
        const onReady = () => {
          video.removeEventListener('loadeddata', onReady);
          startPlayback();
        };
        video.addEventListener('loadeddata', onReady);
        video.load();
      }
    } else {
      video.pause();
      video.src = '';
      setPlaying(false);
      setProgress(0);
      if (startTimeRef.current && clip) {
        const watched = (Date.now() - startTimeRef.current) / 1000;
        const pct = clip.duration_seconds > 0 ? watched / clip.duration_seconds : 0;
        if (pct >= 0.9) onInteract?.(clip.id, 'watch_full', watched, Math.min(pct, 1));
        startTimeRef.current = null;
      }
    }
  }, [isActive, streamUrl, clip, onInteract]);

  useEffect(() => {
    if (!isActive) setShowInfo(false);
  }, [isActive]);

  useEffect(() => {
    const video = videoRef.current;
    if (!video) return;
    const onTime = () => { if (video.duration) setProgress((video.currentTime / video.duration) * 100); };
    const onEnd = () => { video.currentTime = 0; video.play().catch(() => {}); };
    video.addEventListener('timeupdate', onTime);
    video.addEventListener('ended', onEnd);
    return () => {
      video.removeEventListener('timeupdate', onTime);
      video.removeEventListener('ended', onEnd);
    };
  }, [shouldRenderVideo]);

  function handleVideoClick() {
    if (showInfo) {
      setShowInfo(false);
      return;
    }
    const video = videoRef.current;
    if (!video) return;

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
    if (clip?.source_url) window.open(clip.source_url, '_blank', 'noopener,noreferrer');
  }, [clip?.source_url]);

  if (!clip) return <div className="clip-card" />;

  const topics = clip.topics || [];
  const tags = clip.tags || [];
  const sourceHost = platformLabel(clip.source_url);

  return (
    <div className="clip-card" ref={ref} data-clip-id={clip.id} onClick={handleVideoClick}>
      
      {shouldRenderVideo ? (
        <video 
          ref={videoRef} 
          playsInline 
          webkit-playsinline="true"
          preload="auto"
          loop 
          muted={isMuted}
          poster={clip.thumbnail_url || undefined}
        />
      ) : (
        <div className="video-placeholder" />
      )}

      {isActive && isMuted && (
        <div className="unmute-hint">
          <Icons.VolumeX />
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
        <button
          className="action-btn"
          onClick={(e) => { e.stopPropagation(); onToggleMute(); }}
          aria-label={isMuted ? 'Unmute' : 'Mute'}
        >
          {isMuted ? <Icons.VolumeX /> : <Icons.Volume2 />}
        </button>
        <button className={`action-btn ${liked ? 'active' : ''}`} onClick={handleLike} aria-label={liked ? 'Unlike' : 'Like'}>
          <Icons.Heart filled={liked} />
        </button>
        <button className={`action-btn ${saved ? 'active' : ''}`} onClick={handleSave} aria-label={saved ? 'Unsave' : 'Save'}>
          <Icons.Bookmark filled={saved} />
        </button>
        <button className="action-btn" onClick={(e) => { e.stopPropagation(); setShowCollections(true); }} aria-label="Add to collection">
          <Icons.FolderPlus />
        </button>
        <button className={`action-btn ${showInfo ? 'active' : ''}`} onClick={handleToggleInfo} aria-label="Clip details">
          <Icons.Info />
        </button>
        {clip.source_url && (
          <button className="action-btn" onClick={handleOpenSource} aria-label="Open source">
            <Icons.ExternalLink />
          </button>
        )}
      </div>

      {showCollections && (
        <CollectionPicker clipId={clip.id} onClose={() => setShowCollections(false)} />
      )}

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
