import React, { useEffect, useRef, useState } from 'react';
import { call } from './api.js';

const modes = [ ['auto', 'Auto detect'], ['extract', 'Extract from website'], ['hls', 'HLS playlist'], ['youtube', 'YouTube'], ['file', 'Direct file'] ];
const initial = { url: '', out: '', mode: 'auto', temp: '', concurrency: 8, retries: 3, timeout: 30, userAgent: '', referer: '', auto: true, browser: '', headless: false, captureDuration: 5, playSelector: '' };

function Field({ label, help, ...props }) {
  return <label className="field"><span>{label}</span><input {...props} />{help && <small>{help}</small>}</label>;
}

function Options({ request, set }) {
  const number = (key) => (event) => set(key, Number(event.target.value));
  return <details className="advanced"><summary>More settings</summary>
    <div className="settings-grid">
      <Field label="Parallel HLS downloads" type="number" min="1" max="64" value={request.concurrency} onChange={number('concurrency')} />
      <Field label="Retries" type="number" min="0" max="10" value={request.retries} onChange={number('retries')} />
      <Field label="Timeout (seconds)" type="number" min="1" max="86400" value={request.timeout} onChange={number('timeout')} />
      <Field label="Resume folder" value={request.temp} onChange={(e) => set('temp', e.target.value)} />
      <Field label="User-Agent" placeholder="Default for selected mode" value={request.userAgent} onChange={(e) => set('userAgent', e.target.value)} />
      <Field label="Referer" placeholder="https://example.com/" value={request.referer} onChange={(e) => set('referer', e.target.value)} />
      {request.mode === 'extract' && <>
        <Field label="Chrome / Chromium path" placeholder="Auto-detect" value={request.browser} onChange={(e) => set('browser', e.target.value)} />
        <label className="check"><input type="checkbox" checked={request.headless} onChange={(e) => { set('headless', e.target.checked); if (!e.target.checked) set('playSelector', ''); }} />Capture in background</label>
        {request.headless && <>
          <Field label="Capture duration (seconds)" type="number" min="1" max="3600" value={request.captureDuration} onChange={number('captureDuration')} />
          <Field label="Play button CSS selector" placeholder="Optional" value={request.playSelector} onChange={(e) => set('playSelector', e.target.value)} />
        </>}
      </>}
    </div>
  </details>;
}

function Choices({ state, select, pending }) {
  if (!state.choices?.length) return null;
  return <section className="card choices"><h2>{state.choiceKind === 'quality' ? 'Choose HLS quality' : 'Captured media'}</h2>
    <p className="muted">{state.choiceKind === 'quality' ? 'Select a stream to continue downloading.' : 'Select HLS or a direct file. DASH downloads are unsupported.'}</p>
    <div className="choice-list">{state.choices.map((choice) => <button type="button" className="choice" key={choice.index} disabled={pending || choice.disabled} onClick={() => select(state.choiceID, choice.index)}>
      <span className="choice-badge">{choice.label}</span><span className="choice-url">{choice.url || 'Download this quality'}</span><span aria-hidden="true">→</span>
    </button>)}</div>
  </section>;
}

function Activity({ state }) {
  const log = useRef(null);
  useEffect(() => { if (log.current) log.current.scrollTop = log.current.scrollHeight; }, [state.logs]);
  const percent = state.progress?.match(/Resources:\s*(\d+)%/)?.[1];
  return <section className="card activity">
    <div className="section-heading"><h2>Activity</h2><span className={`status ${state.status === 'Failed' ? 'failed' : ''}`} role="status"><i className={state.active ? 'working' : ''} />{state.status}</span></div>
    <p className="progress-text">{state.progress || (state.active ? 'Working…' : 'Ready to download.')}</p>
    {state.active && <progress aria-label="Download progress" {...(percent ? { value: Number(percent), max: 100 } : {})} />}
    {!!state.logs?.length && <details className="log-details"><summary>Download log</summary><pre ref={log} className="logs" aria-label="Download activity">{state.logs.join('\n')}</pre></details>}
    {state.error && <p className="error" role="alert">{state.error}</p>}
  </section>;
}

