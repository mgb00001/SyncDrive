import { useEffect, useState } from "react";
import { api, BrowseEntry, BrowseResponse } from "../api";
import { Badge, Button, Card, CardHeader } from "../components/ui";
import MirrorWarning, { useMirrorGuard } from "../components/MirrorWarning";

// Dot colour per sync status.
const DOT: Record<string, { cls: string; label: string }> = {
  mirroring: { cls: "bg-emerald-500", label: "mirroring" },
  paused: { cls: "bg-zinc-400", label: "paused" },
  pending: { cls: "bg-blue-500", label: "waiting to sync" },
  conflict: { cls: "bg-red-500", label: "conflict" },
  tank: { cls: "bg-amber-500", label: "in holding tank" },
  contains: { cls: "bg-emerald-300", label: "contains mirrored folders" },
  none: { cls: "bg-transparent border border-zinc-400", label: "not mirrored" },
};

function formatBytes(n: number): string {
  if (n <= 0) return "";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.min(units.length - 1, Math.floor(Math.log2(n) / 10));
  return `${(n / 2 ** (10 * i)).toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

export default function Explorer({ accounts }: { accounts: string[] }) {
  const [view, setView] = useState<BrowseResponse | null>(null);
  const [error, setError] = useState("");
  const [busyPath, setBusyPath] = useState("");

  const load = (path?: string) =>
    api
      .browse(path)
      .then((v) => {
        setView(v);
        setError("");
      })
      .catch((e) => setError(String(e)));

  useEffect(() => {
    load();
  }, []);

  const guard = useMirrorGuard(
    () => load(view?.path),
    (e) => setError(e),
  );

  const mirrorFolder = (entry: BrowseEntry) => {
    const account = accounts[0];
    if (!account) {
      setError("Connect a Google account first (Accounts tab).");
      return;
    }
    setError("");
    const target = entry.is_dir ? entry.path : entry.path.slice(0, entry.path.lastIndexOf("/"));
    const name = target.split("/").pop() || "SyncDrive";
    guard.requestMirror(target, account, name, 30);
  };

  const restoreGhost = async (entry: BrowseEntry) => {
    if (!entry.file_id) return;
    setBusyPath(entry.path);
    try {
      await api.restore(entry.file_id);
      await load(view?.path);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusyPath("");
    }
  };

  const crumbs = (view?.path ?? "").split("/").filter(Boolean);

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader
          title="Explorer"
          action={
            <div className="flex gap-1">
              {(view?.roots ?? []).map((r) => (
                <Button key={r} variant="ghost" onClick={() => load(r)}>
                  {r.length <= 3 ? r : "🏠 Home"}
                </Button>
              ))}
            </div>
          }
        />
        <div className="p-4 space-y-3">
          {/* breadcrumbs */}
          <div className="flex flex-wrap items-center gap-1 text-sm">
            {crumbs.map((c, i) => {
              const p = crumbs.slice(0, i + 1).join("/") + (i === 0 && c.endsWith(":") ? "/" : "");
              return (
                <span key={p} className="flex items-center gap-1">
                  {i > 0 && <span className="text-zinc-400">/</span>}
                  <button className="text-blue-600 hover:underline" onClick={() => load(p)}>
                    {c}
                  </button>
                </span>
              );
            })}
          </div>

          {/* legend */}
          <div className="flex flex-wrap gap-3 text-xs text-zinc-500">
            {Object.entries(DOT).map(([k, v]) => (
              <span key={k} className="inline-flex items-center gap-1">
                <span className={`inline-block h-2.5 w-2.5 rounded-full ${v.cls}`} /> {v.label}
              </span>
            ))}
          </div>

          {error && <p className="text-sm text-red-600">{error}</p>}

          {/* entries */}
          <div className="divide-y divide-zinc-100 dark:divide-zinc-800">
            {view?.parent && (
              <button
                className="flex w-full items-center gap-2 px-2 py-1.5 text-sm hover:bg-zinc-50 dark:hover:bg-zinc-800/50 rounded"
                onClick={() => load(view.parent)}
              >
                <span className="w-2.5" />
                <span>📁 ..</span>
              </button>
            )}
            {(view?.entries ?? []).map((e) => {
              const dot = DOT[e.status] ?? DOT.none;
              return (
                <div
                  key={e.path + (e.ghost ? ":ghost" : "")}
                  className={`flex items-center gap-2 px-2 py-1.5 text-sm rounded hover:bg-zinc-50 dark:hover:bg-zinc-800/50 ${
                    e.ghost ? "opacity-60" : ""
                  }`}
                >
                  <span title={dot.label} className={`inline-block h-2.5 w-2.5 shrink-0 rounded-full ${dot.cls}`} />
                  {e.is_dir ? (
                    <button className="flex-1 text-left truncate" onClick={() => load(e.path)}>
                      📁 {e.name}
                    </button>
                  ) : (
                    <span className="flex-1 truncate">
                      📄 {e.name}
                      {e.ghost && <span className="ml-2 text-xs text-amber-600">(deleted — in holding tank)</span>}
                    </span>
                  )}
                  <span className="text-xs text-zinc-400 w-16 text-right">{formatBytes(e.size)}</span>
                  <span className="w-28 text-right">
                    {e.status === "none" && (
                      <Button
                        variant="outline"
                        className="px-2 py-0.5 text-xs"
                        disabled={guard.busy}
                        onClick={() => mirrorFolder(e)}
                        title={e.is_dir ? "Start mirroring this folder" : "Mirror this file's folder"}
                      >
                        {guard.busy ? "…" : "⚑ Mirror"}
                      </Button>
                    )}
                    {e.ghost && (
                      <Button
                        variant="outline"
                        className="px-2 py-0.5 text-xs"
                        disabled={busyPath === e.path}
                        onClick={() => restoreGhost(e)}
                      >
                        {busyPath === e.path ? "…" : "Restore"}
                      </Button>
                    )}
                    {e.status === "conflict" && <Badge tone="red">conflict</Badge>}
                  </span>
                </div>
              );
            })}
            {view && view.entries.length === 0 && (
              <p className="px-2 py-3 text-sm text-zinc-500">Empty folder.</p>
            )}
          </div>
        </div>
      </Card>

      {guard.pending && (
        <MirrorWarning pending={guard.pending} busy={guard.busy} onConfirm={guard.confirm} onCancel={guard.cancel} />
      )}
    </div>
  );
}
