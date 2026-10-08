import { render } from 'preact';
import { useEffect, useMemo, useState } from 'preact/hooks';
import { parse as parseYAML } from 'yaml';
import './style.css';

const icons = {
  overview: '◫',
  agents: '⌘',
  credentials: '◈',
  refresh: '↻',
  search: '⌕',
  copy: '▢',
  shield: '⬡',
};

async function api(path, options) {
  const response = await fetch(path, options);
  const text = await response.text();
  if (!response.ok) {
    let message = text.trim() || `Request failed (${response.status})`;
    try {
      message = JSON.parse(text).error || message;
    } catch {
      // Non-JSON HTTP errors are returned as plain text.
    }
    const error = new Error(message);
    error.status = response.status;
    throw error;
  }
  return text ? JSON.parse(text) : null;
}

function dateTime(value) {
  if (!value) return '—';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString();
}

function shortUID(value = '') {
  return value.length > 16 ? `${value.slice(0, 8)}…${value.slice(-6)}` : value || '—';
}

function App() {
  const [authenticated, setAuthenticated] = useState(null);
  const [loginUsername, setLoginUsername] = useState('');
  const [loginPassword, setLoginPassword] = useState('');
  const [loginBusy, setLoginBusy] = useState(false);
  const [page, setPage] = useState('overview');
  const [agents, setAgents] = useState([]);
  const [tokens, setTokens] = useState([]);
  const [selectedUID, setSelectedUID] = useState('');
  const [query, setQuery] = useState('');
  const [statusFilter, setStatusFilter] = useState('all');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [tokenName, setTokenName] = useState('Linux VM bootstrap');
  const [issuedToken, setIssuedToken] = useState(null);
  const [config, setConfig] = useState('');
  const [savingConfig, setSavingConfig] = useState(false);
  const [configTab, setConfigTab] = useState('desired');
  const [copied, setCopied] = useState(false);

  const selectedAgent = agents.find((agent) => agent.instance_uid === selectedUID) || null;
  const filteredAgents = useMemo(() => agents.filter((agent) => {
    const search = query.trim().toLowerCase();
    const matchesSearch = !search || [
      agent.instance_uid, agent.hostname, agent.os_type, agent.os_description, agent.agent_type,
      agent.agent_version, agent.source_ip,
    ].some((value) => String(value || '').toLowerCase().includes(search));
    return matchesSearch && (statusFilter === 'all' || agent.connected === (statusFilter === 'connected'));
  }), [agents, query, statusFilter]);

  async function refresh(showMessage = false) {
    setBusy(true);
    setError('');
    try {
      const [nextAgents, nextTokens] = await Promise.all([
        api('/api/v1/agents'),
        api('/api/v1/agent-tokens'),
      ]);
      setAuthenticated(true);
      setAgents(nextAgents);
      setTokens(nextTokens);
      if (selectedUID && !nextAgents.some((agent) => agent.instance_uid === selectedUID)) {
        setSelectedUID('');
      }
      if (showMessage) setNotice('Data refreshed.');
    } catch (cause) {
      if (cause.status === 401) {
        setAuthenticated(false);
        setAgents([]);
        setTokens([]);
      } else {
        setError(cause.message);
      }
    } finally {
      setBusy(false);
    }
  }

  useEffect(() => {
    api('/api/v1/session')
      .then((session) => {
        setAuthenticated(session.authenticated);
        if (session.authenticated) refresh();
      })
      .catch((cause) => {
        setAuthenticated(false);
        setError(cause.message);
      });
  }, []);
  useEffect(() => {
    if (!selectedAgent) {
      setConfig('');
      return;
    }
    setConfig(selectedAgent.desired_config || '');
    setConfigTab('desired');
  }, [selectedUID]);
  useEffect(() => {
    if (!notice) return undefined;
    const timeout = setTimeout(() => setNotice(''), 3500);
    return () => clearTimeout(timeout);
  }, [notice]);

  async function createBootstrapToken(event) {
    event.preventDefault();
    setError('');
    try {
      const result = await api('/api/v1/agent-tokens', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: tokenName }),
      });
      setIssuedToken(result);
      setPage('credentials');
      await refresh();
    } catch (cause) {
      setError(cause.message);
    }
  }

  async function issueAgentToken(agent) {
    setError('');
    try {
      const result = await api(`/api/v1/agents/${encodeURIComponent(agent.instance_uid)}/token`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: `Agent ${agent.hostname || agent.instance_uid}` }),
      });
      setIssuedToken(result);
      setPage('credentials');
      await refresh();
    } catch (cause) {
      setError(cause.message);
    }
  }

  async function rotateToken(token) {
    if (!window.confirm(`Rotate "${token.name}"? The existing token will stop working immediately.`)) return;
    setError('');
    try {
      const result = await api(`/api/v1/agent-tokens/${token.id}/rotate`, { method: 'POST' });
      setIssuedToken(result);
      await refresh();
    } catch (cause) {
      setError(cause.message);
    }
  }

  async function revokeToken(token) {
    if (!window.confirm(`Revoke "${token.name}"? Its active agent connection will be disconnected.`)) return;
    setError('');
    try {
      await api(`/api/v1/agent-tokens/${token.id}`, { method: 'DELETE' });
      setIssuedToken(null);
      setNotice('Credential revoked.');
      await refresh();
    } catch (cause) {
      setError(cause.message);
    }
  }

  async function saveConfig() {
    if (!selectedAgent) return;
    setSavingConfig(true);
    setError('');
    try {
      await api(`/api/v1/agents/${encodeURIComponent(selectedAgent.instance_uid)}/config`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ config }),
      });
      setNotice('Desired configuration saved. It will be offered on the agent’s next status report.');
      await refresh();
    } catch (cause) {
      setError(cause.message);
    } finally {
      setSavingConfig(false);
    }
  }

  async function copyToken() {
    if (!issuedToken?.token) return;
    try {
      await navigator.clipboard.writeText(issuedToken.token);
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    } catch {
      setError('Clipboard access failed. Select and copy the token manually.');
    }
  }

  async function login(event) {
    event.preventDefault();
    setLoginBusy(true);
    setError('');
    try {
      await api('/api/v1/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ username: loginUsername, password: loginPassword }),
      });
      setLoginPassword('');
      setAuthenticated(true);
      await refresh();
    } catch (cause) {
      setError(cause.message);
    } finally {
      setLoginBusy(false);
    }
  }

  async function logout() {
    setError('');
    try {
      await api('/api/v1/logout', { method: 'POST' });
      setAuthenticated(false);
      setAgents([]);
      setTokens([]);
      setSelectedUID('');
      setIssuedToken(null);
    } catch (cause) {
      setError(cause.message);
    }
  }

  if (authenticated === null) {
    return <div class="auth-loading">Loading control plane…</div>;
  }
  if (!authenticated) {
    return <LoginPage username={loginUsername} setUsername={setLoginUsername} password={loginPassword} setPassword={setLoginPassword} onSubmit={login} busy={loginBusy} error={error} />;
  }

  const connectedCount = agents.filter((agent) => agent.connected).length;
  const activeTokens = tokens.filter((token) => !token.revoked_at).length;

  return (
    <div class="layout">
      <aside class="sidebar">
        <a class="brand" href="#" onClick={(event) => { event.preventDefault(); setPage('overview'); }}>
          <span class="brand-mark">O</span>
          <span><strong>OpAMP</strong><small>CONTROL PLANE</small></span>
        </a>
        <div class="nav-label">WORKSPACE</div>
        <nav>
          <NavButton icon={icons.overview} active={page === 'overview'} onClick={() => setPage('overview')}>Overview</NavButton>
          <NavButton icon={icons.agents} active={page === 'agents'} onClick={() => setPage('agents')}>Agents <span class="nav-count">{agents.length}</span></NavButton>
          <NavButton icon={icons.credentials} active={page === 'credentials'} onClick={() => setPage('credentials')}>Credentials</NavButton>
        </nav>
        <div class="sidebar-bottom">
          <div class="connection-card"><span class="live-dot" /><span><strong>Control plane online</strong><small>Staging environment</small></span></div>
          <div class="sidebar-foot">{icons.shield} Signed operator session · expires after 12 hours</div>
        </div>
      </aside>
      <main class="main">
        <header class="topbar">
          <div class="breadcrumbs"><span>Workspace</span><b>/</b><strong>{pageTitle(page)}</strong></div>
          <div class="top-actions">
            <span class="endpoint"><span class="live-dot" /> OpAMP <code>ws://</code></span>
            <button class="icon-button" title="Refresh data" onClick={() => refresh(true)} disabled={busy}>{icons.refresh}</button>
            <button class="button button-small" onClick={logout}>Log out</button>
            <div class="avatar">OP</div>
          </div>
        </header>
        <div class="content">
          {(error || notice) && <div class={`banner ${error ? 'banner-error' : 'banner-notice'}`} role="status">{error || notice}<button onClick={() => { setError(''); setNotice(''); }}>×</button></div>}
          {page === 'overview' && <Overview agents={agents} connectedCount={connectedCount} activeTokens={activeTokens} onAgents={() => setPage('agents')} onCredentials={() => setPage('credentials')} onSelect={(agent) => { setSelectedUID(agent.instance_uid); setPage('agents'); }} />}
          {page === 'agents' && (
            <AgentsPage
              agents={filteredAgents} allCount={agents.length} connectedCount={connectedCount}
              query={query} setQuery={setQuery} statusFilter={statusFilter} setStatusFilter={setStatusFilter}
              selectedAgent={selectedAgent} selectedUID={selectedUID}
              onSelect={(agent) => setSelectedUID(agent.instance_uid)}
              onIssueToken={issueAgentToken}
              config={config} setConfig={setConfig} onSaveConfig={saveConfig} savingConfig={savingConfig}
              configTab={configTab} setConfigTab={setConfigTab}
            />
          )}
          {page === 'credentials' && (
            <CredentialsPage
              tokens={tokens} tokenName={tokenName} setTokenName={setTokenName}
              onCreate={createBootstrapToken} onRotate={rotateToken} onRevoke={revokeToken}
              issuedToken={issuedToken} setIssuedToken={setIssuedToken} onCopy={copyToken} copied={copied}
            />
          )}
        </div>
        <footer class="footer"><span>OpAMP Centralized Control Plane</span><span>Agent credentials are sensitive · plaintext WebSocket staging mode</span></footer>
      </main>
    </div>
  );
}

