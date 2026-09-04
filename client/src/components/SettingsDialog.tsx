import { Info, LogOut, Monitor, RotateCcw, ShieldCheck, SlidersHorizontal, TerminalSquare } from 'lucide-react';
import { type FormEvent, useState } from 'react';
import { api, errorMessage } from '../api';
import { DEFAULT_PREFERENCES } from '../preferences';
import type { Host, Session, TerminalPreferences, User } from '../types';
import { Button, ConfirmDialog, Field, Input, Modal, Select } from './UI';

type SettingsDialogProps = {
  open: boolean;
  user: User;
  version: string;
  commit?: string | undefined;
  hosts: Host[];
  sessions: Session[];
  preferences: TerminalPreferences;
  onPreferencesChange: (preferences: TerminalPreferences) => void;
  onClose: () => void;
  onLogout: () => Promise<void>;
};

export function SettingsDialog({
  open,
  user,
  version,
  commit,
  hosts,
  sessions,
  preferences,
  onPreferencesChange,
  onClose,
  onLogout,
}: SettingsDialogProps) {
  const [section, setSection] = useState<'terminal' | 'security' | 'about'>('terminal');
  const [logoutOpen, setLogoutOpen] = useState(false);
  const [loggingOut, setLoggingOut] = useState(false);
  const [currentPassword, setCurrentPassword] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [passwordConfirm, setPasswordConfirm] = useState('');
  const [passwordBusy, setPasswordBusy] = useState(false);
  const [passwordError, setPasswordError] = useState('');
  const [passwordSaved, setPasswordSaved] = useState(false);

  async function logout() {
    setLoggingOut(true);
    try {
      await onLogout();
    } finally {
      setLoggingOut(false);
      setLogoutOpen(false);
    }
  }

  async function changePassword(event: FormEvent) {
    event.preventDefault();
    setPasswordError('');
    setPasswordSaved(false);
    if (!currentPassword) {
      setPasswordError('请输入当前密码');
      return;
    }
    if (newPassword.length < 10) {
      setPasswordError('新密码至少需要 10 个字符');
      return;
    }
    if (newPassword !== passwordConfirm) {
      setPasswordError('两次输入的新密码不一致');
      return;
    }
    if (currentPassword === newPassword) {
      setPasswordError('新密码需要与当前密码不同');
      return;
    }

    setPasswordBusy(true);
    try {
      await api.changePassword(currentPassword, newPassword);
      setCurrentPassword('');
      setNewPassword('');
      setPasswordConfirm('');
      setPasswordSaved(true);
    } catch (reason) {
      setPasswordError(errorMessage(reason));
    } finally {
      setPasswordBusy(false);
    }
  }

  return (
    <>
      <Modal open={open} title="设置" description="调整当前浏览器上的 wmux 体验。" size="lg" onClose={onClose}>
        <div className="settings-layout">
          <nav className="settings-nav">
            <button className={section === 'terminal' ? 'is-active' : ''} onClick={() => setSection('terminal')}>
              <TerminalSquare size={16} /> 终端
            </button>
            <button className={section === 'security' ? 'is-active' : ''} onClick={() => setSection('security')}>
              <ShieldCheck size={16} /> 账户与安全
            </button>
            <button className={section === 'about' ? 'is-active' : ''} onClick={() => setSection('about')}>
              <Info size={16} /> 关于
            </button>
          </nav>

          <div className="settings-content">
            {section === 'terminal' && (
              <section className="settings-section">
                <div className="settings-section__title">
                  <SlidersHorizontal size={18} />
                  <div>
                    <h3>终端显示</h3>
                    <p>设置只保存在这个浏览器中。</p>
                  </div>
                </div>

                <div className="setting-row setting-row--slider">
                  <div>
                    <strong>字体大小</strong>
                    <span>JetBrains Mono · {preferences.fontSize}px</span>
                  </div>
                  <input
                    type="range"
                    min="11"
                    max="22"
                    step="1"
                    value={preferences.fontSize}
                    onChange={(event) => onPreferencesChange({ ...preferences, fontSize: Number(event.target.value) })}
                    aria-label="终端字体大小"
                  />
                </div>

                <div className="setting-row">
                  <div>
                    <strong>界面主题</strong>
                    <span>默认使用浅色，也可跟随系统</span>
                  </div>
                  <Select
                    value={preferences.theme}
                    onChange={(event) =>
                      onPreferencesChange({ ...preferences, theme: event.target.value as TerminalPreferences['theme'] })
                    }
                  >
                    <option value="light">浅色</option>
                    <option value="dark">深色</option>
                    <option value="system">跟随系统</option>
                  </Select>
                </div>

                <div className="setting-row">
                  <div>
                    <strong>光标样式</strong>
                    <span>选择更容易辨认的输入光标</span>
                  </div>
                  <Select
                    value={preferences.cursorStyle}
                    onChange={(event) =>
                      onPreferencesChange({
                        ...preferences,
                        cursorStyle: event.target.value as TerminalPreferences['cursorStyle'],
                      })
                    }
                  >
                    <option value="block">方块</option>
                    <option value="bar">竖线</option>
                    <option value="underline">下划线</option>
                  </Select>
                </div>

                <div className="setting-row">
                  <div>
                    <strong>光标闪烁</strong>
                    <span>让当前输入位置保持醒目</span>
                  </div>
                  <label className="switch">
                    <input
                      type="checkbox"
                      checked={preferences.cursorBlink}
                      onChange={(event) => onPreferencesChange({ ...preferences, cursorBlink: event.target.checked })}
                    />
                    <span />
                  </label>
                </div>

                <div className="setting-row">
                  <div>
                    <strong>回滚缓冲区</strong>
                    <span>浏览器中保留的最大行数</span>
                  </div>
                  <Select
                    value={String(preferences.scrollback)}
                    onChange={(event) =>
                      onPreferencesChange({ ...preferences, scrollback: Number(event.target.value) })
                    }
                  >
                    <option value="2000">2,000 行</option>
                    <option value="10000">10,000 行</option>
                    <option value="25000">25,000 行</option>
                    <option value="50000">50,000 行</option>
                  </Select>
                </div>

                <div className="settings-actions">
                  <Button size="sm" onClick={() => onPreferencesChange(DEFAULT_PREFERENCES)}>
                    <RotateCcw size={16} /> 恢复默认
                  </Button>
                </div>
              </section>
            )}

            {section === 'security' && (
              <section className="settings-section">
                <div className="settings-section__title">
                  <ShieldCheck size={18} />
                  <div>
                    <h3>账户与安全</h3>
                    <p>当前 wmux 管理员。</p>
                  </div>
                </div>
                <div className="account-card">
                  <span className="user-avatar user-avatar--large">{user.username.slice(0, 1).toUpperCase()}</span>
                  <div>
                    <strong>{user.username}</strong>
                    <span>本地管理员账户</span>
                  </div>
                </div>
                <div className="security-note">
                  <ShieldCheck size={18} />
                  <p>
                    <strong>Cookie 会话认证</strong>
                    <span>终端 WebSocket 与所有 API 请求都复用当前安全会话。</span>
                  </p>
                </div>
                <form className="password-change" onSubmit={(event) => void changePassword(event)}>
                  <div className="password-change__title">
                    <strong>修改管理员密码</strong>
                    <span>更新后请在其他设备上使用新密码登录。</span>
                  </div>
                  <Field label="当前密码">
                    <Input
                      type="password"
                      autoComplete="current-password"
                      value={currentPassword}
                      onChange={(event) => setCurrentPassword(event.target.value)}
                    />
                  </Field>
                  <div className="form-row">
                    <Field label="新密码" hint="至少 10 个字符">
                      <Input
                        type="password"
                        autoComplete="new-password"
                        value={newPassword}
                        onChange={(event) => setNewPassword(event.target.value)}
                      />
                    </Field>
                    <Field label="确认新密码">
                      <Input
                        type="password"
                        autoComplete="new-password"
                        value={passwordConfirm}
                        onChange={(event) => setPasswordConfirm(event.target.value)}
                      />
                    </Field>
                  </div>
                  {passwordError && (
                    <div className="form-error" role="alert">
                      {passwordError}
                    </div>
                  )}
                  {passwordSaved && (
                    <div className="success-callout" role="status">
                      <ShieldCheck size={16} />
                      <span>密码已更新</span>
                    </div>
                  )}
                  <div className="password-change__action">
                    <Button type="submit" size="sm" tone="primary" busy={passwordBusy}>
                      更新密码
                    </Button>
                  </div>
                </form>
                <div className="danger-zone">
                  <div>
                    <strong>退出当前设备</strong>
                    <span>持久化终端不会因此结束。</span>
                  </div>
                  <Button tone="danger" size="sm" onClick={() => setLogoutOpen(true)}>
                    <LogOut size={15} /> 退出登录
                  </Button>
                </div>
              </section>
            )}

            {section === 'about' && (
              <section className="settings-section">
                <div className="about-mark">
                  <span className="brand__mark">
                    <TerminalSquare size={24} />
                  </span>
                  <div>
                    <strong>wmux</strong>
                    <span>
                      {version && version !== 'dev' ? `版本 ${version}` : '开发版本'}
                      {commit && commit !== 'unknown' ? ` · ${commit.slice(0, 8)}` : ''}
                    </span>
                  </div>
                </div>
                <p className="about-copy">一个属于自己的持久化 Web 终端，把本机 shell 和 SSH 工作流放在同一个地方。</p>
                <div className="about-stats">
                  <div>
                    <Monitor size={17} />
                    <strong>{sessions.length}</strong>
                    <span>会话</span>
                  </div>
                  <div>
                    <TerminalSquare size={17} />
                    <strong>{sessions.filter((session) => session.status === 'running').length}</strong>
                    <span>运行中</span>
                  </div>
                  <div>
                    <ShieldCheck size={17} />
                    <strong>{hosts.length}</strong>
                    <span>SSH 主机</span>
                  </div>
                </div>
                <div className="about-meta">
                  <span>部署方式</span>
                  <strong>自托管</strong>
                </div>
              </section>
            )}
          </div>
        </div>
      </Modal>

      <ConfirmDialog
        open={logoutOpen}
        title="退出 wmux？"
        description="当前浏览器会断开终端，但所有持久化会话仍会在后台继续运行。"
        confirmLabel="退出登录"
        busy={loggingOut}
        onCancel={() => setLogoutOpen(false)}
        onConfirm={() => void logout()}
      />
    </>
  );
}
