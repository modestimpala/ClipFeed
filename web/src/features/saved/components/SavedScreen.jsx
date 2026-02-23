import React, { useEffect, useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { Icons } from '../../../shared/ui/icons';

export function SavedScreen() {
  const [clips, setClips] = useState([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    api.getSaved()
      .then((data) => setClips(data.clips || []))
      .catch(console.error)
      .finally(() => setLoading(false));
  }, []);

  function handleRemove(clipId) {
    api.unsaveClip(clipId)
      .then(() => setClips((prev) => prev.filter((c) => c.id !== clipId)))
      .catch(console.error);
  }

  if (loading) {
    return (
      <div className="saved-screen">
        <div className="settings-title">Saved</div>
        <div style={{ color: 'var(--text-dim)', fontSize: 14, padding: 20, textAlign: 'center' }}>Loading...</div>
      </div>
    );
  }

  if (!clips.length) {
    return (
      <div className="saved-screen">
        <div className="settings-title">Saved</div>
        <div className="empty-state" style={{ height: 'auto', padding: '40px 0' }}>
          <h2>Nothing saved yet</h2>
          <p>Tap the bookmark icon on any clip to save it here.</p>
        </div>
      </div>
    );
  }

  return (
    <div className="saved-screen">
      <div className="settings-title">Saved</div>
      <div className="saved-grid">
        {clips.map((clip) => (
          <div key={clip.id} className="saved-card">
            <div className="saved-thumb">
              {clip.thumbnail_key && (
                <img
                  src={`/storage/${clip.thumbnail_key}`}
                  alt={clip.title}
                  loading="lazy"
                />
              )}
              <div className="saved-duration">{Math.round(clip.duration_seconds)}s</div>
            </div>
            <div className="saved-info">
              <div className="saved-title">{clip.title}</div>
              {clip.topics && clip.topics.length > 0 && (
                <div className="saved-topics">
                  {clip.topics.slice(0, 3).map((t) => (
                    <span key={t} className="saved-topic-tag">{t}</span>
                  ))}
                </div>
              )}
            </div>
            <button className="saved-remove" onClick={() => handleRemove(clip.id)} title="Remove">
              <Icons.X />
            </button>
          </div>
        ))}
      </div>
    </div>
  );
}