function LoginPage({ username, setUsername, password, setPassword, onSubmit, busy, error }) {
  return (
    <main class="login-shell">
      <form class="login-card" onSubmit={onSubmit}>
        <div class="login-brand"><span class="brand-mark">O</span><span><strong>OpAMP</strong><small>CONTROL PLANE</small></span></div>
        <div class="eyebrow">OPERATOR ACCESS</div>
        <h1>Sign in</h1>
        <p>Sign in to manage your OpAMP agents and configurations.</p>
        {error && <div class="banner banner-error" role="alert">{error}</div>}
        <label class="login-field"><span>Username</span><input autoComplete="username" required value={username} onInput={(event) => setUsername(event.currentTarget.value)} /></label>
        <label class="login-field"><span>Password</span><input type="password" autoComplete="current-password" required value={password} onInput={(event) => setPassword(event.currentTarget.value)} /></label>
        <button class="button button-primary login-submit" type="submit" disabled={busy}>{busy ? 'Signing in…' : 'Sign in'}</button>
        <small class="login-foot">Use the admin credentials configured in the server environment.</small>
      </form>
    </main>
  );
}

function pageTitle(page) {
  return ({ overview: 'Overview', agents: 'Agent fleet', credentials: 'Credentials' })[page] || 'Overview';
}

