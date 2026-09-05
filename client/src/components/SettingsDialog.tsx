import { Info, LogOut, Monitor, RotateCcw, ShieldCheck, TerminalSquare } from 'lucide-react';
import { type FormEvent, useState } from 'react';
import { api, ApiError, errorMessage } from '../api';
import {
  AUTO_COLUMNS,
  COLUMN_PRESETS,
  COLUMN_RANGE,
  DEFAULT_PREFERENCES,
  FONT_SIZE_RANGE,
  isValidColumns,
  SCROLLBACK_OPTIONS,
} from '../preferences';
import { TERMINAL_FONTS, terminalFont, terminalFontStack, type TerminalFontId } from '../terminalFonts';
import type { Host, Session, TerminalPreferences, User } from '../types';

/** Width the custom option starts from when the terminal was fitting the window. */
const CUSTOM_COLUMNS_START = 100;
import { Button, ConfirmDialog, Field, Input, Modal, Select, UserAvatar } from './UI';

type SettingsDialogProps = {
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
  const [logoutError, setLogoutError] = useState('');
  const [currentPassword, setCurrentPassword] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [passwordConfirm, setPasswordConfirm] = useState('');
  const [passwordBusy, setPasswordBusy] = useState(false);
  const [passwordError, setPasswordError] = useState('');
  const [passwordSaved, setPasswordSaved] = useState(false);
  // "自定义" stays selected while the user types a width that happens to equal a preset.
  const [customColumns, setCustomColumns] = useState(
    preferences.columns !== AUTO_COLUMNS && !COLUMN_PRESETS.includes(preferences.columns),
  );
  const [columnsDraft, setColumnsDraft] = useState(String(preferences.columns || CUSTOM_COLUMNS_START));

  async function logout() {
    setLoggingOut(true);
    setLogoutError('');
    try {
      await onLogout();
      setLogoutOpen(false);
    } catch (reason) {
      setLogoutError(errorMessage(reason));
    } finally {
      setLoggingOut(false);
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
      setPasswordError(reason instanceof ApiError && reason.status === 401 ? '当前密码不正确' : errorMessage(reason));
    } finally {
      setPasswordBusy(false);
    }
  }

  return (
    <>
      <Modal open title="设置" size="lg" variant="settings" closeDisabled={passwordBusy} onClose={onClose}>
        <div className="settings-layout">
          <nav className="settings-nav" aria-label="设置类别">
            <button
              type="button"
              className={section === 'terminal' ? 'is-active' : ''}
              aria-current={section === 'terminal' ? 'page' : undefined}
              onClick={() => setSection('terminal')}
            >
              <TerminalSquare size={16} /> 终端
            </button>
            <button
              type="button"
              className={section === 'security' ? 'is-active' : ''}
              aria-current={section === 'security' ? 'page' : undefined}
              onClick={() => setSection('security')}
            >
              <ShieldCheck size={16} /> 账户与安全
            </button>
            <button
              type="button"
              className={section === 'about' ? 'is-active' : ''}
              aria-current={section === 'about' ? 'page' : undefined}
              onClick={() => setSection('about')}
            >
              <Info size={16} /> 关于
            </button>
          </nav>

          <div className="settings-content">
            {section === 'terminal' && (
              <section className="settings-section" aria-labelledby="terminal-settings-title">
                <div className="settings-section__title">
                  <div>
                    <h3 id="terminal-settings-title">终端显示</h3>
                    <p className="field__hint">仅保存在此浏览器</p>
                  </div>
                </div>

                <div className="setting-row setting-row--slider">
                  <div>
                    <strong>字体大小</strong>
                    <span style={{ fontFamily: terminalFontStack(preferences.fontFamily) }}>
                      {terminalFont(preferences.fontFamily).label} · {preferences.fontSize}px
                    </span>
                  </div>
                  <input
                    type="range"
                    min={FONT_SIZE_RANGE.min}
                    max={FONT_SIZE_RANGE.max}
                    step="1"
                    value={preferences.fontSize}
                    onChange={(event) => onPreferencesChange({ ...preferences, fontSize: Number(event.target.value) })}
                    aria-label="终端字体大小"
                  />
                </div>

                <div className="setting-row">
                  <div>
                    <strong>终端字体</strong>
                    <span>选择后按需下载，只影响此浏览器</span>
                  </div>
                  <Select
                    aria-label="终端字体"
                    value={preferences.fontFamily}
                    onChange={(event) =>
                      onPreferencesChange({ ...preferences, fontFamily: event.target.value as TerminalFontId })
                    }
                  >
                    {TERMINAL_FONTS.map((font) => (
                      <option key={font.id} value={font.id}>
                        {font.label}
                      </option>
                    ))}
                  </Select>
                </div>

                <div className="setting-row">
                  <div>
                    <strong>终端宽度</strong>
                    <span>固定列数时会缩小字号以适应窄屏幕，放不下再横向滚动</span>
                  </div>
                  <span className="setting-row__controls">
                    <Select
                      aria-label="终端宽度"
                      value={customColumns ? 'custom' : String(preferences.columns)}
                      onChange={(event) => {
                        if (event.target.value === 'custom') {
                          const start = preferences.columns || CUSTOM_COLUMNS_START;
                          setCustomColumns(true);
                          setColumnsDraft(String(start));
                          onPreferencesChange({ ...preferences, columns: start });
                          return;
                        }
                        setCustomColumns(false);
                        onPreferencesChange({ ...preferences, columns: Number(event.target.value) });
                      }}
                    >
                      <option value={String(AUTO_COLUMNS)}>自动（随窗口）</option>
                      {COLUMN_PRESETS.map((columns) => (
                        <option key={columns} value={columns}>
                          {columns} 列
                        </option>
                      ))}
                      <option value="custom">自定义…</option>
                    </Select>
                    {customColumns && (
                      <Input
                        type="number"
                        inputMode="numeric"
                        aria-label="自定义列数"
                        min={COLUMN_RANGE.min}
                        max={COLUMN_RANGE.max}
                        value={columnsDraft}
                        onChange={(event) => {
                          setColumnsDraft(event.target.value);
                          const columns = Number(event.target.value);
                          if (isValidColumns(columns) && columns !== AUTO_COLUMNS) {
                            onPreferencesChange({ ...preferences, columns });
                          }
                        }}
                      />
                    )}
                  </span>
                </div>

                <div className="setting-row">
                  <div>
                    <strong>界面主题</strong>
                  </div>
                  <Select
                    aria-label="界面主题"
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
                  </div>
                  <Select
                    aria-label="光标样式"
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
                  </div>
                  <label className="switch">
                    <input
                      aria-label="光标闪烁"
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
                  </div>
                  <Select
                    aria-label="回滚缓冲区"
                    value={String(preferences.scrollback)}
                    onChange={(event) =>
                      onPreferencesChange({ ...preferences, scrollback: Number(event.target.value) })
                    }
                  >
                    {SCROLLBACK_OPTIONS.map((option) => (
                      <option key={option.value} value={option.value}>
                        {option.label}
                      </option>
                    ))}
                  </Select>
                </div>

                <div className="settings-actions">
                  <Button
                    size="sm"
                    onClick={() => {
                      setCustomColumns(false);
                      onPreferencesChange(DEFAULT_PREFERENCES);
                    }}
                  >
                    <RotateCcw size={16} /> 恢复默认
                  </Button>
                </div>
              </section>
            )}

            {section === 'security' && (
              <section className="settings-section" aria-labelledby="security-settings-title">
                <div className="settings-section__title">
                  <h3 id="security-settings-title">账户与安全</h3>
                </div>
                <div className="settings-account">
                  <div className="settings-account__identity">
                    <UserAvatar username={user.username} />
                    <strong>{user.username}</strong>
                  </div>
                  <Button
                    size="sm"
                    disabled={passwordBusy}
                    onClick={() => {
                      setLogoutError('');
                      setLogoutOpen(true);
                    }}
                  >
                    <LogOut size={15} /> 退出登录
                  </Button>
                </div>
                <form
                  className="password-change"
                  aria-labelledby="password-change-title"
                  onSubmit={(event) => void changePassword(event)}
                >
                  <div className="password-change__title">
                    <strong id="password-change-title">修改密码</strong>
                  </div>
                  <div className="password-change__fields">
                    <Field label="当前密码">
                      <Input
                        type="password"
                        autoComplete="current-password"
                        value={currentPassword}
                        onChange={(event) => setCurrentPassword(event.target.value)}
                      />
                    </Field>
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
                  {passwordError ? (
                    <div className="form-error" role="alert">
                      {passwordError}
                    </div>
                  ) : null}
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
                  <span>开源字体与组件</span>
                  <a href="/third-party-notices.txt" target="_blank" rel="noreferrer">
                    查看第三方许可
                  </a>
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
        error={logoutError}
        onCancel={() => {
          setLogoutError('');
          setLogoutOpen(false);
        }}
        onConfirm={() => void logout()}
      />
    </>
  );
}
