"use client";

import { useEffect, useState } from "react";

import Link from "next/link";
import { useParams } from "next/navigation";

import { OriginalApproval } from "@/components/enterprise/original-approval";
import { Page } from "@/components/enterprise/shell";
import { Button } from "@/components/ui/button";
import {
  currentProject,
  ent,
  getOriginalRequest,
  type OriginalRequest,
  projectPath,
  setProject,
} from "@/lib/enterprise-api";

type Todo = {
  id: string;
  kind: string;
  kind_label: string;
  project_id?: string;
  subject_id: string;
  report_id?: string;
  preview: string;
};

type Join = { id: string; receipt_no: string; purpose?: string; status_label: string; project_code: string };

export default function TodosPage() {
  const params = useParams<{ projectId?: string }>();
  const project = params.projectId || currentProject();
  const [items, setItems] = useState<Todo[] | null>(null);
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");

  async function load() {
    const data = await ent<{ items: Todo[] }>("/todos");
    setItems(data.items);
  }
  // biome-ignore lint/correctness/useExhaustiveDependencies: load once when the todo page opens
  useEffect(() => {
    void load().catch((reason: Error) => setError(reason.message));
  }, []);

  async function decide(id: string, decision: "approve" | "reject") {
    setError("");
    try {
      const result = await ent<Join & { project_id?: string }>(`/join/${id}/${decision}`, {
        method: "POST",
        body: JSON.stringify({ role: "member" }),
      });
      setMessage(`${result.receipt_no} 现在是${result.status_label}。`);
      await load();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "没有处理成功");
    }
  }

  const visible = (items ?? []).filter((item) => !project || !item.project_id || item.project_id === project);

  return (
    <Page title="待办">
      {!items && !error && <p className="text-sm text-muted-foreground">正在读取待办…</p>}
      {error && <p className="text-sm">{error}</p>}
      {message && <p className="text-sm">{message}</p>}
      {items && visible.length === 0 && <p className="text-sm">{project ? "当前项目没有待办。" : "现在没有待办。"}</p>}
      <div className="max-w-3xl space-y-3">
        {visible.map((item) => (
          <article key={item.id} className="space-y-2 rounded-xl border p-4">
            <h2 className="font-medium">{item.kind_label}</h2>
            <p className="text-sm text-muted-foreground">{item.preview}</p>
            {item.kind === "join_request" && <JoinCard id={item.subject_id} onDecide={decide} />}
            {item.kind === "original_request" && (
              <OriginalRequestCard
                id={item.subject_id}
                onApproved={async (request) => {
                  setMessage(`原件申请${request.status_label}。`);
                  await load();
                }}
              />
            )}
            {item.report_id && item.kind !== "original_request" && (
              <Button asChild size="sm">
                <Link
                  href={
                    item.project_id
                      ? projectPath(item.project_id, `reports/${item.report_id}`)
                      : `/enterprise/reports/detail?id=${item.report_id}`
                  }
                  onClick={() => {
                    if (item.project_id) setProject(item.project_id);
                  }}
                >
                  打开报告
                </Link>
              </Button>
            )}
          </article>
        ))}
      </div>
      {!currentProject() && <p className="text-sm text-muted-foreground">处理加入请求不会签发报告。</p>}
    </Page>
  );
}

function OriginalRequestCard({
  id,
  onApproved,
}: {
  id: string;
  onApproved: (request: OriginalRequest) => Promise<void>;
}) {
  const [request, setRequest] = useState<OriginalRequest | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    const controller = new AbortController();
    void getOriginalRequest(id, controller.signal)
      .then(setRequest)
      .catch((reason: Error) => {
        if (!controller.signal.aborted) setError(reason.message);
      });
    return () => controller.abort();
  }, [id]);
  return (
    <div className="space-y-2 text-sm">
      {error && <p role="alert">{error}</p>}
      {request?.purpose && <p>用途：{request.purpose}</p>}
      {request?.can_approve && (
        <OriginalApproval
          request={request}
          onApproved={async (approval) => {
            setRequest(approval);
            await onApproved(approval);
          }}
        />
      )}
      <Button asChild size="sm" variant="outline">
        <Link href={`/enterprise/evidence?id=${id}`}>打开原件申请</Link>
      </Button>
    </div>
  );
}

function JoinCard({
  id,
  onDecide,
}: {
  id: string;
  onDecide: (id: string, decision: "approve" | "reject") => Promise<void>;
}) {
  const [join, setJoin] = useState<Join | null>(null);
  useEffect(() => {
    void ent<Join>(`/join/${id}`)
      .then(setJoin)
      .catch(() => setJoin(null));
  }, [id]);
  if (!join) return <p className="text-sm">正在打开这张加入请求…</p>;
  return (
    <div className="space-y-2 text-sm">
      <p>
        回执 {join.receipt_no} · 编号 {join.project_code} · {join.status_label}
      </p>
      {join.purpose && <p>用途：{join.purpose}</p>}
      <div className="flex gap-2">
        <Button size="sm" onClick={() => void onDecide(id, "approve")}>
          批准为成员
        </Button>
        <Button size="sm" variant="outline" onClick={() => void onDecide(id, "reject")}>
          拒绝
        </Button>
      </div>
    </div>
  );
}
