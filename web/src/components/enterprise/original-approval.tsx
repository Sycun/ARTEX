"use client";

import { useState } from "react";

import { Button } from "@/components/ui/button";
import { approveOriginal, type OriginalAction, type OriginalRequest } from "@/lib/enterprise-api";

export const originalActionLabel: Record<OriginalAction, string> = {
  view_original: "查看原件",
  view_credential: "查看凭据",
};

export function OriginalApproval({
  request,
  onApproved,
}: {
  request: OriginalRequest;
  onApproved: (request: OriginalRequest) => void | Promise<void>;
}) {
  const [view, setView] = useState(true);
  const [credential, setCredential] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function approve() {
    const actions: OriginalAction[] = [];
    if (view) actions.push("view_original");
    if (credential) actions.push("view_credential");
    setBusy(true);
    setError("");
    try {
      await onApproved(await approveOriginal(request.id, actions));
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "批准失败");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="space-y-2">
      {request.requires_credential && <p>读取含凭据原件需要同时授予两项权限。</p>}
      <div className="flex flex-wrap items-center gap-3">
        <label className="flex items-center gap-1.5">
          <input type="checkbox" checked={view} onChange={(event) => setView(event.target.checked)} disabled={busy} />
          {originalActionLabel.view_original}
        </label>
        {request.requires_credential && (
          <label className="flex items-center gap-1.5">
            <input
              type="checkbox"
              checked={credential}
              onChange={(event) => setCredential(event.target.checked)}
              disabled={busy}
            />
            {originalActionLabel.view_credential}
          </label>
        )}
        <Button size="sm" disabled={busy || (!view && !credential)} onClick={() => void approve()}>
          批准（8 小时）
        </Button>
      </div>
      {error && <p role="alert">{error}</p>}
    </div>
  );
}
