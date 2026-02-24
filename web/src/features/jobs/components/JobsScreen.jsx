import React, { useEffect, useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { JobCard } from './JobCard';

export function JobsScreen() {
  const [jobs, setJobs] = useState([]);

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
          delayMs = hasActiveJobs(nextJobs) ? 5000 : 15000;
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
      delayMs = 5000;
      tick();
    };

    document.addEventListener('visibilitychange', onVisibilityChange);
    tick();

    return () => {
      cancelled = true;
      if (timeoutId) clearTimeout(timeoutId);
      document.removeEventListener('visibilitychange', onVisibilityChange);
    };
  }, []);

  return (
    <div className="jobs-screen">
      <div className="screen-title">Processing Queue</div>
      {jobs.length === 0 && (
        <div className="loading-text">
          No jobs yet. Submit a video URL to get started.
        </div>
      )}
      {jobs.map((job) => <JobCard key={job.id} job={job} />)}
    </div>
  );
}
