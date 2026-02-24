import React, { useCallback, useEffect, useRef, useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { Icons } from '../../../shared/ui/icons';
import { videoCache } from '../../../shared/utils/videoCache';
import { ClipCard } from './ClipCard';

export function FeedScreen() {
  const [clips, setClips] = useState([]);
  const [activeId, setActiveId] = useState(null);
  const [isGlobalMuted, setIsGlobalMuted] = useState(true);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);
  const viewportRef = useRef(null);
  const cardRefs = useRef(new Map());

  const loadFeed = useCallback(() => {
    setLoading(true);
    setError(null);
    api.getFeed().then((data) => {
      const loaded = data.clips || [];
      setClips(loaded);
      if (loaded.length > 0) setActiveId(String(loaded[0].id));
    }).catch((err) => {
      console.error(err);
      setError('Failed to load feed');
    }).finally(() => setLoading(false));
  }, []);

  useEffect(() => { loadFeed(); }, [loadFeed]);

  useEffect(() => {
    const viewport = viewportRef.current;
    if (!viewport || clips.length === 0) return;

    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (entry.isIntersecting) {
            setActiveId(String(entry.target.dataset.clipId));
          }
        }
      },
      { root: viewport, threshold: 0.6 },
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

  const handleToggleMute = useCallback(() => setIsGlobalMuted(m => !m), []);

  const activeIndex = clips.findIndex(c => String(c.id) === activeId);

  // Preload upcoming videos into blob cache
  useEffect(() => {
    if (clips.length === 0 || activeIndex === -1) return;
    for (let i = 1; i <= 2; i++) {
      const next = clips[activeIndex + i];
      if (next) {
        // Skip the API call entirely if already cached or fetching
        if (videoCache.getCachedUrl(next.id)) continue;

        api.getStreamUrl(next.id)
          .then(data => { if (data?.url) videoCache.preload(next.id, data.url); })
          .catch(() => {});
      }
    }
  }, [activeIndex, clips]);

  if (loading) {
    return (
      <div className="feed-loading">
        <div className="feed-spinner" />
      </div>
    );
  }

  if (error) {
    return (
      <div className="empty-state">
        <h2>Something went wrong</h2>
        <p>{error}</p>
        <button className="feed-retry-btn" onClick={loadFeed}>Retry</button>
      </div>
    );
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
      <button className="feed-refresh-btn" onClick={loadFeed} aria-label="Refresh feed">
        <Icons.RefreshCw />
      </button>
      <div className="feed-viewport" ref={viewportRef}>
        {clips.map((clip, index) => {
          // Render video for active card and ±1 neighbors so iOS decoders stay warm
          const isNearActive = Math.abs(index - activeIndex) <= 1;

          return (
            <ClipCard
              key={clip.id}
              clip={clip}
              isActive={String(clip.id) === activeId}
              shouldRenderVideo={isNearActive}
              isMuted={isGlobalMuted}
              onToggleMute={handleToggleMute}
              onInteract={handleInteract}
              ref={(el) => setCardRef(clip.id, el)}
            />
          );
        })}
      </div>
    </div>
  );
}