export default function App() {
  const [request, setRequest] = useState(initial);
  const [state, setState] = useState({ active: false, status: 'Ready', logs: [], choices: [] });
  const [error, setError] = useState('');
  const [ready, setReady] = useState(false);
  const [pending, setPending] = useState(false);
  const set = (key, value) => setRequest((current) => ({ ...current, [key]: value }));
  const refresh = async () => setState(await call('GetState'));
  useEffect(() => {
    let mounted = true;
    Promise.all([call('Defaults'), call('GetState')]).then(([defaults, current]) => {
      if (mounted) { setRequest({ ...initial, ...defaults }); setState(current); setReady(true); }
    }).catch((e) => { if (mounted) setError(String(e)); });
    return () => { mounted = false; };
  }, []);
  useEffect(() => {
    if (!state.active) return;
    let stopped = false;
    let timer;
    const poll = async () => {
      try { const next = await call('GetState'); if (!stopped) { setState(next); if (next.active) timer = setTimeout(poll, 250); } }
      catch (e) { if (!stopped) setError(String(e)); }
    };
    timer = setTimeout(poll, 250);
    return () => { stopped = true; clearTimeout(timer); };
  }, [state.active]);
  const perform = async (action) => {
    setPending(true); setError('');
    try { await action(); await refresh(); } catch (e) { setError(String(e)); }
    finally { setPending(false); }
  };
  const start = (event) => { event.preventDefault(); perform(() => call('Start', request)); };
  const savePath = () => perform(async () => { const path = await call('SavePath', request.out); if (path) set('out', path); });
  return <main>
    <header><div className="app-icon" aria-hidden="true">↓</div><h1>JavIt Downloader</h1></header>
    <form className="card download-form" onSubmit={start}>
      <fieldset disabled={!ready || pending || state.active}>
        <div className="section-heading"><h2>New download</h2><label className="mode"><span className="sr-only">Download mode</span><select value={request.mode} onChange={(e) => set('mode', e.target.value)}>{modes.map(([value,label]) => <option key={value} value={value}>{label}</option>)}</select></label></div>
        <Field label={request.mode === 'extract' ? 'Website URL' : 'Download URL'} type="url" required placeholder="https://…" value={request.url} onChange={(e) => set('url', e.target.value)} />
        <p className="hint">{request.mode === 'extract' ? 'Play the video in Chrome, then select captured media.' : request.mode === 'auto' ? 'YouTube, HLS or direct files. For webpages, use Extract.' : request.mode === 'file' ? 'Saves the response unchanged using the exact filename.' : 'Saves as MP4. FFmpeg is required; YouTube also needs yt-dlp.'}</p>
        <div className="save-row"><Field label="Save to" required value={request.out} onChange={(e) => set('out', e.target.value)} /><button className="secondary" type="button" onClick={savePath}>Browse…</button></div>
        {!['file','youtube'].includes(request.mode) && <label className="check"><input type="checkbox" checked={request.auto} onChange={(e) => set('auto', e.target.checked)} />Auto-select highest-bandwidth HLS quality</label>}
        <Options request={request} set={set} />
      </fieldset>
      <div className="form-footer"><span className="muted">Never overwrites existing files.</span>{state.active ? <button type="button" className="cancel" disabled={pending || state.status === 'Cancelling…'} onClick={() => perform(() => call('Cancel'))}>Cancel download</button> : <button className="primary" disabled={!ready || pending} type="submit">Start download <span aria-hidden="true">↓</span></button>}</div>
    </form>
    {error && <p className="error" role="alert">{error}</p>}
    <Choices state={state} pending={pending} select={(id,index) => perform(() => call('Select', id, index))} />
    <Activity state={state} />
  </main>;
}