function NavButton({ icon, active, onClick, children }) {
  return <button class={`nav-button ${active ? 'active' : ''}`} onClick={onClick}><span class="nav-icon">{icon}</span>{children}</button>;
}

function PageHeading({ eyebrow, title, description, action }) {
  return <div class="page-heading"><div><div class="eyebrow">{eyebrow}</div><h1>{title}</h1><p>{description}</p></div>{action}</div>;
}

function Overview({ agents, connectedCount, activeTokens, onAgents, onCredentials, onSelect }) {
  const recent = [...agents].sort((a, b) => new Date(b.last_seen) - new Date(a.last_seen)).slice(0, 5);
  return (
    <>
      <PageHeading eyebrow="CONTROL PLANE" title="Overview" description="Monitor connected Collector agents and manage their remote configuration." action={<button class="button button-primary" onClick={onAgents}>View agents <span>→</span></button>} />
      <div class="notice"><span class="notice-icon">!</span><div><strong>Staging transport is not encrypted</strong><p>Agent tokens and telemetry use plaintext <code>ws://</code>. Keep this deployment on a trusted, isolated network.</p></div></div>
      <div class="metrics-grid">
        <Metric label="Total agents" value={agents.length} caption="Registered instances" icon="⌘" tone="blue" />
        <Metric label="Connected now" value={connectedCount} caption={`${agents.length - connectedCount} currently offline`} icon="↗" tone="green" />
        <Metric label="Active credentials" value={activeTokens} caption="Issued bearer tokens" icon="◈" tone="purple" />
        <Metric label="Desired configs" value={agents.filter((agent) => agent.desired_config).length} caption="Agents with saved config" icon="≋" tone="amber" />
      </div>
      <section class="panel">
        <div class="panel-heading"><div><h2>Recently seen agents</h2><p>Latest reported instance status</p></div><button class="button button-quiet" onClick={onAgents}>All agents <span>→</span></button></div>
        {recent.length ? <AgentTable agents={recent} compact onSelect={onSelect} /> : <EmptyState icon="⌘" title="No agents connected yet" description="Create a bootstrap credential and configure an OpAMP Supervisor to get started." action={<button class="button button-primary" onClick={onCredentials}>Manage credentials</button>} />}
      </section>
      <div class="quick-grid">
        <button class="quick-card" onClick={onCredentials}><span class="quick-icon purple">◈</span><span><strong>Provision an agent</strong><small>Create a single-use-display bootstrap credential</small></span><b>→</b></button>
        <div class="quick-card"><span class="quick-icon blue">≋</span><span><strong>Remote configuration</strong><small>Review desired and agent-reported config per instance</small></span><b>↗</b></div>
      </div>
    </>
  );
}

