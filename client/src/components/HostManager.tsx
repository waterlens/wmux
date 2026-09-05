import {
  Edit3,
  Fingerprint,
  KeyRound,
  LoaderCircle,
  LockKeyhole,
  MoreHorizontal,
  Network,
  Plus,
  Server,
  ShieldAlert,
  ShieldCheck,
  TerminalSquare,
  Trash2,
  Wifi,
} from 'lucide-react';
import { type FormEvent, useId, useState } from 'react';
import { api, errorMessage } from '../api';
import type { AuthType, Host, HostInput, Notify } from '../types';
import { SSHConfigImport } from './SSHConfigImport';
import { ActionMenu, Button, ConfirmDialog, EmptyState, Field, Input, Modal, Textarea } from './UI';

type HostManagerProps = {
  hosts: Host[];
  onHostsChange: (hosts: Host[]) => void;
  onStartSession: (hostId: string) => void;
  notify: Notify;
};

type ProbeResult = {
  host: Host;
  fingerprint: string;
  algorithm: string;
};

export function HostManager({ hosts, onHostsChange, onStartSession, notify }: HostManagerProps) {
  const [editorOpen, setEditorOpen] = useState(false);
  const [editingHost, setEditingHost] = useState<Host | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<Host | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [probing, setProbing] = useState<string | null>(null);
  const [probe, setProbe] = useState<ProbeResult | null>(null);
  const [trusting, setTrusting] = useState(false);
  const [testing, setTesting] = useState<string | null>(null);
  const [openMenuId, setOpenMenuId] = useState<string | null>(null);

  function openCreate() {
    setEditingHost(null);
    setEditorOpen(true);
  }

  function openEdit(host: Host) {
    setEditingHost(host);
    setEditorOpen(true);
  }

  function upsert(host: Host) {
    const exists = hosts.some((item) => item.id === host.id);
    onHostsChange(exists ? hosts.map((item) => (item.id === host.id ? host : item)) : [...hosts, host]);
  }

  async function removeHost() {
    if (!deleteTarget) return;
    setDeleting(true);
    try {
      await api.deleteHost(deleteTarget.id);
      onHostsChange(hosts.filter((host) => host.id !== deleteTarget.id));
      notify(`已删除主机「${deleteTarget.name}」`, 'success');
      setDeleteTarget(null);
    } catch (reason) {
      notify(errorMessage(reason), 'error');
    } finally {
      setDeleting(false);
    }
  }

  async function probeHost(host: Host) {
    setProbing(host.id);
    try {
      const result = await api.probeHost(host.id);
      setProbe({ host, ...result });
    } catch (reason) {
      notify(errorMessage(reason), 'error');
    } finally {
      setProbing(null);
    }
  }

  async function trustFingerprint() {
    if (!probe) return;
    setTrusting(true);
    try {
      await api.trustHost(probe.host.id, probe.fingerprint);
      upsert({ ...probe.host, fingerprint: probe.fingerprint });
      notify('主机指纹已信任', 'success');
      setProbe(null);
    } catch (reason) {
      notify(errorMessage(reason), 'error');
    } finally {
      setTrusting(false);
    }
  }

  async function testHost(host: Host) {
    setTesting(host.id);
    try {
      const result = await api.testHost(host.id);
      notify(`已成功连接 ${host.name}（${Math.round(result.latencyMs)} ms）`, 'success');
    } catch (reason) {
      notify(errorMessage(reason), 'error');
    } finally {
      setTesting(null);
    }
  }

  return (
    <section className="manager-view">
      <header className="manager-header">
        <div>
          <h1>SSH 主机</h1>
          <p>安全地保存连接信息，并验证每台主机的身份。</p>
        </div>
        <div className="manager-header__actions">
          <SSHConfigImport onImported={upsert} notify={notify} />
          <Button tone="primary" onClick={openCreate}>
            <Plus size={17} /> 添加主机
          </Button>
        </div>
      </header>

      {hosts.length === 0 ? (
        <EmptyState
          icon={<Server size={25} />}
          title="还没有远程主机"
          description="添加 SSH 主机后，可以像本机一样创建持久化终端。"
          action={
            <Button tone="primary" onClick={openCreate}>
              <Plus size={16} /> 添加第一台主机
            </Button>
          }
        />
      ) : (
        <div className="host-grid">
          {hosts.map((host) => (
            <article key={host.id} className="host-card">
              <div className="host-card__top">
                <span className="host-avatar">
                  <Server size={21} />
                </span>
                <div className="host-card__identity">
                  <h3>{host.name}</h3>
                  <p>
                    {host.username}@{host.address}:{host.port}
                  </p>
                </div>
                <ActionMenu
                  className="host-menu"
                  open={openMenuId === host.id}
                  onOpenChange={(open) => setOpenMenuId(open ? host.id : null)}
                  label={`${host.name} 操作`}
                  trigger={<MoreHorizontal size={18} />}
                >
                  <button
                    role="menuitem"
                    onClick={() => {
                      setOpenMenuId(null);
                      openEdit(host);
                    }}
                  >
                    <Edit3 size={16} /> 编辑主机
                  </button>
                  <button
                    role="menuitem"
                    className="danger-text"
                    onClick={() => {
                      setOpenMenuId(null);
                      setDeleteTarget(host);
                    }}
                  >
                    <Trash2 size={16} /> 删除主机
                  </button>
                </ActionMenu>
              </div>

              <div className="host-card__meta">
                <span>
                  <KeyRound size={15} />{' '}
                  {host.authType === 'privateKey' ? 'SSH 密钥' : host.authType === 'agent' ? 'SSH 代理' : '密码'}
                </span>
                <span className={host.fingerprint ? 'trusted' : 'untrusted'}>
                  {host.fingerprint ? <ShieldCheck size={15} /> : <ShieldAlert size={15} />}
                  {host.fingerprint ? '指纹已信任' : '待验证指纹'}
                </span>
              </div>

              {host.fingerprint && (
                <div className="fingerprint-preview" title={host.fingerprint}>
                  <Fingerprint size={14} />
                  <code>{host.fingerprint}</code>
                </div>
              )}

              <div className="host-card__actions">
                <Button
                  size="sm"
                  onClick={() => void testHost(host)}
                  disabled={testing === host.id || !host.fingerprint}
                >
                  {testing === host.id ? <LoaderCircle className="spin" size={15} /> : <Wifi size={15} />}
                  测试连接
                </Button>
                {!host.fingerprint && (
                  <Button size="sm" onClick={() => void probeHost(host)} disabled={probing === host.id}>
                    {probing === host.id ? <LoaderCircle className="spin" size={15} /> : <Fingerprint size={15} />}
                    验证身份
                  </Button>
                )}
                <Button
                  size="sm"
                  tone="primary"
                  disabled={!host.fingerprint}
                  title={host.fingerprint ? '新建 SSH 会话' : '请先验证主机指纹'}
                  onClick={() => onStartSession(host.id)}
                >
                  <TerminalSquare size={15} /> 新建会话
                </Button>
              </div>
            </article>
          ))}
        </div>
      )}

      {editorOpen && (
        <HostEditor
          host={editingHost}
          onClose={() => setEditorOpen(false)}
          onSaved={(host) => {
            upsert(host);
            setEditorOpen(false);
            if (!host.fingerprint) {
              notify('主机已保存，请确认主机指纹', 'info');
              void probeHost(host);
            } else {
              notify(editingHost ? '主机信息已更新' : '主机已添加', 'success');
            }
          }}
        />
      )}

      <ConfirmDialog
        open={Boolean(deleteTarget)}
        title={`删除 ${deleteTarget?.name ?? '主机'}？`}
        description="将删除连接配置和凭据；有关联会话时无法删除。"
        confirmLabel="删除主机"
        danger
        busy={deleting}
        onCancel={() => setDeleteTarget(null)}
        onConfirm={() => void removeHost()}
      />

      <Modal
        open={Boolean(probe)}
        title="验证主机身份"
        description={`首次连接 ${probe?.host.address ?? ''}，请与服务器管理员或控制台显示的指纹核对。`}
        closeDisabled={trusting}
        onClose={() => setProbe(null)}
        footer={
          <>
            <Button onClick={() => setProbe(null)} disabled={trusting}>
              取消
            </Button>
            <Button tone="primary" busy={trusting} onClick={() => void trustFingerprint()}>
              <ShieldCheck size={16} /> 信任此指纹
            </Button>
          </>
        }
      >
        {probe && (
          <div className="probe-result">
            <div className="probe-result__icon">
              <Fingerprint size={27} />
            </div>
            <div>
              <span>算法</span>
              <strong>{probe.algorithm}</strong>
            </div>
            <div className="probe-result__fingerprint">
              <span>SHA256 指纹</span>
              <code>{probe.fingerprint}</code>
            </div>
          </div>
        )}
      </Modal>
    </section>
  );
}

