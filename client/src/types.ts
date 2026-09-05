export type StatusResponse = {
  setupRequired: boolean;
  authenticated: boolean;
  version: string;
  commit?: string | undefined;
};

export type User = {
  username: string;
  createdAt: string;
};

export type AuthType = 'password' | 'privateKey' | 'agent';

export type Host = {
  id: string;
  name: string;
  address: string;
  port: number;
  username: string;
  authType: AuthType;
  fingerprint?: string | undefined;
  hasSecret: boolean;
  createdAt: string;
  updatedAt: string;
};

export type SessionKind = 'local' | 'ssh';
export type PersistenceMode = 'auto' | 'tmux' | 'screen' | 'none';
export type SessionStatus = 'connecting' | 'running' | 'reconnecting' | 'detached' | 'exited' | 'error';

export type Session = {
  id: string;
  name: string;
  kind: SessionKind;
  hostId?: string | undefined;
  hostName?: string | undefined;
  cwd?: string | undefined;
  command?: string | undefined;
  persistence: PersistenceMode;
  backend?: string | undefined;
  backendName?: string | undefined;
  status: SessionStatus;
  generation?: number | undefined;
  cols: number;
  rows: number;
  createdAt: string;
  updatedAt: string;
  lastAttachedAt?: string | undefined;
  exitCode?: number | undefined;
  error?: string | undefined;
};

export type HostInput = {
  name: string;
  address: string;
  port: number;
  username: string;
  authType: AuthType;
  password?: string;
  privateKey?: string;
  passphrase?: string;
};

export type SSHConfigCandidate = {
  alias: string;
  address: string;
  port: number;
  username: string;
  hasIdentityFile: boolean;
  unsupported: string[];
  existingHostId?: string | undefined;
};

export type SSHConfigDiscovery = {
  available: boolean;
  source: string;
  candidates: SSHConfigCandidate[];
};

export type SessionInput = {
  name: string;
  kind: SessionKind;
  hostId?: string;
  cwd: string;
  command: string;
  persistence: PersistenceMode;
};

export type TerminalPreferences = {
  fontSize: number;
  cursorStyle: 'block' | 'bar' | 'underline';
  cursorBlink: boolean;
  scrollback: number;
  theme: 'light' | 'dark' | 'system';
};

export type Toast = {
  id: number;
  tone: 'success' | 'error' | 'info';
  message: string;
};
