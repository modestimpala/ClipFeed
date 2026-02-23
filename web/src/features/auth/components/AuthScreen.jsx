import React, { useState } from 'react';
import { api } from '../../../shared/api/clipfeedApi';

export function AuthScreen({ onAuth, onSkip }) {
  const [mode, setMode] = useState('login');
  const [username, setUsername] = useState('');
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);

  async function handleSubmit(e) {
    e.preventDefault();
    setError('');
    setLoading(true);
    try {
      const data = mode === 'register'
        ? await api.register(username, email, password)
        : await api.login(username, password);
      api.setToken(data.token);
      onAuth(data);
    } catch (err) {
      setError(err.error || 'Something went wrong');
    } finally {
      setLoading(false);
    }
  }

  return (
    <div className="auth-screen">
      <div className="auth-logo">Clip<span>Feed</span></div>
      <div className="auth-subtitle">Your feed, your rules</div>
      <form className="auth-form" onSubmit={handleSubmit}>
        <input className="auth-input" placeholder="Username" value={username} onChange={(e) => setUsername(e.target.value)} autoCapitalize="none" />
        {mode === 'register' && (
          <input className="auth-input" type="email" placeholder="Email" value={email} onChange={(e) => setEmail(e.target.value)} />
        )}
        <input className="auth-input" type="password" placeholder="Password" value={password} onChange={(e) => setPassword(e.target.value)} />
        {error && <div className="auth-error">{error}</div>}
        <button className="auth-submit" type="submit" disabled={loading}>
          {loading ? 'Loading...' : mode === 'login' ? 'Sign In' : 'Create Account'}
        </button>
      </form>
      <button className="auth-toggle" onClick={() => setMode(mode === 'login' ? 'register' : 'login')}>
        {mode === 'login' ? <>No account? <span>Sign up</span></> : <>Have an account? <span>Sign in</span></>}
      </button>
      <button className="auth-skip" onClick={onSkip}>Browse without an account</button>
    </div>
  );
}
