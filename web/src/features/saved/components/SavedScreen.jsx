import React, { useEffect, useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { Icons } from '../../../shared/ui/icons';

function SavedClipsList() {
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
    return <div style={{ color: 'var(--text-dim)', fontSize: 14, padding: 20, textAlign: 'center' }}>Loading...</div>;
  }

  if (!clips.length) {
    return (
      <div className="empty-state" style={{ height: 'auto', padding: '40px 0' }}>
        <h2>Nothing saved yet</h2>
        <p>Tap the bookmark icon on any clip to save it here.</p>
      </div>
    );
  }

  return (
    <div className="saved-grid">
      {clips.map((clip) => (
        <div key={clip.id} className="saved-card">
          <div className="saved-thumb">
            {clip.thumbnail_key && (
              <img src={`/storage/${clip.thumbnail_key}`} alt={clip.title} loading="lazy" />
            )}
            <div className="saved-duration">{Math.round(clip.duration_seconds)}s</div>
          </div>
          <div className="saved-info">
            <div className="saved-title">{clip.title}</div>
            <div className="saved-meta-line">
              {clip.platform && <span className="saved-platform">{clip.platform}</span>}
              {clip.channel_name && <span className="saved-channel">{clip.channel_name}</span>}
            </div>
            {clip.topics && clip.topics.length > 0 && (
              <div className="saved-topics">
                {clip.topics.slice(0, 3).map((t) => (
                  <span key={t} className="saved-topic-tag">{t}</span>
                ))}
              </div>
            )}
          </div>
          <div className="saved-actions">
            {clip.source_url && (
              <button className="saved-source-btn" onClick={() => window.open(clip.source_url, '_blank', 'noopener')} title="Open source">
                <Icons.ExternalLink />
              </button>
            )}
            <button className="saved-remove" onClick={() => handleRemove(clip.id)} title="Remove">
              <Icons.X />
            </button>
          </div>
        </div>
      ))}
    </div>
  );
}

function CollectionDetail({ collection, onBack }) {
  const [clips, setClips] = useState([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    api.getCollectionClips(collection.id)
      .then((data) => setClips(data.clips || []))
      .catch(console.error)
      .finally(() => setLoading(false));
  }, [collection.id]);

  function handleRemove(clipId) {
    api.removeFromCollection(collection.id, clipId)
      .then(() => setClips((prev) => prev.filter((c) => c.id !== clipId)))
      .catch(console.error);
  }

  return (
    <>
      <button className="collection-back-btn" onClick={onBack}>
        <Icons.ChevronLeft />
        <span>Collections</span>
      </button>
      <h2 className="collection-detail-title">{collection.title}</h2>
      {collection.description && <p className="collection-detail-desc">{collection.description}</p>}

      {loading && (
        <div style={{ color: 'var(--text-dim)', fontSize: 14, padding: 20, textAlign: 'center' }}>Loading...</div>
      )}

      {!loading && clips.length === 0 && (
        <div className="empty-state" style={{ height: 'auto', padding: '40px 0' }}>
          <h2>Empty collection</h2>
          <p>Long-press the bookmark on any clip or tap the folder icon to add clips here.</p>
        </div>
      )}

      {!loading && clips.length > 0 && (
        <div className="saved-grid">
          {clips.map((clip) => (
            <div key={clip.id} className="saved-card">
              <div className="saved-thumb">
                {clip.thumbnail_key && (
                  <img src={`/storage/${clip.thumbnail_key}`} alt={clip.title} loading="lazy" />
                )}
                <div className="saved-duration">{Math.round(clip.duration_seconds)}s</div>
              </div>
              <div className="saved-info">
                <div className="saved-title">{clip.title}</div>
                <div className="saved-meta-line">
                  {clip.platform && <span className="saved-platform">{clip.platform}</span>}
                  {clip.channel_name && <span className="saved-channel">{clip.channel_name}</span>}
                </div>
              </div>
              <div className="saved-actions">
                <button className="saved-remove" onClick={() => handleRemove(clip.id)} title="Remove from collection">
                  <Icons.X />
                </button>
              </div>
            </div>
          ))}
        </div>
      )}
    </>
  );
}

function CollectionsList({ onSelect }) {
  const [collections, setCollections] = useState([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    api.getCollections()
      .then((data) => setCollections(data.collections || []))
      .catch(console.error)
      .finally(() => setLoading(false));
  }, []);

  function handleDelete(e, colId) {
    e.stopPropagation();
    if (!confirm('Delete this collection?')) return;
    api.deleteCollection(colId)
      .then(() => setCollections((prev) => prev.filter((c) => c.id !== colId)))
      .catch(console.error);
  }

  if (loading) {
    return <div style={{ color: 'var(--text-dim)', fontSize: 14, padding: 20, textAlign: 'center' }}>Loading...</div>;
  }

  if (!collections.length) {
    return (
      <div className="empty-state" style={{ height: 'auto', padding: '40px 0' }}>
        <h2>No collections yet</h2>
        <p>Long-press the bookmark on any clip or tap the folder icon to create one.</p>
      </div>
    );
  }

  return (
    <div className="collections-grid">
      {collections.map((col) => (
        <button key={col.id} className="collection-card" onClick={() => onSelect(col)}>
          <div className="collection-card-icon"><Icons.Folder /></div>
          <div className="collection-card-info">
            <div className="collection-card-title">{col.title}</div>
            <div className="collection-card-count">
              {col.clip_count} clip{col.clip_count !== 1 ? 's' : ''}
            </div>
          </div>
          <button className="collection-card-delete" onClick={(e) => handleDelete(e, col.id)} title="Delete">
            <Icons.Trash />
          </button>
        </button>
      ))}
    </div>
  );
}

export function SavedScreen() {
  const [tab, setTab] = useState('clips');
  const [selectedCollection, setSelectedCollection] = useState(null);

  return (
    <div className="saved-screen">
      <div className="settings-title">Library</div>

      {!selectedCollection && (
        <div className="saved-tabs">
          <button className={`saved-tab ${tab === 'clips' ? 'active' : ''}`} onClick={() => setTab('clips')}>
            Saved Clips
          </button>
          <button className={`saved-tab ${tab === 'collections' ? 'active' : ''}`} onClick={() => setTab('collections')}>
            Collections
          </button>
        </div>
      )}

      {selectedCollection ? (
        <CollectionDetail
          collection={selectedCollection}
          onBack={() => setSelectedCollection(null)}
        />
      ) : tab === 'clips' ? (
        <SavedClipsList />
      ) : (
        <CollectionsList onSelect={(col) => setSelectedCollection(col)} />
      )}
    </div>
  );
}
