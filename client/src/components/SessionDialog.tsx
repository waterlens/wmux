import { Command, Server, Sparkles, TerminalSquare } from 'lucide-react';
import { type FormEvent, useMemo, useState } from 'react';
import { api, errorMessage } from '../api';
import type { Host, PersistenceMode, Session, SessionInput, SessionKind } from '../types';
import { Button, Field, Input, Modal, Select } from './UI';

type SessionDialogProps = {
  open: boolean;
  hosts: Host[];
  sessions: Session[];
  initialHostId?: string | undefined;
  onClose: () => void;
  onCreated: (session: Session) => void;
};

export function SessionDialog({ open, hosts, sessions, initialHostId, onClose, onCreated }: SessionDialogProps) {
  const [kind, setKind] = useState<SessionKind>(() => (initialHostId ? 'ssh' : 'local'));
  const [hostId, setHostId] = useState(initialHostId ?? '');
  const [name, setName] = useState('');
  const [cwd, setCwd] = useState('');
  const [command, setCommand] = useState('');
  const [persistence, setPersistence] = useState<PersistenceMode>('auto');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);

  const selectedHost = useMemo(() => hosts.find((host) => host.id === hostId), [hostId, hosts]);
  const trustedHosts = useMemo(() => hosts.filter((host) => Boolean(host.fingerprint)), [hosts]);

  function availableDefaultName(): string {
    const base = kind === 'local' ? '本机终端' : (selectedHost?.name ?? 'SSH 会话');
    const names = new Set(sessions.map((session) => session.name));
    if (!names.has(base)) return base;
    let number = 2;
    while (names.has(`${base} ${number}`)) number += 1;
    return `${base} ${number}`;
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    setError('');
    if (kind === 'ssh' && !trustedHosts.some((host) => host.id === hostId)) {
      setError(trustedHosts.length ? '请选择一台 SSH 主机' : '请先添加并验证一台 SSH 主机');
      return;
    }

    const base: SessionInput = {
      name: name.trim(),
      kind,
      cwd: cwd.trim(),
      command: command.trim(),
      persistence,
    };
    const input: SessionInput = kind === 'ssh' ? { ...base, hostId } : base;
    setBusy(true);
    try {
      const session = await api.createSession(input);
      onCreated(session);
      onClose();
    } catch (reason) {
      setError(errorMessage(reason));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      open={open}
      title="新建会话"
      description="进程将在浏览器关闭后继续运行。"
      onClose={onClose}
      footer={
        <>
          <Button onClick={onClose} disabled={busy}>
            取消
          </Button>
          <Button type="submit" form="new-session-form" tone="primary" busy={busy}>
            启动会话
          </Button>
        </>
      }
    >
      <form id="new-session-form" className="form-grid" onSubmit={(event) => void submit(event)}>
        <div className="segment-label">运行位置</div>
        <div className="choice-grid choice-grid--two">
          <button
            type="button"
            className={`choice-card ${kind === 'local' ? 'is-active' : ''}`}
            onClick={() => setKind('local')}
          >
            <span className="choice-card__icon">
              <TerminalSquare size={20} />
            </span>
            <span>
              <strong>本机</strong>
              <small>运行 wmux 的设备</small>
            </span>
          </button>
          <button
            type="button"
            className={`choice-card ${kind === 'ssh' ? 'is-active' : ''}`}
            onClick={() => setKind('ssh')}
          >
            <span className="choice-card__icon">
              <Server size={20} />
            </span>
            <span>
              <strong>SSH 主机</strong>
              <small>已保存的远程机器</small>
            </span>
          </button>
        </div>

        {kind === 'ssh' && (
          <Field label="主机">
            <Select value={hostId} onChange={(event) => setHostId(event.target.value)}>
              <option value="">选择 SSH 主机…</option>
              {trustedHosts.map((host) => (
                <option key={host.id} value={host.id}>
                  {host.name} — {host.username}@{host.address}
                </option>
              ))}
            </Select>
          </Field>
        )}

        <div className="form-row">
          <Field label="会话名称" optional hint="留空则自动命名">
            <Input
              value={name}
              onChange={(event) => setName(event.target.value)}
              placeholder={availableDefaultName()}
            />
          </Field>
          <Field label="持久化方式" hint="自动选择可用后端">
            <Select value={persistence} onChange={(event) => setPersistence(event.target.value as PersistenceMode)}>
              <option value="auto">自动（推荐）</option>
              <option value="tmux">tmux</option>
              <option value="screen">screen</option>
              <option value="none">不持久化</option>
            </Select>
          </Field>
        </div>

        <Field label="工作目录" optional hint={kind === 'local' ? '默认为当前用户的主目录' : '远程主机上的目录'}>
          <Input
            className="mono-input"
            value={cwd}
            onChange={(event) => setCwd(event.target.value)}
            placeholder="/path/to/project"
            spellCheck={false}
          />
        </Field>
        <Field label="启动命令" optional hint="留空则启动默认 shell">
          <div className="input-with-icon">
            <Command size={16} />
            <Input
              className="mono-input"
              value={command}
              onChange={(event) => setCommand(event.target.value)}
              placeholder="例如 htop 或 pnpm dev"
              spellCheck={false}
            />
          </div>
        </Field>

        <div className="info-callout">
          <Sparkles size={17} />
          <span>
            {persistence === 'none' ? '这个会话会在服务连接终止时结束。' : '断线后会自动重连，并补发缺失的终端输出。'}
          </span>
        </div>
        {error && (
          <div className="form-error" role="alert">
            {error}
          </div>
        )}
      </form>
    </Modal>
  );
}

type RenameProps = {
  session: Session | null;
  onClose: () => void;
  onSaved: (session: Session) => void;
};

export function RenameSessionDialog({ session, onClose, onSaved }: RenameProps) {
  const [name, setName] = useState(session?.name ?? '');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!session || !name.trim()) return;
    setBusy(true);
    setError('');
    try {
      onSaved(await api.updateSession(session.id, { name: name.trim() }));
      onClose();
    } catch (reason) {
      setError(errorMessage(reason));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      open={Boolean(session)}
      title="重命名会话"
      onClose={onClose}
      size="sm"
      footer={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button type="submit" form="rename-session-form" tone="primary" busy={busy}>
            保存
          </Button>
        </>
      }
    >
      <form id="rename-session-form" onSubmit={(event) => void submit(event)}>
        <Field label="会话名称">
          <Input value={name} onChange={(event) => setName(event.target.value)} />
        </Field>
        {error && (
          <div className="form-error" role="alert">
            {error}
          </div>
        )}
      </form>
    </Modal>
  );
}
