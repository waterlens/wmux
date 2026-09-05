import { FileInput, KeyRound, LoaderCircle, RefreshCw, ShieldAlert } from 'lucide-react';
import { useCallback, useEffect, useState } from 'react';
import { api, errorMessage } from '../api';
import type { Host, Notify, SSHConfigCandidate, SSHConfigDiscovery } from '../types';
import { Button, Modal } from './UI';

type SSHConfigImportProps = {
  onImported: (host: Host) => void;
  notify: Notify;
};

export function SSHConfigImport({ onImported, notify }: SSHConfigImportProps) {
  const [open, setOpen] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [discovery, setDiscovery] = useState<SSHConfigDiscovery | null>(null);
  const [importingAlias, setImportingAlias] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      setDiscovery(await api.sshConfigHosts());
    } catch (reason) {
      setDiscovery(null);
      setError(errorMessage(reason));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    const timer = window.setTimeout(() => void refresh(), 0);
    return () => window.clearTimeout(timer);
  }, [refresh]);

  async function importCandidate(candidate: SSHConfigCandidate) {
    setImportingAlias(candidate.alias);
    try {
      const host = await api.importSSHConfigHost(candidate.alias);
      onImported(host);
      setDiscovery((current) =>
        current
          ? {
              ...current,
              candidates: current.candidates.map((item) =>
                item.alias === candidate.alias ? { ...item, existingHostId: host.id } : item,
              ),
            }
          : current,
      );
      notify(`已导入「${candidate.alias}」，请验证主机指纹`, 'success');
    } catch (reason) {
      notify(errorMessage(reason), 'error');
    } finally {
      setImportingAlias(null);
    }
  }

  return (
    <>
      <Button onClick={() => setOpen(true)} aria-label="从 SSH config 导入主机">
        <FileInput size={17} /> 导入 SSH config
      </Button>
      <Modal
        open={open}
        title="从 SSH config 导入"
        description="只导入别名、地址、端口和用户名；不会读取或复制私钥，也不会自动信任主机指纹。"
        size="lg"
        onClose={() => setOpen(false)}
        footer={<Button onClick={() => setOpen(false)}>完成</Button>}
      >
        <div className="ssh-config-import">
          <div className="ssh-config-import__source">
            <span>{discovery?.source || '~/.ssh/config'}</span>
            <Button size="sm" onClick={() => void refresh()} busy={loading} aria-label="重新扫描 SSH config">
              <RefreshCw size={15} /> 重新扫描
            </Button>
          </div>

          {loading && !discovery ? (
            <div className="ssh-config-import__empty" role="status">
              <LoaderCircle className="spin" size={18} /> 正在读取 SSH config…
            </div>
          ) : error ? (
            <div className="form-error" role="alert">
              {error}
            </div>
          ) : !discovery?.available ? (
            <div className="ssh-config-import__empty">
              未找到 SSH config。Docker 部署需要将配置文件和使用到的 Include 片段只读挂载到容器中。
            </div>
          ) : discovery.candidates.length === 0 ? (
            <div className="ssh-config-import__empty">没有发现可直接导入的 Host 别名。</div>
          ) : (
            <ul className="ssh-config-list">
              {discovery.candidates.map((candidate) => {
                const unsupported = candidate.unsupported.length > 0;
                const imported = Boolean(candidate.existingHostId);
                const unavailableReason = imported
                  ? '已导入'
                  : unsupported
                    ? `暂不支持 ${candidate.unsupported.join('、')}`
                    : '';
                return (
                  <li key={candidate.alias} className="ssh-config-candidate">
                    <div className="ssh-config-candidate__identity">
                      <strong>{candidate.alias}</strong>
                      <code>
                        {candidate.username}@{candidate.address}:{candidate.port}
                      </code>
                    </div>
                    {(candidate.hasIdentityFile || unsupported) && (
                      <div className="ssh-config-candidate__meta">
                        {candidate.hasIdentityFile && (
                          <span>
                            <KeyRound size={14} /> IdentityFile 不会导入
                          </span>
                        )}
                        {unsupported && (
                          <span className="is-unsupported">
                            <ShieldAlert size={14} /> {unavailableReason}
                          </span>
                        )}
                      </div>
                    )}
                    <Button
                      size="sm"
                      disabled={imported || unsupported}
                      busy={importingAlias === candidate.alias}
                      title={unavailableReason || `导入 ${candidate.alias}`}
                      aria-label={imported ? `${candidate.alias} 已导入` : `导入 ${candidate.alias}`}
                      onClick={() => void importCandidate(candidate)}
                    >
                      {imported ? '已导入' : '导入'}
                    </Button>
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      </Modal>
    </>
  );
}
