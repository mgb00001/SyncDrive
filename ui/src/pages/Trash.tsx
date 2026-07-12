import { useEffect, useState } from "react";
import { api, TrashedFile } from "../api";
import { Badge, Button, Card, CardHeader } from "../components/ui";

function daysLeft(deletedAt: string | null, holdingDays = 30): number {
  if (!deletedAt) return holdingDays;
  const expiry = new Date(deletedAt).getTime() + holdingDays * 86400000;
  return Math.max(0, Math.ceil((expiry - Date.now()) / 86400000));
}

export default function Trash() {
  const [items, setItems] = useState<TrashedFile[]>([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");

  const load = () => api.trash().then(setItems).catch((e) => setError(String(e)));
  useEffect(() => {
    load();
  }, []);

  const restore = async (id: string) => {
    setBusy(id);
    setError("");
    try {
      await api.restore(id);
      await load();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy("");
    }
  };

  const purgeOne = async (f: TrashedFile) => {
    if (!confirm(`Permanently delete "${f.RelativePath}" from the cloud now?\nThis cannot be undone.`)) return;
    setBusy(f.ID);
    setError("");
    try {
      await api.purge(f.ID);
      await load();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy("");
    }
  };

  const purgeAll = async () => {
    if (!confirm(`Permanently delete ALL ${items.length} file(s) in the holding tank now?\nThis cannot be undone.`))
      return;
    setBusy("*");
    setError("");
    try {
      const r = await api.purge();
      await load();
      if (r.purged < items.length) {
        setError(`Purged ${r.purged} of ${items.length} — some accounts may need to be reconnected.`);
      }
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy("");
    }
  };

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader
          title="Deletion holding tank"
          action={
            items.length > 0 ? (
              <Button variant="destructive" disabled={busy === "*"} onClick={purgeAll}>
                {busy === "*" ? "Emptying…" : "Empty holding tank"}
              </Button>
            ) : undefined
          }
        />
        <div className="p-4 space-y-2">
          <p className="text-sm text-zinc-500">
            Files deleted locally are kept in a hidden cloud folder for the holding period (default 30 days) before
            being permanently removed. Restore pulls a file back to its original local location; Delete now frees the
            cloud space immediately.
          </p>
          {error && <p className="text-sm text-red-600">{error}</p>}
          {items.length === 0 && <p className="text-sm text-zinc-500">The holding tank is empty.</p>}
          {items.map((f) => (
            <div
              key={f.ID}
              className="flex items-center gap-3 rounded-md bg-zinc-50 dark:bg-zinc-800/50 px-3 py-2 text-sm"
            >
              <span className="font-medium truncate">{f.RelativePath}</span>
              <Badge tone={daysLeft(f.DeletedAt) <= 5 ? "red" : "amber"}>{daysLeft(f.DeletedAt)} days left</Badge>
              <span className="ml-auto flex gap-2">
                <Button variant="outline" disabled={busy === f.ID} onClick={() => restore(f.ID)}>
                  {busy === f.ID ? "…" : "Restore"}
                </Button>
                <Button variant="destructive" disabled={busy === f.ID} onClick={() => purgeOne(f)}>
                  Delete now
                </Button>
              </span>
            </div>
          ))}
        </div>
      </Card>
    </div>
  );
}
