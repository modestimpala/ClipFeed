import React, { useEffect, useRef, useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { Icons } from '../../../shared/ui/icons';

export const ClipCard = React.forwardRef(function ClipCard({ 
  clip, 
  isActive, 
  shouldRenderVideo,
  isMuted,
  onUnmute,
  onInteract 
}, ref) {
  const videoRef = useRef(null);
  const [playing, setPlaying] = useState(false);
  const [progress, setProgress] = useState(0);
  const [liked, setLiked] = useState(false);
  const [saved, setSaved] = useState(false);
  const [streamUrl, setStreamUrl] = useState(null);
  const startTimeRef = useRef(null);

  useEffect(() => {
    if (!shouldRenderVideo || !clip) return;
    let cancelled = false;
    api.getStreamUrl(clip.id).then((data) => {
      if (!cancelled) setStreamUrl(data.url);
    }).catch(() => {});
    return () => { cancelled = true; };
  }, [clip?.id, shouldRenderVideo]);

  useEffect(() => {
    const video = videoRef.current;
    if (!video) return;

    if (isActive && streamUrl) {
      video.src = streamUrl;
      video.play().then(() => {
        setPlaying(true);
        startTimeRef.current = Date.now();
        onInteract?.(clip.id, 'view');
      }).catch((e) => {
        console.warn("Autoplay prevented:", e);
        setPlaying(false);
      });
    } else {
      video.pause();
      video.removeAttribute('src');
      video.load();
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
    const video = videoRef.current;
    if (!video) return;

    if (isMuted) {
      onUnmute();
      return;
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

  if (!clip) return <div className="clip-card" />;

  return (
    <div className="clip-card" ref={ref} data-clip-id={clip.id} onClick={handleVideoClick}>
      
      {shouldRenderVideo ? (
        <video 
          ref={videoRef} 
          playsInline 
          preload="metadata"
          loop 
          muted={isMuted}
        />
      ) : (
        <div className="video-placeholder" style={{ width: '100%', height: '100%', background: '#000' }} />
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
            <span>{Math.round(clip.duration_seconds)}s</span>
          </div>
        </div>
      </div>
      <div className="clip-actions">
        <button className={`action-btn ${liked ? 'active' : ''}`} onClick={handleLike}><Icons.Heart filled={liked} /></button>
        <button className={`action-btn ${saved ? 'active' : ''}`} onClick={handleSave}><Icons.Bookmark filled={saved} /></button>
        <button className="action-btn" onClick={(e) => e.stopPropagation()}><Icons.Share /></button>
      </div>
      <div className="clip-progress"><div className="clip-progress-bar" style={{ width: `${progress}%` }} /></div>
    </div>
  );
});
