// Thin client for the syncdrived loopback JSON API.
const BASE = "http://127.0.0.1:8737";

async function req<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    headers: { "Content-Type": "application/json" },
    ...init,
  });
  if (!res.ok) {
    throw new Error(await res.text());
  }
  return res.json() as Promise<T>;
}

export interface FolderTarget {
  ID: number;
  LocalRootPath: string;
  GoogleAccountID: string;
  RemoteParentFolderID: string;
}

export interface MirroredFolder {
  LocalRootPath: string;
  IsPaused: boolean;
  VersioningEnabled: boolean;
  HoldingPeriodDays: number;
  targets: FolderTarget[];
}

export interface TrashedFile {
  ID: string;
  RelationID: number;
  RelativePath: string;
  DeletedAt: string | null;
  LocalSize: number;
}

export interface Status {
  folders: number;
  accounts: string[] | null;
  trashed: number;
  time: string;
}

export interface AccountView {
  email: string;
  quota_limit: number;
  quota_usage: number;
  free_pct: number;
  token_days_left: number; // -1 = no expiry tracking (production client)
  token_warning: boolean;
  token_expired: boolean;
}

export interface BrowseEntry {
  name: string;
  path: string;
  is_dir: boolean;
  size: number;
  status: "none" | "mirroring" | "paused" | "pending" | "conflict" | "tank" | "contains";
  ghost?: boolean;
  file_id?: string;
  mirror_root?: string;
}

export interface BrowseResponse {
  path: string;
  parent?: string;
  roots: string[];
  entries: BrowseEntry[];
}

export const api = {
  status: () => req<Status>("/api/status"),
  folders: () => req<MirroredFolder[]>("/api/folders"),
  addFolder: (local_root_path: string, account: string, remote_folder_name: string, holding_period_days: number) =>
    req("/api/folders", { method: "POST", body: JSON.stringify({ local_root_path, account, remote_folder_name, holding_period_days }) }),
  pauseFolder: (local_root_path: string, paused: boolean) =>
    req("/api/folders/pause", { method: "POST", body: JSON.stringify({ local_root_path, paused }) }),
  removeFolder: (local_root_path: string) =>
    req("/api/folders", { method: "DELETE", body: JSON.stringify({ local_root_path }) }),
  trash: () => req<TrashedFile[]>("/api/trash"),
  restore: (file_id: string) =>
    req("/api/trash/restore", { method: "POST", body: JSON.stringify({ file_id }) }),
  accounts: () => req<AccountView[]>("/api/accounts"),
  browse: (path?: string) => req<BrowseResponse>(`/api/browse${path ? `?path=${encodeURIComponent(path)}` : ""}`),
  addAccount: () => req<{ email: string }>("/api/accounts", { method: "POST" }),
  shareUser: (account: string, remote_id: string, email: string) =>
    req("/api/share/user", { method: "POST", body: JSON.stringify({ account, remote_id, email }) }),
  shareLink: (account: string, remote_id: string) =>
    req<{ link: string }>("/api/share/link", { method: "POST", body: JSON.stringify({ account, remote_id }) }),
  triggerSync: () => req("/api/sync", { method: "POST" }),
};