function Metric({ label, value, caption, icon, tone }) {
  return <div class="metric-card"><div class="metric-top"><span>{label}</span><span class={`metric-icon ${tone}`}>{icon}</span></div><strong class="metric-value">{value}</strong><small>{caption}</small></div>;
}

function AgentsPage(props) {
  const { agents, allCount, connectedCount, query, setQuery, statusFilter, setStatusFilter, selectedAgent, selectedUID, onSelect, onIssueToken, config, setConfig, onSaveConfig, savingConfig, configTab, setConfigTab } = props;
  return (
    <>
      <PageHeading eyebrow="FLEET MANAGEMENT" title="Agent fleet" description="Inspect agent health, reported configurations, and manage instance credentials." action={<span class="page-stat"><i class="live-dot" /> {connectedCount} / {allCount} connected</span>} />
      <section class="panel">
        <div class="table-toolbar"><label class="search-box"><span>{icons.search}</span><input value={query} onInput={(event) => setQuery(event.currentTarget.value)} placeholder="Search hostname, UID, OS, IP…" /></label><select value={statusFilter} onChange={(event) => setStatusFilter(event.currentTarget.value)}><option value="all">All statuses</option><option value="connected">Connected</option><option value="disconnected">Disconnected</option></select><span class="results-count">{agents.length} shown</span></div>
        {agents.length ? <AgentTable agents={agents} selectedUID={selectedUID} onSelect={onSelect} /> : <EmptyState icon="⌘" title="No matching agents" description={allCount ? 'Try a different search or status filter.' : 'Agents will appear here after they connect with a valid credential.'} />}
      </section>
      {selectedAgent && (
        <section class="panel detail-panel">
          <div class="panel-heading detail-heading"><div><div class="eyebrow">SELECTED INSTANCE</div><h2>{selectedAgent.hostname || shortUID(selectedAgent.instance_uid)}</h2><p class="mono muted">{selectedAgent.instance_uid}</p></div><button class="button button-secondary" onClick={() => onIssueToken(selectedAgent)}>＋ Issue agent token</button></div>
          <div class="agent-facts">
            <Fact label="Connection" value={<Status connected={selectedAgent.connected} />} />
            <Fact label="Operating system" value={<>{selectedAgent.os_description || selectedAgent.os_type || '—'}{selectedAgent.os_description && selectedAgent.os_type && <small class="fact-sub">{selectedAgent.os_type}</small>}</>} />
            <Fact label="Agent version" value={selectedAgent.agent_version || '—'} />
            <Fact label="Agent type" value={selectedAgent.agent_type || '—'} />
            <Fact label="Source IP" value={selectedAgent.source_ip || '—'} />
            <Fact label="Last seen" value={dateTime(selectedAgent.last_seen)} />
          </div>
          <div class="tabs"><button class={configTab === 'desired' ? 'selected' : ''} onClick={() => setConfigTab('desired')}>Desired config</button><button class={configTab === 'reported' ? 'selected' : ''} onClick={() => setConfigTab('reported')}>Effective config</button><button class={configTab === 'pipeline' ? 'selected' : ''} onClick={() => setConfigTab('pipeline')}>Pipeline</button><button class={configTab === 'health' ? 'selected' : ''} onClick={() => setConfigTab('health')}>Health & status</button></div>
          {configTab === 'desired' && <div class="config-editor"><div class="config-label"><div><strong>Collector YAML</strong><small>Saved configuration is offered to the agent on its next status report.</small></div><button class="button button-primary" disabled={savingConfig} onClick={onSaveConfig}>{savingConfig ? 'Saving…' : 'Save desired config'}</button></div><textarea spellcheck="false" value={config} onInput={(event) => setConfig(event.currentTarget.value)} placeholder={'receivers:\n  otlp:\n    protocols:\n      grpc:\n'} /></div>}
          {configTab === 'reported' && <ReportedConfig files={selectedAgent.reported_config_files} />}
          {configTab === 'pipeline' && <PipelineView files={selectedAgent.reported_config_files} />}
          {configTab === 'health' && <div class="json-panels"><JsonPanel title="Agent health" value={selectedAgent.health} /><JsonPanel title="Remote config status" value={selectedAgent.remote_config_status} /></div>}
        </section>
      )}
    </>
  );
}

