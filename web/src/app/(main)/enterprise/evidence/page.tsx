"use client";

import { Suspense, useEffect, useState } from "react";

import { useSearchParams } from "next/navigation";

import { Page } from "@/components/enterprise/shell";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { ent } from "@/lib/enterprise-api";

type RequestView = {
  id: string;
  report_id: string;
  status: string;
  status_label: string;
  purpose?: string;
  reason?: string;
  credential_notice?: string;
  previewable: boolean;
};

function Evidence() {
  const id = useSearchParams().get("id") ?? "";
  const [item, setItem] = useState<RequestView | null>(null);
  const [reason, setReason] = useState("演示报告以脱敏正文为准，不查看原件。");
  const [error, setError] = useState("");

  function load() {
    return ent<RequestView>(`/original-requests/${id}`).then(setItem);
  }
  // biome-ignore lint/correctness/useExhaustiveDependencies: reload uses the request id already listed
  useEffect(() => {
    if (!id) return;
    void load().catch((reason: Error) => setError(reason.message));
  }, [id]);

  return (
    <Page title="原件申请">
      {error && <p className="text-sm">{error}</p>}
      {item && (
        <div className="max-w-2xl space-y-3 text-sm">
          <p>状态：{item.status_label}。这张单是原件申请，不是报告复核。</p>
          {item.purpose && <p>用途：{item.purpose}</p>}
          {item.reason && <p>拒绝理由：{item.reason}</p>}
          {item.credential_notice && <p>{item.credential_notice}</p>}
          <p>{item.previewable ? "可以查看画面。" : "预览仍不可用。"}</p>
          {item.status === "pending" && (
            <>
              <Textarea value={reason} onChange={(event) => setReason(event.target.value)} />
              <Button
                disabled={!reason.trim()}
                onClick={() =>
                  void ent(`/original-requests/${id}/reject`, {
                    method: "POST",
                    body: JSON.stringify({ reason }),
                  })
                    .then(() => load())
                    .catch((reason: Error) => setError(reason.message))
                }
              >
                拒绝
              </Button>
            </>
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
