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

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader title="Deletion holding tank" />
        <div className="p-4 space-y-2">
          <p className="text-sm text-zinc-500">
            Files deleted locally are kept in a hidden cloud folder for the holding period (default 30 days) before
            being permanently removed. Restore pulls the file back to its original local location.
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
              <span className="ml-auto">
                <Button variant="outline" disabled={busy === f.ID} onClick={() => restore(f.ID)}>
                  {busy === f.ID ? "Restoring…" : "Restore"}
                </Button>
              </span>
            </div>
          ))}
        </div>
      </Card>
    </div>
  );
}
