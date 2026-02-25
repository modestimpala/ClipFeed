import React from 'react';

/**
 * Top-level error boundary that catches unhandled render errors and shows a
 * recovery UI instead of a blank screen.
 */
export class ErrorBoundary extends React.Component {
  constructor(props) {
    super(props);
    this.state = { hasError: false, error: null };
  }

  static getDerivedStateFromError(error) {
    return { hasError: true, error };
  }

  componentDidCatch(error, info) {
    console.error('[ErrorBoundary]', error, info?.componentStack);
  }

  handleRetry = () => {
    this.setState({ hasError: false, error: null });
  };

  handleReload = () => {
    window.location.reload();
  };

  render() {
    if (this.state.hasError) {
      return (
        <div style={styles.container}>
          <div style={styles.card}>
            <h2 style={styles.title}>Something went wrong</h2>
            <p style={styles.message}>
              {this.state.error?.message || 'An unexpected error occurred.'}
            </p>
            <div style={styles.actions}>
              <button style={styles.btnPrimary} onClick={this.handleRetry}>
                Try Again
              </button>
              <button style={styles.btnSecondary} onClick={this.handleReload}>
                Reload Page
              </button>
            </div>
          </div>
        </div>
      );
    }
    return this.props.children;
  }
}

const styles = {
  container: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    minHeight: '100dvh',
    padding: '1rem',
    background: 'var(--bg, #0a0a0a)',
    color: 'var(--text, #e0e0e0)',
    fontFamily: 'system-ui, sans-serif',
  },
  card: {
    maxWidth: '400px',
    textAlign: 'center',
    padding: '2rem',
  },
  title: {
    fontSize: '1.25rem',
    fontWeight: 600,
    marginBottom: '0.75rem',
  },
  message: {
    fontSize: '0.875rem',
    opacity: 0.7,
    marginBottom: '1.5rem',
    lineHeight: 1.5,
    wordBreak: 'break-word',
  },
  actions: {
    display: 'flex',
    gap: '0.75rem',
    justifyContent: 'center',
  },
  btnPrimary: {
    padding: '0.5rem 1.25rem',
    borderRadius: '8px',
    border: 'none',
    background: 'var(--accent, #6366f1)',
    color: '#fff',
    fontWeight: 600,
    cursor: 'pointer',
  },
  btnSecondary: {
    padding: '0.5rem 1.25rem',
    borderRadius: '8px',
    border: '1px solid var(--border, #333)',
    background: 'transparent',
    color: 'var(--text, #e0e0e0)',
    cursor: 'pointer',
  },
};