function AgentTable({ agents, selectedUID, onSelect, compact = false }) {
  return (
    <div class="table-wrap"><table class="data-table"><thead><tr><th>AGENT</th><th>OS / VERSION</th><th>TYPE</th><th>SOURCE IP</th><th>STATUS</th><th>LAST SEEN</th><th>CONFIG</th><th /></tr></thead><tbody>
      {agents.map((agent) => <tr key={agent.instance_uid} class={selectedUID === agent.instance_uid ? 'row-selected' : ''}>
        <td><button class="agent-name" onClick={() => onSelect(agent)}><span class={`agent-avatar ${agent.connected ? 'online' : ''}`}>{(agent.hostname || 'A').slice(0, 1).toUpperCase()}</span><span><strong>{agent.hostname || shortUID(agent.instance_uid)}</strong><small class="mono">{shortUID(agent.instance_uid)}</small></span></button></td>
        <td><strong class="regular">{agent.os_description || agent.os_type || '—'}</strong><small class="cell-sub">{[agent.os_type, agent.agent_version && `v${agent.agent_version}`].filter(Boolean).join(' · ') || 'OS / version unknown'}</small></td>
        <td>{agent.agent_type || '—'}</td><td class="mono">{agent.source_ip || '—'}</td><td><Status connected={agent.connected} /></td>
        <td>{dateTime(agent.last_seen)}</td><td><span class={`config-status ${agent.desired_config ? 'config-saved' : ''}`}><i />{agent.desired_config ? 'Saved' : 'Not set'}</span></td>
        <td><button class="button button-small" onClick={() => onSelect(agent)}>{compact ? 'Inspect' : selectedUID === agent.instance_uid ? 'Selected' : 'Details'}</button></td>
      </tr>)}
    </tbody></table></div>
  );
}

