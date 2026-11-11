import { api, setToken, type Meta } from "../api";
import { useAsync } from "../hooks";
import { Button, Card, ErrorState, PageHeader, Spinner } from "../ui";

export function Settings() {
  const meta = useAsync<Meta>(() => api.meta(), []);
  return (
    <>
      <PageHeader title="Settings" subtitle="Server information and your session." />
      <Card title="Server">
        {meta.status === "loading" && <Spinner />}
        {meta.status === "error" && <ErrorState error={meta.error} onRetry={meta.reload} />}
        {meta.status === "ready" && (
          <dl className="grid grid-cols-[180px_1fr] gap-y-2 border-t border-line px-4 py-4 text-[13px]">
            <dt className="text-fg-2">Version</dt>
            <dd className="font-mono">{meta.data.version}</dd>
            <dt className="text-fg-2">Runners available</dt>
            <dd className="font-mono">{meta.data.runners.join(", ")}</dd>
            <dt className="text-fg-2">Shell runner</dt>
            <dd>{meta.data.shell_enabled ? "Enabled (CLOCKWORK_ENABLE_SHELL=true)" : "Disabled"}</dd>
          </dl>
        )}
      </Card>
      <Card title="Session">
        <div className="flex items-center gap-4 border-t border-line px-4 py-4 text-[13px] text-fg-2">
          <span className="flex-1">You are signed in with the admin token, stored in this browser only.</span>
          <Button onClick={() => setToken("")}>Sign out</Button>
        </div>
      </Card>
    </>
  );
}
