import React, { useEffect, useState, useRef } from 'react';
import { api } from '../api/clipfeedApi';
import { Icons } from './icons';

export function CollectionPicker({ clipId, onClose }) {
  const [collections, setCollections] = useState([]);
  const [loading, setLoading] = useState(true);
  const [newName, setNewName] = useState('');
  const [creating, setCreating] = useState(false);
  const [savedMsg, setSavedMsg] = useState(null);
  const timerRef = useRef(null);

  useEffect(() => {
    api.getCollections()
      .then((data) => setCollections(data.collections || []))
      .catch(() => {})
      .finally(() => setLoading(false));
    return () => clearTimeout(timerRef.current);
  }, []);

  function dismiss(msg) {
    setSavedMsg(msg);
    timerRef.current = setTimeout(() => onClose(), 900);
  }

  function handleAdd(col) {
    api.addToCollection(col.id, clipId)
      .then(() => dismiss(`Added to "${col.title}"`))
      .catch(console.error);
  }

  function handleCreate(e) {
    e.preventDefault();
    const title = newName.trim();
    if (!title || creating) return;
    setCreating(true);
    api.createCollection(title, '')
      .then((data) => {
        return api.addToCollection(data.id, clipId).then(() => {
          dismiss(`Added to "${title}"`);
        });
      })
      .catch(console.error)
      .finally(() => setCreating(false));
  }

  return (
    <div className="collection-picker-backdrop" onClick={onClose}>
      <div className="collection-picker" onClick={(e) => e.stopPropagation()}>

        {savedMsg ? (
          <div className="collection-picker-saved">
            <Icons.Check />
            <span>{savedMsg}</span>
          </div>
        ) : (
          <>
            <div className="collection-picker-header">
              <h3>Save to Collection</h3>
              <button className="collection-picker-close" onClick={onClose}>
                <Icons.X />
              </button>
            </div>

            <form className="collection-picker-create" onSubmit={handleCreate}>
              <input
                type="text"
                placeholder="New collection..."
                value={newName}
                onChange={(e) => setNewName(e.target.value)}
                maxLength={60}
              />
              <button type="submit" disabled={!newName.trim() || creating}>
                <Icons.Plus />
              </button>
            </form>

            <div className="collection-picker-list">
              {loading && (
                <div className="collection-picker-empty">Loading...</div>
              )}
              {!loading && collections.length === 0 && (
                <div className="collection-picker-empty">
                  No collections yet. Create one above.
                </div>
              )}
              {collections.map((col) => (
                <button
                  key={col.id}
                  className="collection-picker-item"
                  onClick={() => handleAdd(col)}
                >
                  <span className="collection-picker-icon">
                    <Icons.Folder />
                  </span>
                  <span className="collection-picker-name">{col.title}</span>
                  <span className="collection-picker-count">
                    {col.clip_count} clip{col.clip_count !== 1 ? 's' : ''}
                  </span>
                </button>
              ))}
            </div>
          </>
        )}
      </div>
    </div>
  );
}
