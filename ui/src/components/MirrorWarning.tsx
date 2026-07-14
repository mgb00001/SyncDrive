import { useState } from "react";
import { api, PreflightResult, SensitiveFinding } from "../api";
import { Badge, Button } from "./ui";

// A pending mirror request awaiting the user's decision after preflight.
export interface PendingMirror {
  path: string;
  account: string;
  name: string;
  holdingDays: number;
  result: PreflightResult;
}

const SEV_TONE: Record<string, "red" | "amber" | "neutral"> = {
  high: "red",
  medium: "amber",
  low: "neutral",
};

const CAT_LABEL: Record<SensitiveFinding["category"], string> = {
  secret: "Credential / key file",
  "secret-dir": "Credentials folder",
  "system-dir": "System / app data",
  "cloud-synced": "Already cloud-synced",
  syncdrive: "SyncDrive's own secrets",
};

// useMirrorGuard runs a preflight scan before every mirror. If the scan finds
// nothing, the mirror proceeds immediately; otherwise the caller renders
// <MirrorWarning> with the returned `pending` state so the user can confirm
// or cancel.
export function useMirrorGuard(onDone: () => void, onError: (e: string) => void) {
  const [pending, setPending] = useState<PendingMirror | null>(null);
  const [busy, setBusy] = useState(false);

  const doMirror = async (path: string, account: string, name: string, holdingDays: number) => {
    await api.addFolder(path, account, name, holdingDays);
    await api.triggerSync();
    onDone();
  };

  const requestMirror = async (path: string, account: string, name: string, holdingDays = 30) => {
    setBusy(true);
    try {
      const result = await api.preflight(path);
      if (result.findings.length > 0) {
        setPending({ path, account, name, holdingDays, result });
      } else {
        await doMirror(path, account, name, holdingDays);
      }
    } catch (e) {
      onError(String(e));
    } finally {
      setBusy(false);
    }
  };

  const confirm = async () => {
    if (!pending) return;
    setBusy(true);
    try {
      await doMirror(pending.path, pending.account, pending.name, pending.holdingDays);
      setPending(null);
    } catch (e) {
      onError(String(e));
    } finally {
      setBusy(false);
    }
  };

  const cancel = () => setPending(null);

  return { pending, busy, requestMirror, confirm, cancel };
}

export default function MirrorWarning({
  pending,
  busy,
  onConfirm,
  onCancel,
}: {
  pending: PendingMirror;
  busy: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const { result, path } = pending;
  const high = result.findings.filter((f) => f.severity === "high").length;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4" onClick={onCancel}>
      <div
        className="max-h-[85vh] w-full max-w-xl overflow-y-auto rounded-xl bg-white dark:bg-zinc-900 p-6 shadow-xl space-y-4"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center gap-2">
          <span className="text-xl">⚠️</span>
          <h2 className="text-lg font-semibold">Sensitive data found in this folder</h2>
        </div>

        <p className="text-sm text-zinc-600 dark:text-zinc-400">
          You are about to mirror <code className="break-all">{path}</code> to Google Drive. The scan found{" "}
          <b>{result.findings.length}</b> item{result.findings.length === 1 ? "" : "s"} that may be sensitive
          {high > 0 && (
            <>
              {" "}
              (<b className="text-red-600">{high} high-risk</b>)
            </>
          )}
          . Uploading credentials or private keys to the cloud is risky. Review before continuing.
        </p>

        <div className="divide-y divide-zinc-100 dark:divide-zinc-800 rounded-md border border-zinc-200 dark:border-zinc-800">
          {result.findings.map((f, i) => (
            <div key={i} className="flex items-start gap-2 px-3 py-2 text-sm">
              <Badge tone={SEV_TONE[f.severity]}>{f.severity}</Badge>
              <div className="min-w-0">
                <div className="font-medium">{CAT_LABEL[f.category]}</div>
                <div className="text-zinc-500 break-all">{f.path}</div>
                <div className="text-zinc-400 text-xs">{f.reason}</div>
              </div>
            </div>
          ))}
        </div>

        {result.truncated && (
          <p className="text-xs text-zinc-500">
            Scan stopped early ({result.files_scanned.toLocaleString()} files checked) — there may be more.
          </p>
        )}

        <p className="text-sm text-zinc-600 dark:text-zinc-400">
          Tip: cancel and mirror a specific subfolder (e.g. Documents) instead of a whole home or system directory.
        </p>

        <div className="flex justify-end gap-2">
          <Button variant="outline" onClick={onCancel}>
            Cancel
          </Button>
          <Button variant="destructive" disabled={busy} onClick={onConfirm}>
            {busy ? "Setting up…" : "Mirror anyway"}
          </Button>
        </div>
      </div>
    </div>
  );
}