type HostEditorProps = {
  host: Host | null;
  onClose: () => void;
  onSaved: (host: Host) => void;
};

function HostEditor({ host, onClose, onSaved }: HostEditorProps) {
  const authTypeLabelId = useId();
  const [name, setName] = useState(host?.name ?? '');
  const [address, setAddress] = useState(host?.address ?? '');
  const [port, setPort] = useState(String(host?.port ?? 22));
  const [username, setUsername] = useState(host?.username ?? '');
  const [authType, setAuthType] = useState<AuthType>(host?.authType ?? 'privateKey');
  const [password, setPassword] = useState('');
  const [privateKey, setPrivateKey] = useState('');
  const [passphrase, setPassphrase] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);

  async function submit(event: FormEvent) {
    event.preventDefault();
    setError('');
    const numericPort = Number(port);
    if (!name.trim() || !address.trim() || !username.trim()) {
      setError('请填写名称、地址和用户名');
      return;
    }
    if (!Number.isInteger(numericPort) || numericPort < 1 || numericPort > 65535) {
      setError('端口应为 1–65535 之间的整数');
      return;
    }
    const credentialsChanged = !host || host.authType !== authType;
    if (credentialsChanged && authType === 'password' && !password) {
      setError('请输入 SSH 密码');
      return;
    }
    if (credentialsChanged && authType === 'privateKey' && !privateKey.trim()) {
      setError('请粘贴 SSH 私钥');
      return;
    }

    const input: HostInput = {
      name: name.trim(),
      address: address.trim(),
      port: numericPort,
      username: username.trim(),
      authType,
    };
    if (authType === 'password' && password) input.password = password;
    if (authType === 'privateKey') {
      const key = privateKey.trim();
      if (key) input.privateKey = key;
      if (key || passphrase) input.passphrase = passphrase;
    }

    setBusy(true);
    try {
      const saved = host ? await api.updateHost(host.id, input) : await api.createHost(input);
      onSaved(saved);
    } catch (reason) {
      setError(errorMessage(reason));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      open
      title={host ? '编辑 SSH 主机' : '添加 SSH 主机'}
      closeDisabled={busy}
      onClose={onClose}
      footer={
        <>
          <Button onClick={onClose} disabled={busy}>
            取消
          </Button>
          <Button type="submit" form="host-editor-form" tone="primary" busy={busy}>
            {host ? '保存更改' : '保存主机'}
          </Button>
        </>
      }
    >
      <form id="host-editor-form" className="form-grid" onSubmit={(event) => void submit(event)}>
        <Field label="显示名称">
          <Input value={name} onChange={(event) => setName(event.target.value)} placeholder="例如：家用服务器" />
        </Field>
        <div className="form-row form-row--address">
          <Field label="主机地址">
            <div className="input-with-icon">
              <Server size={16} />
              <Input
                value={address}
                onChange={(event) => setAddress(event.target.value)}
                placeholder="192.168.1.10 或 server.example.com"
                autoCapitalize="none"
                spellCheck={false}
              />
            </div>
          </Field>
          <Field label="端口">
            <Input
              type="number"
              min="1"
              max="65535"
              inputMode="numeric"
              value={port}
              onChange={(event) => setPort(event.target.value)}
            />
          </Field>
        </div>
        <Field label="用户名">
          <Input
            value={username}
            onChange={(event) => setUsername(event.target.value)}
            placeholder="root"
            autoCapitalize="none"
            spellCheck={false}
          />
        </Field>

        <div id={authTypeLabelId} className="segment-label">
          认证方式
        </div>
        <div className="choice-grid choice-grid--three" role="group" aria-labelledby={authTypeLabelId}>
          <button
            type="button"
            className={`choice-card ${authType === 'privateKey' ? 'is-active' : ''}`}
            aria-pressed={authType === 'privateKey'}
            onClick={() => setAuthType('privateKey')}
          >
            <span className="choice-card__icon">
              <KeyRound size={20} />
            </span>
            <span>
              <strong>SSH 私钥</strong>
            </span>
          </button>
          <button
            type="button"
            className={`choice-card ${authType === 'password' ? 'is-active' : ''}`}
            aria-pressed={authType === 'password'}
            onClick={() => setAuthType('password')}
          >
            <span className="choice-card__icon">
              <LockKeyhole size={20} />
            </span>
            <span>
              <strong>密码</strong>
            </span>
          </button>
          <button
            type="button"
            className={`choice-card ${authType === 'agent' ? 'is-active' : ''}`}
            aria-pressed={authType === 'agent'}
            onClick={() => setAuthType('agent')}
          >
            <span className="choice-card__icon">
              <Network size={20} />
            </span>
            <span>
              <strong>SSH 代理</strong>
            </span>
          </button>
        </div>

        {authType === 'agent' ? (
          <p className="field__hint">使用运行 wmux 的系统用户的 SSH agent（SSH_AUTH_SOCK）。</p>
        ) : authType === 'password' ? (
          <Field label="SSH 密码" hint={host?.hasSecret ? '留空以保留已保存的密码' : undefined}>
            <Input
              type="password"
              autoComplete="new-password"
              value={password}
              onChange={(event) => setPassword(event.target.value)}
              placeholder={host?.hasSecret ? '已保存 · 留空不变' : '输入 SSH 密码'}
            />
          </Field>
        ) : (
          <>
            <Field label="私钥" hint={host?.hasSecret ? '留空以保留已保存的私钥' : undefined}>
              <Textarea
                className="key-textarea"
                value={privateKey}
                onChange={(event) => setPrivateKey(event.target.value)}
                placeholder={host?.hasSecret ? '已保存 · 留空不变' : '-----BEGIN OPENSSH PRIVATE KEY-----'}
                spellCheck={false}
              />
            </Field>
            <Field label="私钥口令" optional>
              <Input
                type="password"
                autoComplete="new-password"
                value={passphrase}
                onChange={(event) => setPassphrase(event.target.value)}
                placeholder="仅加密私钥需要"
              />
            </Field>
          </>
        )}

        {error ? (
          <div className="form-error" role="alert">
            {error}
          </div>
        ) : null}
      </form>
    </Modal>
  );
}