function Status({ connected }) {
  return <span class={`status ${connected ? 'status-online' : 'status-offline'}`}><i />{connected ? 'Connected' : 'Offline'}</span>;
}

function Fact({ label, value }) {
  return <div class="fact"><small>{label}</small><strong>{value}</strong></div>;
}

function ReportedConfig({ files }) {
  const entries = Object.entries(files || {});
  if (!entries.length) return <div class="empty-inline">No effective config has been reported by this agent yet.</div>;
  return <div class="reported-config">{entries.map(([name, content]) => <div class="reported-file" key={name}><div class="file-label"><span>▤</span>{name || 'collector.yaml'}</div><pre>{content}</pre></div>)}</div>;
}

function PipelineView({ files }) {
  const entries = Object.entries(files || {}).filter(([, content]) => typeof content === 'string' && content.trim());
  const [selectedFile, setSelectedFile] = useState(entries[0]?.[0] || '');
  useEffect(() => {
    if (!entries.some(([name]) => name === selectedFile)) setSelectedFile(entries[0]?.[0] || '');
  }, [files, selectedFile]);

  if (!entries.length) {
    return <div class="empty-inline">No effective config has been reported by this agent yet. The pipeline view uses the agent-reported effective config.</div>;
  }

  const content = entries.find(([name]) => name === selectedFile)?.[1] || '';
  let config;
  try {
    config = parseYAML(content);
  } catch (cause) {
    return <div class="pipeline-error"><strong>Could not parse this agent config</strong><span>{cause.message}</span></div>;
  }

  const pipelines = config?.service?.pipelines;
  if (!pipelines || typeof pipelines !== 'object' || Array.isArray(pipelines) || !Object.keys(pipelines).length) {
    return <div class="pipeline-view">
      <PipelineFileSelect entries={entries} selectedFile={selectedFile} onChange={setSelectedFile} />
      <div class="empty-inline">This config does not define any <code>service.pipelines</code>.</div>
    </div>;
  }

  return (
    <div class="pipeline-view">
      <div class="pipeline-toolbar">
        <div><strong>Agent-reported pipelines</strong><small>Built from the effective configuration reported by the agent.</small></div>
        <PipelineFileSelect entries={entries} selectedFile={selectedFile} onChange={setSelectedFile} />
      </div>
      <div class="pipeline-list">
        {Object.entries(pipelines).map(([name, pipeline]) => (
          <PipelineDiagram key={name} name={name} pipeline={pipeline} />
        ))}
      </div>
    </div>
  );
}

function PipelineFileSelect({ entries, selectedFile, onChange }) {
  if (entries.length < 2) return null;
  return <label class="pipeline-file-select">Config file<select value={selectedFile} onChange={(event) => onChange(event.currentTarget.value)}>{entries.map(([name]) => <option key={name} value={name}>{name || 'collector.yaml'}</option>)}</select></label>;
}

