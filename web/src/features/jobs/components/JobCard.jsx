import React, { useState } from 'react';
import { displayUrl, formatDuration, STATUS_LABELS, summarizeError, timeAgo } from '../utils/jobFormatters';

function formatMetric(value) {
  const num = Number(value);
  if (!Number.isFinite(num) || num <= 0) return null;
  return Intl.NumberFormat('en', { notation: 'compact', maximumFractionDigits: 1 }).format(num);
}

function formatUploadDate(raw) {
  if (!raw || typeof raw !== 'string' || raw.length !== 8) return null;
  const yyyy = raw.slice(0, 4);
  const mm = raw.slice(4, 6);
  const dd = raw.slice(6, 8);
  const d = new Date(`${yyyy}-${mm}-${dd}T00:00:00Z`);
  if (Number.isNaN(d.getTime())) return null;
  return d.toLocaleDateString();
}

export function JobCard({ job }) {
  const [expanded, setExpanded] = useState(false);
  const errorSummary = summarizeError(job.error);
  const elapsed = formatDuration(job.started_at, job.completed_at);
  const sourceMetadata = typeof job.source_metadata === 'object' ? job.source_metadata : null;
  const videoId = job.external_id || sourceMetadata?.id;
  const uploader = job.channel_name || sourceMetadata?.uploader || sourceMetadata?.channel;
  const viewCount = formatMetric(sourceMetadata?.view_count);
  const likeCount = formatMetric(sourceMetadata?.like_count);
  const uploaderFollowers = formatMetric(sourceMetadata?.channel_follower_count || sourceMetadata?.uploader_follower_count);
  const uploadDate = formatUploadDate(sourceMetadata?.upload_date);
  const sourceDuration = sourceMetadata?.duration ? `${Math.round(Number(sourceMetadata.duration))}s` : null;
  const hasMoreDetails = Boolean(
    job.error ||
    job.url ||
    videoId ||
    uploader ||
    job.thumbnail_url ||
    viewCount ||
    likeCount ||
    uploaderFollowers ||
    uploadDate ||
    sourceDuration
  );

  return (
    <div className={`job-card ${job.status === 'failed' && hasMoreDetails ? 'job-card-failed' : ''}`} onClick={() => hasMoreDetails && setExpanded(!expanded)}>
      <div className="job-card-header">
        <div className={`job-status ${job.status}`} />
        <div className="job-info">
          <div className="job-title-row">
            <span className="job-type">{job.title || displayUrl(job.url) || job.job_type}</span>
            {job.platform && <span className="job-platform">{job.platform}</span>}
          </div>
          <div className="job-meta">
            <span className={`job-status-label ${job.status}`}>{STATUS_LABELS[job.status] || job.status}</span>
            <span className="job-meta-sep" />
            <span>{timeAgo(job.created_at)}</span>
            {elapsed && <><span className="job-meta-sep" /><span>{elapsed}</span></>}
            {job.status === 'failed' && job.attempts > 0 && (
              <><span className="job-meta-sep" /><span>attempt {job.attempts}/{job.max_attempts}</span></>
            )}
          </div>
        </div>
      </div>
      {errorSummary && (
        <div className={`job-error ${expanded ? 'job-error-expanded' : ''}`}>
          <div className="job-error-summary">{errorSummary}</div>
          <button
            type="button"
            className="job-error-toggle"
            onClick={(event) => {
              event.stopPropagation();
              setExpanded(!expanded);
            }}
          >
            {expanded ? 'show less' : 'show more'}
          </button>
          {expanded && (
            <div className="job-error-context">
              {uploader && <div><strong>channel:</strong> {uploader}</div>}
              {uploaderFollowers && <div><strong>followers:</strong> {uploaderFollowers}</div>}
              {videoId && <div><strong>video id:</strong> {videoId}</div>}
              {uploadDate && <div><strong>upload date:</strong> {uploadDate}</div>}
              {viewCount && <div><strong>views:</strong> {viewCount}</div>}
              {likeCount && <div><strong>likes:</strong> {likeCount}</div>}
              {sourceDuration && <div><strong>duration:</strong> {sourceDuration}</div>}
              {job.url && <div><strong>source:</strong> {job.url}</div>}
              {job.thumbnail_url && <div><strong>thumbnail:</strong> {job.thumbnail_url}</div>}
              {job.error && job.error !== errorSummary && (
                <pre className="job-error-detail">{job.error}</pre>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
