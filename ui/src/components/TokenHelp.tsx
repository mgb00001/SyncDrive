import { AccountView } from "../api";
import { Button } from "./ui";

// Modal explaining how to refresh or replace Google credentials when a
// refresh token is close to its 7-day Testing-mode expiry.
export default function TokenHelp({
  accounts,
  onClose,
}: {
  accounts: AccountView[];
  onClose: () => void;
}) {
  const affected = accounts.filter((a) => a.token_warning || a.token_expired);
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4" onClick={onClose}>
      <div
        className="max-h-[85vh] w-full max-w-lg overflow-y-auto rounded-xl bg-white dark:bg-zinc-900 p-6 shadow-xl space-y-4"
        onClick={(e) => e.stopPropagation()}
      >
        <h2 className="text-lg font-semibold">Google credentials need attention</h2>

        <div className="space-y-1 text-sm">
          {affected.length === 0 && <p className="text-zinc-500">No accounts currently need attention.</p>}
          {affected.map((a) => (
            <p key={a.email}>
              <span className="font-medium">{a.email}</span>{" "}
              {a.token_expired ? (
                <span className="text-red-600">— sign-in has expired.</span>
              ) : (
                <span className="text-amber-600">— expires in about {Math.max(0, a.token_days_left).toFixed(1)} days.</span>
              )}
            </p>
          ))}
        </div>

        <div className="space-y-3 text-sm">
          <div>
            <h3 className="font-semibold">Why this happens</h3>
            <p className="text-zinc-600 dark:text-zinc-400">
              Your OAuth client is in <b>Testing</b> mode in the Google Cloud Console, so Google expires every
              sign-in after 7 days. This is a Google policy, not a SyncDrive setting.
            </p>
          </div>
          <div>
            <h3 className="font-semibold">Quick fix (buys another 7 days)</h3>
            <ol className="list-decimal ml-5 space-y-1 text-zinc-600 dark:text-zinc-400">
              <li>Go to the <b>Accounts</b> tab and click <b>Connect account</b>.</li>
              <li>Sign in with the same Google account. The 7-day clock restarts.</li>
            </ol>
          </div>
          <div>
            <h3 className="font-semibold">Permanent fix (production credentials)</h3>
            <ol className="list-decimal ml-5 space-y-1 text-zinc-600 dark:text-zinc-400">
              <li>
                In the Google Cloud Console open <b>APIs &amp; Services → OAuth consent screen</b> and click{" "}
                <b>Publish app</b> (moves it from Testing to In production).
              </li>
              <li>
                If you created a new OAuth client for production, download its JSON and replace{" "}
                <code>credentials.json</code> in the SyncDrive folder.
              </li>
              <li>
                Restart the daemon with expiry warnings off:{" "}
                <code>syncdrived -secrets credentials.json -token-lifetime-days 0</code>
              </li>
              <li>Reconnect each account once via <b>Connect account</b>. Tokens then no longer expire.</li>
            </ol>
          </div>
        </div>

        <div className="flex justify-end">
          <Button onClick={onClose}>Got it</Button>
        </div>
      </div>
    </div>
  );
}
