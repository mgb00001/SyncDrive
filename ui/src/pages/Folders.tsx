import { useEffect, useState } from "react";
import { api, MirroredFolder } from "../api";
import { Badge, Button, Card, CardHeader, Input } from "../components/ui";

export default function Folders({ accounts }: { accounts: string[] }) {
  const [folders, setFolders] = useState<MirroredFolder[]>([]);
  const [error, setError] = useState("");
  const [newPath, setNewPath] = useState("");
  const [newAccount, setNewAccount] = useState("");
  const [shareEmail, setShareEmail] = useState("");
  const [link, setLink] = useState("");

  const load = () => api.folders().then(setFolders).catch((e) => setError(String(e)));
  useEffect(() => {
    load();
  }, []);

  const add = async () => {
    setError("");
    try {
      await api.addFolder(newPath, newAccount || accounts[0] || "", "SyncDrive", 30);
      setNewPath("");
      load();
    } catch (e) {
      setError(String(e));
    }
  };

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader title="Add a mirrored folder" />
        <div className="flex flex-col gap-2 p-4 sm:flex-row">
          <Input
            placeholder="Local folder path, e.g. C:\Users\me\Documents"
            value={newPath}
            onChange={(e) => setNewPath(e.target.value)}
          />
          <select
            className="rounded-md border border-zinc-300 dark:border-zinc-700 bg-transparent px-3 py-1.5 text-sm"
            value={newAccount}
            onChange={(e) => setNewAccount(e.target.value)}
          >
            <option value="">{accounts.length ? "Choose account" : "No accounts yet"}</option>
            {accounts.map((a) => (
              <option key={a} value={a}>
                {a}
              </option>
            ))}
          </select>
          <Button onClick={add} disabled={!newPath || accounts.length === 0}>
            Mirror it
          </Button>
        </div>
      </Card>

      {error && <p className="text-sm text-red-600">{error}</p>}
      {link && (
        <p className="rounded-md bg-emerald-50 dark:bg-emerald-900/30 px-4 py-2 text-sm break-all">
          Public link: <a className="underline" href={link}>{link}</a>
        </p>
      )}

      {folders.map((f) => (
        <Card key={f.LocalRootPath}>
          <CardHeader
            title={f.LocalRootPath}
            action={
              <div className="flex gap-2">
                {f.IsPaused ? <Badge tone="amber">paused</Badge> : <Badge tone="green">mirroring</Badge>}
                <Button variant="outline" onClick={() => api.pauseFolder(f.LocalRootPath, !f.IsPaused).then(load)}>
                  {f.IsPaused ? "Resume" : "Pause"}
                </Button>
                <Button
                  variant="destructive"
                  onClick={() => {
                    if (confirm(`Stop mirroring ${f.LocalRootPath}? Cloud copies are left untouched.`))
                      api.removeFolder(f.LocalRootPath).then(load);
                  }}
                >
                  Remove
                </Button>
              </div>
            }
          />
          <div className="p-4 space-y-3 text-sm">
            <p className="text-zinc-500">
              Holding period: {f.HoldingPeriodDays} days · {f.targets?.length ?? 0} cloud target(s)
            </p>
            {(f.targets ?? []).map((t) => (
              <div key={t.ID} className="flex flex-wrap items-center gap-2 rounded-md bg-zinc-50 dark:bg-zinc-800/50 px-3 py-2">
                <span className="font-medium">{t.GoogleAccountID}</span>
                <span className="text-zinc-400 text-xs">→ {t.RemoteParentFolderID}</span>
                <span className="ml-auto flex gap-2">
                  <Input
                    placeholder="share with email…"
                    className="w-48"
                    value={shareEmail}
                    onChange={(e) => setShareEmail(e.target.value)}
                  />
                  <Button
                    variant="outline"
                    disabled={!shareEmail}
                    onClick={() =>
                      api
                        .shareUser(t.GoogleAccountID, t.RemoteParentFolderID, shareEmail)
                        .then(() => setShareEmail(""))
                        .catch((e) => setError(String(e)))
                    }
                  >
                    Share
                  </Button>
                  <Button
                    variant="outline"
                    onClick={() =>
                      api
                        .shareLink(t.GoogleAccountID, t.RemoteParentFolderID)
                        .then((r) => setLink(r.link))
                        .catch((e) => setError(String(e)))
                    }
                  >
                    Public link
                  </Button>
                </span>
              </div>
            ))}
          </div>
        </Card>
      ))}
      {folders.length === 0 && (
        <p className="text-sm text-zinc-500">No folders are being mirrored yet. Add one above to get started.</p>
      )}
    </div>
  );
}
