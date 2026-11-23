"use client";

import { Suspense, useEffect, useState } from "react";

import Link from "next/link";
import { useParams, useSearchParams } from "next/navigation";

import { Page } from "@/components/enterprise/shell";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { auth } from "@/lib/auth";
import { currentProject, EntError, ent, projectPath, sha256 } from "@/lib/enterprise-api";

type Version = {
  id: string;
  number: number;
  status: string;
  status_label: string;
  body?: string;
  body_hidden?: boolean;
  body_hash?: string;
  return_reason?: string;
  editors: string[];
  lineage: string[];
  download?: boolean;
};

type Report = {
  id: string;
  title: string;
  focus_id: string;
  versions: Version[];
  issued_count: number;
  no_issued_notice?: string;
  waiting?: string;
  next_step?: string;
  action_hint?: string;
  actions: Record<string, boolean>;
  original?: { id: string; filename: string; created_at: string; notice: string };
  case_count: number;
  project_id?: string;
  finding_id?: number;
  task_id?: number;
  task_name?: string;
  sample_label?: string;
};

function ReportDetail() {
  const params = useParams<{ projectId?: string; reportId?: string }>();
  const search = useSearchParams();
  const id = params.reportId || search.get("id") || "";
  const [report, setReport] = useState<Report | null>(null);
  const [body, setBody] = useState("");
  const [reason, setReason] = useState("");
  const [purpose, setPurpose] = useState("核对演示发现");
  const [message, setMessage] = useState("");
  const [hash, setHash] = useState("");
  const [fileText, setFileText] = useState("");
  const [error, setError] = useState("");
  const project = params.projectId || report?.project_id || currentProject();

  async function load(reportID = id) {
    const next = await ent<Report>(`/reports/${reportID}`);
    setReport(next);
    const focus = next.versions.find((item) => item.id === next.focus_id);
    if (focus?.body) setBody(focus.body);
    return next;
  }

  // biome-ignore lint/correctness/useExhaustiveDependencies: reload uses the report id already listed
  useEffect(() => {
    if (!id) return;
    void load().catch((reason: Error) => setError(reason.message));
  }, [id]);

  const focus = report?.versions.find((item) => item.id === report.focus_id);

  async function run(action: () => Promise<void>) {
    setError("");
    setMessage("");
    try {
      await action();
      if (id) await load(id);
    } catch (reason) {
      const data = reason instanceof EntError ? reason.data : {};
      const next = typeof data.next === "string" ? `下一步：${data.next}。` : "";
      const actor = typeof data.actor_name === "string" ? `操作者是${data.actor_name}。` : "";
      setError(`${reason instanceof Error ? reason.message : "没有完成"} ${actor}${next}`);
    }
  }

  return (
    <Page title={report ? `${report.title}` : "报告"}>
      {error && <p className="max-w-3xl rounded-lg border border-destructive/30 px-3 py-2 text-sm">{error}</p>}
      {message && <p className="max-w-3xl rounded-lg border px-3 py-2 text-sm">{message}</p>}
      {report && (
        <div className="max-w-3xl space-y-4">
          {report.sample_label && <p className="text-sm">{report.sample_label}</p>}
          {report.task_name && <p className="text-sm">来自任务：{report.task_name}</p>}
          <p className="text-sm text-muted-foreground">
            {focus ? `v${focus.number} ${focus.status_label}` : report.no_issued_notice || "尚无签发版"}
            。归并组 {report.case_count}。已签发 {report.issued_count}。
          </p>
          {report.waiting && <p className="text-sm">正在等谁：{report.waiting}</p>}
          {report.next_step && <p className="text-sm">下一步：{report.next_step}</p>}
          {report.action_hint && <p className="text-sm">{report.action_hint}</p>}
          {focus?.return_reason && (
            <p className="rounded-lg border px-3 py-2 text-sm">退回说明：{focus.return_reason}</p>
          )}
          {focus && !focus.body_hidden && report.actions.save && (
            <Textarea value={body} onChange={(event) => setBody(event.target.value)} className="min-h-40" />
          )}
          {focus && !focus.body_hidden && !report.actions.save && focus.body && (
            <pre className="whitespace-pre-wrap rounded-lg border p-3 text-sm">{focus.body}</pre>
          )}
          {focus?.body_hidden && <p className="text-sm">这一版的正文现在不能打开。</p>}
          <div className="flex flex-wrap gap-2">
            {report.actions.save && (
              <Button
                onClick={() =>
                  void run(async () => {
                    await ent(`/reports/${report.id}/draft`, { method: "PUT", body: JSON.stringify({ body }) });
                    setMessage("已保存。你可以继续修改，或送审这一版。");
                  })
                }
              >
                保存
              </Button>
            )}
            {report.actions.submit && (
              <Button
                onClick={() =>
                  void run(async () => {
                    const saved = await ent<{ id: string; body_hash: string }>(`/reports/${report.id}/draft`, {
                      method: "PUT",
                      body: JSON.stringify({ body }),
                    });
                    const result = await ent<{ message: string }>(`/reports/${report.id}/versions/${saved.id}/submit`, {
                      method: "POST",
                      body: JSON.stringify({ body_hash: saved.body_hash }),
                    });
                    setMessage(result.message);
                  })
                }
              >
                送审
              </Button>
            )}
            {report.actions.withdraw && focus && (
              <Button
                variant="outline"
                onClick={() =>
                  void run(async () => {
                    const result = await ent<{ message: string }>(
                      `/reports/${report.id}/versions/${focus.id}/withdraw`,
                      {
                        method: "POST",
                        body: JSON.stringify({ body_hash: focus.body_hash }),
                      },
                    );
                    setMessage(result.message);
                  })
                }
              >
                撤回送审
              </Button>
            )}
            {report.actions.issue && focus && (
              <Button
                onClick={() =>
                  void run(async () => {
                    const result = await ent<{ message: string; file_hash?: string }>(
                      `/reports/${report.id}/versions/${focus.id}/issue`,
                      { method: "POST", body: JSON.stringify({ body_hash: focus.body_hash }) },
                    );
                    setMessage(`${result.message}${result.file_hash ? ` 文件哈希 ${result.file_hash}` : ""}`);
                  })
                }
              >
                签发此冻结版
              </Button>
            )}
            {focus?.download && (
              <Button
                variant="outline"
                onClick={() => void download(report.id, focus.id, setHash, setFileText, setError)}
              >
                下载脱敏版
              </Button>
            )}
          </div>
          {report.actions.return && focus && (
            <div className="space-y-2">
              <Textarea value={reason} onChange={(event) => setReason(event.target.value)} placeholder="填写退回理由" />
              <Button
                variant="outline"
                disabled={!reason.trim()}
                onClick={() =>
                  void run(async () => {
                    const result = await ent<{ message: string; credential_notice?: string }>(
                      `/reports/${report.id}/versions/${focus.id}/return`,
                      { method: "POST", body: JSON.stringify({ body_hash: focus.body_hash, reason }) },
                    );
                    setMessage(`${result.message} ${result.credential_notice ?? ""}`);
                    setReason("");
                  })
                }
              >
                退回
              </Button>
            </div>
          )}
          {hash && <p className="font-mono text-xs break-all">刚下载文件的 SHA-256：{hash}</p>}
          {fileText && <pre className="whitespace-pre-wrap rounded-lg border p-3 text-sm">{fileText}</pre>}
          {report.original && (
            <section className="space-y-2 rounded-xl border p-4 text-sm">
              <p>
                {report.original.filename} · {report.original.notice}
              </p>
              <p>原件申请和报告复核是两件待办。拒绝原件不会退回报告。</p>
              {report.actions.request_original && (
                <>
                  <Textarea value={purpose} onChange={(event) => setPurpose(event.target.value)} />
                  <Button
                    variant="outline"
                    onClick={() =>
                      void run(async () => {
                        const request = await ent<{ id: string }>(`/reports/${report.id}/original-requests`, {
                          method: "POST",
                          body: JSON.stringify({ purpose }),
                        });
                        window.location.href = `/enterprise/evidence?id=${request.id}`;
                      })
                    }
                  >
                    申请查看原件
                  </Button>
                </>
              )}
            </section>
          )}
          <div className="space-y-2">
            {report.versions.map((version) => (
              <p key={version.id} className="text-sm text-muted-foreground">
                v{version.number} {version.status_label}
                {version.body_hash ? ` · ${version.body_hash}` : ""}
                {(version.editors ?? []).length ? ` · 编辑者 ${version.editors.join("、")}` : ""}
                {(version.lineage ?? []).length ? ` · 来源贡献人 ${version.lineage.join("、")}` : ""}
              </p>
            ))}
          </div>
          <div className="flex flex-wrap gap-2">
            {project && report.task_id ? (
              <Button asChild variant="outline">
                <Link href={projectPath(project, `tasks/${report.task_id}`)}>返回任务</Link>
              </Button>
            ) : null}
            <Button asChild variant="ghost">
              <Link
                href={
                  project && report.finding_id
                    ? projectPath(project, `findings/${report.finding_id}`)
                    : "/enterprise/findings"
                }
              >
                返回发现
              </Link>
            </Button>
            {project && (
              <Button asChild variant="ghost">
                <Link href={projectPath(project)}>返回项目首页</Link>
              </Button>
            )}
          </div>
        </div>
      )}
    </Page>
  );
}

