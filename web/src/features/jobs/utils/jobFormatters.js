export { timeAgo } from '../../../shared/utils/formatters';

export function formatDuration(startStr, endStr) {
  if (!startStr) return null;
  const end = endStr ? new Date(endStr) : new Date();
  const seconds = Math.floor((end - new Date(startStr)) / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  const secs = seconds % 60;
  return `${minutes}m ${secs}s`;
}

export function displayUrl(url) {
  if (!url) return null;
  try {
    const u = new URL(url);
    const path = u.pathname.length > 30 ? `${u.pathname.slice(0, 30)}…` : u.pathname;
    return u.hostname.replace('www.', '') + path;
  } catch {
    return url.length > 50 ? `${url.slice(0, 50)}…` : url;
  }
}

export function summarizeError(error) {
  if (!error) return null;
  if (error.includes('403: Forbidden')) return 'Access denied (403) — try adding cookies in Tuning → Platform Cookies';
  if (error.includes('404')) return 'Video not found (404) — link may be broken or removed';
  if (error.includes('429')) return 'Rate limited — too many requests, will retry';
  if (error.includes('nsig extraction failed')) return 'Download blocked — try adding cookies in Tuning → Platform Cookies';
  if (error.includes('Unsupported URL')) return 'URL not supported — try a different link';
  if (error.includes('Video unavailable')) return 'Video unavailable — may be deleted or private';
  const firstLine = error.split('\n').pop().trim();
  return firstLine.length > 120 ? `${firstLine.slice(0, 120)}…` : firstLine;
}

export const STATUS_LABELS = {
  queued: 'Queued',
  running: 'Processing',
  complete: 'Done',
  failed: 'Failed',
};
