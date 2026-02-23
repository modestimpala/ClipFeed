import React, { useEffect, useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';
import { JobCard } from './JobCard';

export function JobsScreen() {
  const [jobs, setJobs] = useState([]);

  useEffect(() => {
    api.getJobs().then((data) => setJobs(data.jobs || [])).catch(console.error);
    const interval = setInterval(() => {
      api.getJobs().then((data) => setJobs(data.jobs || [])).catch(() => {});
    }, 5000);
    return () => clearInterval(interval);
  }, []);

  return (
    <div className="jobs-screen">
      <div className="settings-title">Processing Queue</div>
      {jobs.length === 0 && (
        <div style={{ color: 'var(--text-dim)', fontSize: 14, padding: 20, textAlign: 'center' }}>
          No jobs yet. Submit a video URL to get started.
        </div>
      )}
      {jobs.map((job) => <JobCard key={job.id} job={job} />)}
    </div>
  );
}
