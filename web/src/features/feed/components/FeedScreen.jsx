import React, { useCallback, useEffect, useRef, useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { ClipCard } from './ClipCard';

export function FeedScreen() {
  const [clips, setClips] = useState([]);
  const [activeId, setActiveId] = useState(null);
  const viewportRef = useRef(null);
  const cardRefs = useRef(new Map());

  useEffect(() => {
    api.getFeed().then((data) => {
      const loaded = data.clips || [];
      setClips(loaded);
      if (loaded.length > 0) setActiveId(loaded[0].id);
    }).catch(console.error);
  }, []);

  useEffect(() => {
    const viewport = viewportRef.current;
    if (!viewport || clips.length === 0) return;

    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (entry.isIntersecting) {
            setActiveId(entry.target.dataset.clipId);
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
        {clips.map((clip) => (
          <ClipCard
            key={clip.id}
            clip={clip}
            isActive={clip.id === activeId}
            onInteract={handleInteract}
            ref={(el) => setCardRef(clip.id, el)}
          />
        ))}
      </div>
    </div>
  );
}
