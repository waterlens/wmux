import {
  ChevronDown,
  ChevronRight,
  CircleDot,
  Edit3,
  MoreHorizontal,
  PanelLeftClose,
  Plus,
  RefreshCw,
  Search,
  Server,
  Settings,
  TerminalSquare,
  Trash2,
  X,
} from 'lucide-react';
import { useMemo, useState } from 'react';
import { persistenceLabel, sessionStatusLabel, sessionStatusTone } from '../sessionStatus';
import type { Host, Session, User } from '../types';
import { ActionMenu, Button } from './UI';

type SidebarProps = {
  sessions: Session[];
  hosts: Host[];
  user: User;
  activeSessionId: string | null;
  currentView: 'home' | 'terminal' | 'hosts';
  mobileOpen: boolean;
  onMobileClose: () => void;
  onSelectSession: (session: Session) => void;
  onNewSession: () => void;
  onHome: () => void;
  onHosts: () => void;
  onRename: (session: Session) => void;
  onRestart: (session: Session) => void;
  onDelete: (session: Session) => void;
  onSettings: () => void;
  onCollapse: () => void;
};

export function Sidebar({
  sessions,
  hosts,
  user,
  activeSessionId,
  currentView,
  mobileOpen,
  onMobileClose,
  onSelectSession,
  onNewSession,
  onHome,
  onHosts,
  onRename,
  onRestart,
  onDelete,
  onSettings,
  onCollapse,
}: SidebarProps) {
  const [query, setQuery] = useState('');
  const [collapsedGroups, setCollapsedGroups] = useState<Set<string>>(new Set());
  const [openMenuId, setOpenMenuId] = useState<string | null>(null);

  const filtered = useMemo(() => {
    const needle = query.trim().toLocaleLowerCase();
    if (!needle) return sessions;
    return sessions.filter((session) =>
      `${session.name} ${session.hostName ?? ''} ${session.cwd}`.toLocaleLowerCase().includes(needle),
    );
  }, [query, sessions]);

  const groups = useMemo(() => {
    const output: Array<{ id: string; label: string; icon: 'local' | 'ssh'; sessions: Session[] }> = [];
    const local = filtered.filter((session) => session.kind === 'local');
    if (local.length || !query) output.push({ id: 'local', label: '本机', icon: 'local', sessions: local });
    for (const host of hosts) {
      const hostSessions = filtered.filter((session) => session.hostId === host.id);
      if (hostSessions.length || !query)
        output.push({ id: host.id, label: host.name, icon: 'ssh', sessions: hostSessions });
    }
    const missingHost = filtered.filter(
      (session) => session.kind === 'ssh' && !hosts.some((host) => host.id === session.hostId),
    );
    if (missingHost.length) output.push({ id: 'unknown', label: '其他 SSH', icon: 'ssh', sessions: missingHost });
    return output;
  }, [filtered, hosts, query]);

  function toggleGroup(id: string) {
    setCollapsedGroups((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  return (
    <aside className={`sidebar ${mobileOpen ? 'is-mobile-open' : ''}`}>
      <header className="sidebar__header">
        <div className="brand">
          <span className="brand__mark">
            <TerminalSquare size={19} />
          </span>
          <span>wmux</span>
        </div>
        <div className="sidebar__header-actions">
          <button className="icon-button desktop-only" onClick={onCollapse} aria-label="收起侧栏">
            <PanelLeftClose size={18} />
          </button>
          <button className="icon-button mobile-only" onClick={onMobileClose} aria-label="关闭侧栏">
            <X size={19} />
          </button>
        </div>
      </header>

      <div className="sidebar__primary">
        <Button tone="primary" className="new-session-button" onClick={onNewSession}>
          <Plus size={17} />
          <span>新建会话</span>
        </Button>

        <div className="session-search">
          <Search size={15} />
          <input
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="搜索会话"
            aria-label="搜索会话"
          />
          {query && (
            <button onClick={() => setQuery('')} aria-label="清除搜索">
              <X size={14} />
            </button>
          )}
        </div>
      </div>

      <div className="sidebar__scroll">
        <div className="sidebar-section-title">
          <span>会话</span>
          <span>{sessions.length}</span>
        </div>

        {groups.map((group) => {
          const collapsed = collapsedGroups.has(group.id);
          return (
            <section className="session-group" key={group.id}>
              <button className="session-group__header" onClick={() => toggleGroup(group.id)}>
                {collapsed ? <ChevronRight size={14} /> : <ChevronDown size={14} />}
                {group.icon === 'local' ? <TerminalSquare size={14} /> : <Server size={14} />}
                <span>{group.label}</span>
                <em>{group.sessions.length}</em>
              </button>
              {!collapsed && (
                <div className="session-group__items">
                  {group.sessions.length === 0 ? (
                    <span className="session-group__empty">暂无会话</span>
                  ) : (
                    group.sessions.map((session) => (
                      <div
                        key={session.id}
                        className={`session-row ${currentView === 'terminal' && activeSessionId === session.id ? 'is-active' : ''}`}
                      >
                        <button className="session-row__main" onClick={() => onSelectSession(session)}>
                          <span className={`session-status is-${sessionStatusTone(session.status)}`} />
                          <span className="session-row__copy">
                            <strong>{session.name}</strong>
                            <small>
                              {sessionStatusLabel(session.status)}
                              {session.status === 'running'
                                ? ` · ${persistenceLabel(session.backend ?? session.persistence)}`
                                : ''}
                            </small>
                          </span>
                        </button>
                        <ActionMenu
                          className="row-menu"
                          open={openMenuId === session.id}
                          onOpenChange={(open) => setOpenMenuId(open ? session.id : null)}
                          label={`${session.name} 操作`}
                          trigger={<MoreHorizontal size={16} />}
                        >
                          <button
                            role="menuitem"
                            onClick={() => {
                              setOpenMenuId(null);
                              onRename(session);
                            }}
                          >
                            <Edit3 size={16} /> 重命名
                          </button>
                          <button
                            role="menuitem"
                            onClick={() => {
                              setOpenMenuId(null);
                              onRestart(session);
                            }}
                          >
                            <RefreshCw size={16} /> 重启会话
                          </button>
                          <button
                            role="menuitem"
                            className="danger-text"
                            onClick={() => {
                              setOpenMenuId(null);
                              onDelete(session);
                            }}
                          >
                            <Trash2 size={16} /> 结束会话
                          </button>
                        </ActionMenu>
                      </div>
                    ))
                  )}
                </div>
              )}
            </section>
          );
        })}

        {query && filtered.length === 0 && (
          <div className="sidebar-no-results">
            <CircleDot size={18} />
            <span>没有匹配的会话</span>
          </div>
        )}
      </div>

      <footer className="sidebar__footer">
        <button className={`sidebar-nav ${currentView === 'home' ? 'is-active' : ''}`} onClick={onHome}>
          <TerminalSquare size={17} />
          <span>终端会话</span>
        </button>
        <button className={`sidebar-nav ${currentView === 'hosts' ? 'is-active' : ''}`} onClick={onHosts}>
          <Server size={17} />
          <span>SSH 主机</span>
          <em>{hosts.length}</em>
        </button>
        <button className="sidebar-nav" onClick={onSettings}>
          <span className="user-avatar">{user.username.slice(0, 1).toUpperCase()}</span>
          <span className="sidebar-nav__user">
            <strong>{user.username}</strong>
            <small>管理员</small>
          </span>
          <Settings size={16} />
        </button>
      </footer>
    </aside>
  );
}
