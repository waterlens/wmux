import type { PersistenceMode, SessionStatus } from './types';

const labels: Record<SessionStatus, string> = {
  connecting: '启动中',
  running: '运行中',
  reconnecting: '重连中',
  detached: '已分离',
  exited: '已结束',
  error: '异常',
};

export function sessionStatusLabel(status: SessionStatus): string {
  return labels[status];
}

export function sessionStatusTone(status: SessionStatus): 'online' | 'pending' | 'error' | 'idle' {
  if (status === 'running') return 'online';
  if (status === 'connecting' || status === 'reconnecting') return 'pending';
  if (status === 'error') return 'error';
  return 'idle';
}

export function persistenceLabel(value: PersistenceMode | string | undefined): string {
  switch (value) {
    case 'tmux':
      return 'tmux 持久化';
    case 'screen':
      return 'screen 持久化';
    case 'none':
      return '直接连接';
    case 'auto':
      return '自动持久化';
    default:
      return '';
  }
}
