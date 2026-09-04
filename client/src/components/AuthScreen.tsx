import { ArrowRight, CheckCircle2, Eye, EyeOff, TerminalSquare } from 'lucide-react';
import { type FormEvent, useState } from 'react';
import { api, errorMessage } from '../api';
import { Button, Field, Input } from './UI';

type AuthScreenProps = {
  mode: 'setup' | 'login';
  version: string;
  onAuthenticated: () => void;
};

export function AuthScreen({ mode, version, onAuthenticated }: AuthScreenProps) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [showPassword, setShowPassword] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const isSetup = mode === 'setup';

  async function submit(event: FormEvent) {
    event.preventDefault();
    setError('');
    if (!username.trim()) {
      setError('请输入用户名');
      return;
    }
    if (!password) {
      setError('请输入密码');
      return;
    }
    if (isSetup && password.length < 10) {
      setError('密码至少需要 10 个字符');
      return;
    }
    if (isSetup && password !== confirm) {
      setError('两次输入的密码不一致');
      return;
    }

    setBusy(true);
    try {
      if (isSetup) await api.setup(username.trim(), password);
      else await api.login(username.trim(), password);
      onAuthenticated();
    } catch (reason) {
      setError(errorMessage(reason));
    } finally {
      setBusy(false);
    }
  }

  return (
    <main className="auth-shell auth-shell--plain">
      <section className="auth-panel">
        <div className="auth-card">
          <div className="auth-card__brand brand">
            <span className="brand__mark">
              <TerminalSquare size={19} />
            </span>
            <span>wmux</span>
          </div>
          <h2>{isSetup ? '初始化 wmux' : '登录 wmux'}</h2>
          <p>{isSetup ? '创建此实例的管理员账户。' : '登录后继续使用本机和 SSH 会话。'}</p>

          {isSetup && (
            <div className="setup-note">
              <CheckCircle2 size={17} />
              <span>账户和凭据只保存在此实例。</span>
            </div>
          )}

          <form className="auth-form" onSubmit={(event) => void submit(event)}>
            <Field label="用户名">
              <Input
                autoComplete="username"
                autoCapitalize="none"
                spellCheck={false}
                value={username}
                onChange={(event) => setUsername(event.target.value)}
                placeholder="管理员用户名"
              />
            </Field>
            <Field label="密码" hint={isSetup ? '至少 10 个字符' : undefined}>
              <div className="password-input">
                <Input
                  type={showPassword ? 'text' : 'password'}
                  autoComplete={isSetup ? 'new-password' : 'current-password'}
                  value={password}
                  onChange={(event) => setPassword(event.target.value)}
                  placeholder="输入密码"
                />
                <button
                  type="button"
                  onClick={() => setShowPassword((value) => !value)}
                  aria-label={showPassword ? '隐藏密码' : '显示密码'}
                >
                  {showPassword ? <EyeOff size={18} /> : <Eye size={18} />}
                </button>
              </div>
            </Field>
            {isSetup && (
              <Field label="确认密码">
                <Input
                  type={showPassword ? 'text' : 'password'}
                  autoComplete="new-password"
                  value={confirm}
                  onChange={(event) => setConfirm(event.target.value)}
                  placeholder="再次输入密码"
                />
              </Field>
            )}
            {error && (
              <div className="form-error" role="alert">
                {error}
              </div>
            )}
            <Button type="submit" tone="primary" busy={busy} className="auth-submit">
              {isSetup ? '完成设置' : '进入工作台'}
              {!busy && <ArrowRight size={17} />}
            </Button>
          </form>
        </div>
        <div className="auth-footer">
          <span>{version && version !== 'dev' ? `wmux v${version}` : 'wmux 开发版本'}</span>
          <span className="auth-footer__dot" />
          <span>自托管</span>
        </div>
      </section>
    </main>
  );
}
