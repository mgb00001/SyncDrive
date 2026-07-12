import { useEffect, useState } from "react";
import { api, AccountView, Status } from "./api";
import Folders from "./pages/Folders";
import Trash from "./pages/Trash";
import Accounts from "./pages/Accounts";
import Explorer from "./pages/Explorer";
import TokenHelp from "./components/TokenHelp";
import { Badge, Button } from "./components/ui";

type Tab = "explorer" | "folders" | "trash" | "accounts";

export default function App() {
  const [tab, setTab] = useState<Tab>("folders");
  const [status, setStatus] = useState<Status | null>(null);
  const [accounts, setAccounts] = useState<AccountView[]>([]);
  const [daemonUp, setDaemonUp] = useState(true);
  const [showTokenHelp, setShowTokenHelp] = useState(false);

  const refresh = () => {
    api
      .status()
      .then((s) => {
        setStatus(s);
        setDaemonUp(true);
      })
      .catch(() => setDaemonUp(false));
    api.accounts().then(setAccounts).catch(() => {});
  };

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  const tabs: { id: Tab; label: string }[] = [
    { id: "explorer", label: "Explorer" },
    { id: "folders", label: "Mirrored Folders" },
    { id: "trash", label: "Trash & Recovery" },
    { id: "accounts", label: "Accounts" },
  ];

  const expired = accounts.filter((a) => a.token_expired);
  const warning = accounts.filter((a) => a.token_warning && !a.token_expired);
  const emails = accounts.map((a) => a.email);

  return (
    <div className="mx-auto max-w-4xl p-6 space-y-6">
      <header className="flex items-center justify-between">
        <div>
          <h1 className="text-xl font-bold tracking-tight">SyncDrive</h1>
          <p className="text-sm text-zinc-500">Local-first mirroring to Google Drive</p>
        </div>
        <div className="flex items-center gap-3">
          {(expired.length > 0 || warning.length > 0) && (
            <button onClick={() => setShowTokenHelp(true)} title="Google credentials need attention">
              {expired.length > 0 ? (
                <Badge tone="red">⚠ {expired.length} sign-in{expired.length > 1 ? "s" : ""} expired</Badge>
              ) : (
                <Badge tone="amber">⚠ sign-in expires soon</Badge>
              )}
            </button>
          )}
          {daemonUp ? <Badge tone="green">daemon connected</Badge> : <Badge tone="red">daemon offline</Badge>}
          <Button variant="outline" onClick={() => api.triggerSync().then(refresh)}>
            Sync now
          </Button>
        </div>
      </header>

      <nav className="flex gap-1 border-b border-zinc-200 dark:border-zinc-800">
        {tabs.map((t) => (
          <button
            key={t.id}
            onClick={() => setTab(t.id)}
            className={`px-4 py-2 text-sm font-medium border-b-2 -mb-px transition-colors ${
              tab === t.id
                ? "border-blue-600 text-blue-600"
                : "border-transparent text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100"
            }`}
          >
            {t.label}
          </button>
        ))}
      </nav>

      {!daemonUp && (
        <p className="rounded-md bg-amber-50 dark:bg-amber-900/30 px-4 py-3 text-sm text-amber-800 dark:text-amber-200">
          Cannot reach the SyncDrive daemon on 127.0.0.1:8737. Start <code>syncdrived</code> and this page will
          reconnect automatically.
        </p>
      )}

      {tab === "explorer" && <Explorer accounts={emails} />}
      {tab === "folders" && <Folders accounts={status?.accounts ?? emails} />}
      {tab === "trash" && <Trash />}
      {tab === "accounts" && <Accounts onChanged={refresh} onShowTokenHelp={() => setShowTokenHelp(true)} />}

      {showTokenHelp && <TokenHelp accounts={accounts} onClose={() => setShowTokenHelp(false)} />}
    </div>
  );
}
