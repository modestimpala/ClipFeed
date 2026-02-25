import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { JobCard } from './JobCard';

const FILTERS = [
  { key: 'active', label: 'Active' },
  { key: 'all', label: 'All' },
  { key: 'failed', label: 'Failed' },
  { key: 'done', label: 'Done' },
];

function filterJobs(jobs, filter) {
  switch (filter) {
    case 'active':
      return jobs.filter((j) => j.status === 'queued' || j.status === 'running');
    case 'failed':
      return jobs.filter((j) => j.status === 'failed' || j.status === 'rejected' || j.status === 'cancelled');
    case 'done':
      return jobs.filter((j) => j.status === 'complete');
    default:
      return jobs;
  }
}

export function JobsScreen() {
  const [jobs, setJobs] = useState([]);
  const [filter, setFilter] = useState('active');
  const [refreshKey, setRefreshKey] = useState(0);

  const refresh = useCallback(() => setRefreshKey((k) => k + 1), []);

  useEffect(() => {
    let cancelled = false;
    let timeoutId = null;
    let delayMs = 5000;

    const isVisible = () => document.visibilityState === 'visible';
    const hasActiveJobs = (items) => items.some((job) => job.status === 'queued' || job.status === 'running');

    const schedule = (nextDelay) => {
      if (cancelled) return;
      timeoutId = setTimeout(tick, nextDelay);
    };

    const tick = async () => {
      try {
        const data = await api.getJobs();
        if (cancelled) return;

        const nextJobs = data.jobs || [];
        setJobs(nextJobs);

        if (!isVisible()) {
          delayMs = 30000;
        } else {
          delayMs = hasActiveJobs(nextJobs) ? 3000 : 15000;
        }
      } catch {
        delayMs = Math.min(delayMs * 2, 60000);
      } finally {
        schedule(delayMs);
      }
    };

    const onVisibilityChange = () => {
      if (!isVisible()) return;
      if (timeoutId) clearTimeout(timeoutId);
      delayMs = 3000;
      tick();
    };

    document.addEventListener('visibilitychange', onVisibilityChange);
    tick();

    return () => {
      cancelled = true;
      if (timeoutId) clearTimeout(timeoutId);
      document.removeEventListener('visibilitychange', onVisibilityChange);
    };
  }, [refreshKey]);

  const filtered = filterJobs(jobs, filter);
  const activeCount = jobs.filter((j) => j.status === 'queued' || j.status === 'running').length;
  const failedCount = jobs.filter((j) => j.status === 'failed' || j.status === 'rejected' || j.status === 'cancelled').length;

  return (
    <div className="jobs-screen">
      <div className="screen-title">Processing Queue</div>
      <div className="job-filters">
        {FILTERS.map(({ key, label }) => {
          const count = key === 'active' ? activeCount : key === 'failed' ? failedCount : null;
          return (
            <button
              key={key}
              className={`job-filter-btn${filter === key ? ' active' : ''}`}
              onClick={() => setFilter(key)}
            >
              {label}
              {count > 0 && <span className="job-filter-count">{count}</span>}
            </button>
          );
        })}
      </div>
      {filtered.length === 0 && (
        <div className="loading-text">
          {jobs.length === 0
            ? 'No jobs yet. Submit a video URL to get started.'
            : `No ${filter === 'active' ? 'active' : filter} jobs.`}
        </div>
      )}
      {filtered.map((job) => <JobCard key={job.id} job={job} onAction={refresh} />)}
    </div>
  );
}