async function download(
  reportID: string,
  versionID: string,
  setHash: (value: string) => void,
  setFileText: (value: string) => void,
  setError: (value: string) => void,
) {
  const token = auth.getToken();
  const response = await fetch(`/api/enterprise/reports/${reportID}/versions/${versionID}/download`, {
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  });
  if (!response.ok) {
    const data = (await response.json().catch(() => ({}))) as { message?: string };
    setError(data.message || "找不到可下载的签发文件");
    return;
  }
  const bytes = await response.arrayBuffer();
  const digest = await sha256(bytes);
  const headerHash = response.headers.get("X-Content-SHA256") ?? "";
  setHash(headerHash && headerHash !== digest ? `${digest}（与记录不一致）` : digest);
  setFileText(new TextDecoder().decode(bytes));
  const blob = new Blob([bytes], { type: "text/markdown" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  const named = (response.headers.get("Content-Disposition") ?? "").match(/filename\*=UTF-8''([^;]+)/)?.[1];
  link.download = named ? decodeURIComponent(named) : "脱敏报告.md";
  link.click();
  URL.revokeObjectURL(url);
}

export default function ReportPage() {
  return (
    <Suspense fallback={<p className="p-6 text-sm">正在打开报告…</p>}>
      <ReportDetail />
    </Suspense>
  );
}
