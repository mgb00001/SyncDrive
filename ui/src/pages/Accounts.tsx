import { useEffect, useState } from "react";
import { api, AccountView } from "../api";
import { Badge, Button, Card, CardHeader } from "../components/ui";

const LOW_SPACE_PCT = 20;

function formatBytes(n: number): string {
  if (n <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.min(units.length - 1, Math.floor(Math.log2(n) / 10));
  return `${(n / 2 ** (10 * i)).toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

export default function Accounts({
  onChanged,
  onShowTokenHelp,
}: {
  onChanged: () => void;
  onShowTokenHelp: () => void;
}) {
  const [accounts, setAccounts] = useState<AccountView[]>([]);
  const [error, setError] = useState("");
  const [connecting, setConnecting] = useState(false);

  const load = () => api.accounts().then(setAccounts).catch((e) => setError(String(e)));
  useEffect(() => {
    load();
  }, []);

  const connect = async () => {
    setConnecting(true);
    setError("");
    try {
      await api.addAccount(); // daemon opens the browser for the OAuth consent flow
      await load();
      onChanged();
    } catch (e) {
      setError(String(e));
    } finally {
      setConnecting(false);
    }
  };

  return (
    <Card>
      <CardHeader
        title="Google accounts"
        action={
          <Button onClick={connect} disabled={connecting}>
            {connecting ? "Waiting for browser…" : "Connect account"}
          </Button>
        }
      />
      <div className="p-4 space-y-3 text-sm">
        <p className="text-zinc-500">
          Refresh tokens live in the OS credential vault. When an account drops below {LOW_SPACE_PCT}% free space,
          new files automatically spill over to the next connected account.
        </p>
        {error && <p className="text-red-600">{error}</p>}
        {accounts.map((a) => {
          const usedPct = a.quota_limit > 0 ? (a.quota_usage / a.quota_limit) * 100 : 0;
          const low = a.free_pct < LOW_SPACE_PCT;
          return (
            <div key={a.email} className="rounded-md bg-zinc-50 dark:bg-zinc-800/50 px-3 py-2 space-y-1.5">
              <div className="flex items-center gap-2">
                <span className="font-medium">{a.email}</span>
                {low ? <Badge tone="red">low space — spilling over</Badge> : <Badge tone="green">receiving new files</Badge>}
                {a.token_expired && (
                  <button onClick={onShowTokenHelp} title="Click for how to fix">
                    <Badge tone="red">⚠ sign-in expired — click to fix</Badge>
                  </button>
                )}
                {!a.token_expired && a.token_warning && (
                  <button onClick={onShowTokenHelp} title="Click for how to fix">
                    <Badge tone="amber">⚠ expires in {Math.max(0, a.token_days_left).toFixed(1)}d</Badge>
                  </button>
                )}
                <span className="ml-auto text-xs text-zinc-500">
                  {a.quota_limit > 0
                    ? `${formatBytes(a.quota_usage)} of ${formatBytes(a.quota_limit)} used · ${a.free_pct.toFixed(1)}% free`
                    : "quota unknown"}
                </span>
              </div>
              <div className="h-2 rounded-full bg-zinc-200 dark:bg-zinc-700 overflow-hidden">
                <div
                  className={`h-full rounded-full ${low ? "bg-red-500" : "bg-blue-500"}`}
                  style={{ width: `${Math.min(100, usedPct)}%` }}
                />
              </div>
            </div>
          );
        })}
        {accounts.length === 0 && <p className="text-zinc-500">No accounts connected yet.</p>}
      </div>
    </Card>
  );
}
