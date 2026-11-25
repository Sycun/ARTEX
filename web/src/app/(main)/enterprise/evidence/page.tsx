"use client";

import { Suspense, useEffect, useState } from "react";

import { useSearchParams } from "next/navigation";

import { OriginalApproval, originalActionLabel } from "@/components/enterprise/original-approval";
import { Page } from "@/components/enterprise/shell";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { ent, getOriginalRequest, type OriginalRequest, originalDownloadPath } from "@/lib/enterprise-api";

const grantStatus = { active: "有效", expired: "已过期", revoked: "已撤销" };

function Evidence() {
  const id = useSearchParams().get("id") ?? "";
  const [request, setRequest] = useState<OriginalRequest | null>(null);
  const [reason, setReason] = useState("演示报告以脱敏正文为准，不查看原件。");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const item = request?.id === id ? request : null;

  useEffect(() => {
    if (!id) return;
    const controller = new AbortController();
    void getOriginalRequest(id, controller.signal)
      .then(setRequest)
      .catch((reason: Error) => {
        if (!controller.signal.aborted) setError(reason.message);
      });
    return () => controller.abort();
  }, [id]);

  async function decide(action: "reject" | "revoke") {
    setBusy(true);
    setError("");
    try {
      setRequest(
        await ent<OriginalRequest>(`/original-requests/${encodeURIComponent(id)}/${action}`, {
          method: "POST",
          body: action === "reject" ? JSON.stringify({ reason }) : undefined,
        }),
      );
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "操作失败");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Page title="原件申请">
      {error && <p className="text-sm">{error}</p>}
      {item && (
        <div className="max-w-2xl space-y-3 text-sm">
          <p>状态：{item.status_label}</p>
          {item.purpose && <p>用途：{item.purpose}</p>}
          {item.reason && <p>拒绝理由：{item.reason}</p>}
          {item.credential_notice && <p>{item.credential_notice}</p>}
          {item.grants?.map((grant) => (
            <p key={grant.action}>
              {originalActionLabel[grant.action]} · {grantStatus[grant.status]} · 到期时间{" "}
              {new Date(grant.expires_at).toLocaleString()}
            </p>
          ))}
          {item.status === "approved" && !item.previewable && <p>当前不可下载原件。</p>}
          {item.previewable && item.original_id && (
            <Button asChild size="sm">
              <a href={originalDownloadPath(item.original_id)} download={item.original_filename}>
                下载受限原件
              </a>
            </Button>
          )}
          {item.can_approve && <OriginalApproval key={item.id} request={item} onApproved={setRequest} />}
          {item.can_reject && (
            <>
              <Textarea aria-label="拒绝理由" value={reason} onChange={(event) => setReason(event.target.value)} />
              <Button variant="outline" disabled={busy || !reason.trim()} onClick={() => void decide("reject")}>
                拒绝
              </Button>
            </>
          )}
          {item.can_revoke && (
            <Button size="sm" variant="outline" disabled={busy} onClick={() => void decide("revoke")}>
              撤销授权
            </Button>
          )}
          {item.status === "rejected" && (
            <Button
              onClick={() => {
                window.location.href = `/enterprise/reports/detail?id=${item.report_id}`;
              }}
            >
              回到报告草稿
            </Button>
          )}
        </div>
      )}
    </Page>
  );
}

export default function EvidencePage() {
  return (
    <Suspense fallback={<p className="p-6 text-sm">正在打开申请单…</p>}>
      <Evidence />
    </Suspense>
  );
}