function PipelineDiagram({ name, pipeline }) {
  const stages = [
    { title: 'Receivers', kind: 'receiver', values: normalizeComponentRefs(pipeline?.receivers) },
    { title: 'Processors', kind: 'processor', values: normalizeComponentRefs(pipeline?.processors) },
    { title: 'Exporters', kind: 'exporter', values: normalizeComponentRefs(pipeline?.exporters) },
  ];

  return (
    <section class="pipeline-card">
      <div class="pipeline-title"><span class="pipeline-signal">{name.split('/')[0]}</span><strong>{name}</strong></div>
      <div class="pipeline-flow">
        {stages.map((stage, index) => (
          <div class="pipeline-flow-part" key={stage.kind}>
            {index > 0 && <span class="pipeline-arrow" aria-hidden="true">→</span>}
            <div class="pipeline-stage">
              <div class="pipeline-stage-title">{stage.title}<span>{stage.values.length}</span></div>
              {stage.values.length
                ? stage.values.map((value) => <div class={`pipeline-component ${stage.kind}`} key={value}>{value}</div>)
                : <div class="pipeline-none">{stage.kind === 'processor' ? 'No processors' : `No ${stage.title.toLowerCase()}`}</div>}
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}

function normalizeComponentRefs(value) {
  if (typeof value === 'string') return [value];
  if (!Array.isArray(value)) return [];
  return value.filter((item) => typeof item === 'string');
}

function JsonPanel({ title, value }) {
  return <div class="json-panel"><div class="file-label">{title}</div><pre>{JSON.stringify(value || {}, null, 2)}</pre></div>;
}

function CredentialsPage(props) {
  const { tokens, tokenName, setTokenName, onCreate, onRotate, onRevoke, issuedToken, setIssuedToken, onCopy, copied } = props;
  return (
    <>
      <PageHeading eyebrow="ACCESS MANAGEMENT" title="Credentials" description="Issue, rotate, and revoke per-agent credentials. Secrets are stored as hashes and shown only once." />
      <section class="panel install-guide">
        <div class="panel-heading"><div><h2>Add a Linux agent</h2><p>Follow these steps on a Debian or Ubuntu VM with systemd.</p></div><span class="quick-icon blue">⌘</span></div>
        <ol class="install-steps">
          <li><strong>Create a bootstrap token</strong><span>Use the form below. Keep this page open; the token is displayed only once.</span></li>
          <li><strong>Connect to the agent VM</strong><span>Make sure it can reach the OpAMP server and has <code>git</code>, <code>curl</code>, and <code>sudo</code>.</span></li>
          <li><strong>Clone the installer from GitHub</strong><code class="install-command">git clone https://github.com/feriva22/OPAMP-Centralized.git<br />cd OPAMP-Centralized/agent-init-config/linux</code></li>
          <li><strong>Run the installer with your server endpoint</strong><span>Replace <code>YOUR_OPAMP_SERVER</code> with the DNS name or IP reachable from the VM. Keep the WebSocket path and use the externally reachable port if it differs from <code>4320</code>.</span><code class="install-command">sudo bash ./install-agent.sh "ws://YOUR_OPAMP_SERVER:4320/v1/opamp"</code><span>When prompted, paste the bootstrap token from this page and press Enter. The input is hidden while you type; do not put the token in the command.</span></li>
          <li><strong>Know where and how the token is stored</strong><span>On the agent VM, it is stored in <code>/etc/opamp/supervisor.yaml</code> as <code>server.headers.Authorization</code>. The file is owned by <code>root:opamp</code> with mode <code>0640</code>.</span><pre class="install-example">{`server:
  endpoint: "ws://YOUR_OPAMP_SERVER:4320/v1/opamp"
  headers:
    Authorization: "<TOKEN_SHOWN_ONCE>"`}</pre><span><code>&lt;TOKEN_SHOWN_ONCE&gt;</code> is a placeholder; the installer writes the real token when you paste it at the hidden prompt. On the control plane, PostgreSQL stores a 32-byte SHA-256 hash in <code>agent_tokens.token_hash</code>, not the plaintext. The plaintext is shown only once and cannot be recovered from the server.</span></li>
          <li><strong>Confirm the service is running</strong><code class="install-command">sudo systemctl status opamp-supervisor</code></li>
        </ol>
      </section>
      <div class="notice notice-warning"><span class="notice-icon">{icons.shield}</span><div><strong>Plaintext WebSocket transport</strong><p>Bearer credentials can be intercepted over <code>ws://</code>. Use a trusted, isolated network until TLS is enabled.</p></div></div>
      {issuedToken && <section class="token-reveal"><div class="token-reveal-head"><span class="quick-icon green">✓</span><div><strong>Credential created — copy it now</strong><small>This plaintext will not be shown again.</small></div><button class="icon-button" title="Hide token" onClick={() => setIssuedToken(null)}>×</button></div><div class="token-secret"><code>{issuedToken.token}</code><button class="button button-secondary" onClick={onCopy}>{copied ? 'Copied' : `${icons.copy} Copy token`}</button></div><div class="token-install"><small>Install command (the script securely prompts for the token)</small><code>sudo bash ./install-agent.sh "ws://SERVER_HOST:4320/v1/opamp"</code></div></section>}
      <div class="credential-layout">
        <section class="panel create-panel"><div class="panel-heading"><div><h2>Create bootstrap credential</h2><p>Bind the token to the first agent UID that connects.</p></div><span class="quick-icon purple">◈</span></div><form onSubmit={onCreate}><label class="field-label" for="credential-name">Credential label</label><input id="credential-name" required maxLength={100} value={tokenName} onInput={(event) => setTokenName(event.currentTarget.value)} placeholder="e.g. linux-datacenter-bootstrap" /><button class="button button-primary full-button" type="submit">＋ Create bootstrap token</button></form><div class="helper-note">For a known instance, issue a dedicated credential from its row in the Agent fleet page. Avoid sharing a bootstrap token across machines.</div></section>
        <section class="panel credential-list"><div class="panel-heading"><div><h2>Issued credentials</h2><p>{tokens.filter((token) => !token.revoked_at).length} active · {tokens.filter((token) => token.revoked_at).length} revoked</p></div></div>{tokens.length ? <div class="table-wrap"><table class="data-table credential-table"><thead><tr><th>LABEL</th><th>BOUND INSTANCE</th><th>STATUS</th><th>LAST USED</th><th>ACTIONS</th></tr></thead><tbody>{tokens.map((token) => <tr key={token.id}><td><strong class="regular">{token.name}</strong><small class="cell-sub">Created {dateTime(token.created_at)}</small></td><td class="mono">{token.instance_uid ? shortUID(token.instance_uid) : 'Awaiting first connect'}</td><td><span class={`credential-state ${token.revoked_at ? 'revoked' : 'active'}`}><i />{token.revoked_at ? 'Revoked' : 'Active'}</span></td><td>{dateTime(token.last_used_at)}</td><td>{!token.revoked_at && <div class="table-actions"><button class="button button-small" onClick={() => onRotate(token)}>Rotate</button><button class="button button-small button-danger" onClick={() => onRevoke(token)}>Revoke</button></div>}</td></tr>)}</tbody></table></div> : <EmptyState icon="◈" title="No credentials yet" description="Create a bootstrap credential to enroll the first agent." />}</section>
      </div>
    </>
  );
}

function EmptyState({ icon, title, description, action }) {
  return <div class="empty-state"><span class="empty-icon">{icon}</span><strong>{title}</strong><p>{description}</p>{action}</div>;
}

render(<App />, document.getElementById('app'));
