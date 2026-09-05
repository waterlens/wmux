import { z } from 'zod';

// Response contracts are defined once as zod schemas; the TypeScript types are inferred from them so
// the runtime validation in api.ts and the compile-time shape can never drift apart.

export const statusSchema = z.object({
  setupRequired: z.boolean(),
  authenticated: z.boolean(),
  version: z.string(),
  commit: z.string().optional(),
});
export type StatusResponse = z.infer<typeof statusSchema>;

export const userSchema = z.object({
  username: z.string(),
  createdAt: z.string(),
});
export type User = z.infer<typeof userSchema>;

export const authTypeSchema = z.enum(['password', 'privateKey', 'agent']);
export type AuthType = z.infer<typeof authTypeSchema>;

export const hostSchema = z.object({
  id: z.string(),
  name: z.string(),
  address: z.string(),
  port: z.number().int(),
  username: z.string(),
  authType: authTypeSchema,
  fingerprint: z.string().optional(),
  hasSecret: z.boolean(),
  createdAt: z.string(),
  updatedAt: z.string(),
});
export type Host = z.infer<typeof hostSchema>;

export const sessionKindSchema = z.enum(['local', 'ssh']);
export type SessionKind = z.infer<typeof sessionKindSchema>;

export const persistenceModeSchema = z.enum(['auto', 'tmux', 'screen', 'none']);
export type PersistenceMode = z.infer<typeof persistenceModeSchema>;

export const sessionStatusSchema = z.enum(['connecting', 'running', 'reconnecting', 'detached', 'exited', 'error']);
export type SessionStatus = z.infer<typeof sessionStatusSchema>;

export const sessionSchema = z.object({
  id: z.string(),
  name: z.string(),
  kind: sessionKindSchema,
  hostId: z.string().optional(),
  hostName: z.string().optional(),
  cwd: z.string().optional(),
  command: z.string().optional(),
  persistence: persistenceModeSchema,
  backend: persistenceModeSchema.optional(),
  status: sessionStatusSchema,
  // Bumped by the server on every restart; used as the remount key of the terminal view.
  generation: z.number().int().optional(),
  createdAt: z.string(),
  updatedAt: z.string(),
  lastAttachedAt: z.string().optional(),
});
export type Session = z.infer<typeof sessionSchema>;

export const sshConfigCandidateSchema = z.object({
  alias: z.string(),
  address: z.string(),
  port: z.number().int().min(1).max(65_535),
  username: z.string(),
  hasIdentityFile: z.boolean(),
  unsupported: z.array(z.string()),
  existingHostId: z.string().optional(),
});
export type SSHConfigCandidate = z.infer<typeof sshConfigCandidateSchema>;

export const sshConfigDiscoverySchema = z.object({
  available: z.boolean(),
  source: z.string(),
  candidates: z.array(sshConfigCandidateSchema),
});
export type SSHConfigDiscovery = z.infer<typeof sshConfigDiscoverySchema>;

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

export type Notify = (message: string, tone?: Toast['tone']) => void;
