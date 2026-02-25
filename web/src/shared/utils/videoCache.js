// Blob-based video cache keyed by clip ID.
// Bypasses Safari Range Request issues and enables instant playback
// for preloaded clips while allowing immediate streaming for others.

const MAX_CACHE_SIZE = 8;
const blobCache = new Map();     // clipId → objectUrl
const activeFetches = new Map(); // clipId → Promise<objectUrl>

function manageCacheSize() {
  if (blobCache.size >= MAX_CACHE_SIZE) {
    const oldestId = blobCache.keys().next().value;
    const objectUrl = blobCache.get(oldestId);
    URL.revokeObjectURL(objectUrl);
    blobCache.delete(oldestId);
  }
}

export const videoCache = {
  /**
   * Returns a cached blob URL for the clip if available, null otherwise.
   * Never blocks -- use this to check before falling back to streaming.
   */
  getCachedUrl(clipId) {
    return blobCache.get(clipId) || null;
  },

  /**
   * Fetches a video into a blob and caches it by clip ID.
   * Returns the blob object URL when complete.
   */
  async fetchAndCache(clipId, networkUrl) {
    if (!clipId || !networkUrl) return null;

    if (blobCache.has(clipId)) {
      return blobCache.get(clipId);
    }

    if (activeFetches.has(clipId)) {
      return activeFetches.get(clipId);
    }

    const fetchPromise = fetch(networkUrl)
      .then(res => {
        if (!res.ok) throw new Error('Network response was not ok');
        return res.blob();
      })
      .then(blob => {
        const objectUrl = URL.createObjectURL(blob);
        manageCacheSize();
        blobCache.set(clipId, objectUrl);
        activeFetches.delete(clipId);
        return objectUrl;
      })
      .catch(err => {
        console.warn('Blob cache failed for clip', clipId, err);
        activeFetches.delete(clipId);
        return null;
      });

    activeFetches.set(clipId, fetchPromise);
    return fetchPromise;
  },

  /**
   * Preloads a clip in the background.
   */
  preload(clipId, networkUrl) {
    if (clipId && networkUrl && !blobCache.has(clipId) && !activeFetches.has(clipId)) {
      this.fetchAndCache(clipId, networkUrl).catch(() => {});
    }
  },
};
