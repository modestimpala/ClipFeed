import React, { useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { api } from '../api/clipfeedApi';
import { Icons } from './icons';
import './clip-player-modal.css';

export function ClipPlayerModal({ clip, onClose }) {
  const videoRef = useRef(null);
  const [streamUrl, setStreamUrl] = useState(null);
  const [muted, setMuted] = useState(false);

  useEffect(() => {
    api.getStreamUrl(clip.id)
      .then((data) => { if (data?.url) setStreamUrl(data.url); })
      .catch(console.error);
  }, [clip.id]);

  useEffect(() => {
    const video = videoRef.current;
    if (!video || !streamUrl) return;
    video.muted = true;
    video.src = streamUrl;
    video.load();
    video.play()
      .then(() => { video.muted = muted; })
      .catch(() => {});
  }, [streamUrl]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (videoRef.current) videoRef.current.muted = muted;
  }, [muted]);

  // Close on Escape
  useEffect(() => {
    const handler = (e) => { if (e.key === 'Escape') onClose(); };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [onClose]);

  return createPortal(
    <div className="cpm-backdrop" onClick={(e) => e.target === e.currentTarget && onClose()}>
      <div className="cpm-container">
        <div className="cpm-video-wrap">
          <video
            ref={videoRef}
            className="cpm-video"
            playsInline
            webkit-playsinline="true"
            preload="auto"
            loop
            muted
          />
        </div>

        <div className="cpm-toolbar">
          <button className="cpm-close" onClick={onClose} aria-label="Close">
            <Icons.X />
          </button>
          <div className="cpm-meta">
            <div className="cpm-title">{clip.title}</div>
            {clip.channel_name && <div className="cpm-channel">{clip.channel_name}</div>}
          </div>
          <button className="cpm-mute" onClick={() => setMuted((m) => !m)} aria-label="Toggle mute">
            {muted ? <Icons.VolumeX /> : <Icons.Volume2 />}
          </button>
        </div>
      </div>
    </div>,
    document.body
  );
}
