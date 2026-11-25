"use client";

import { useEffect, useState } from "react";

import Link from "next/link";
import { useParams } from "next/navigation";

import { Button } from "@/components/ui/button";
import { apiFetch } from "@/lib/api";
import { ent, sha256 } from "@/lib/enterprise-api";

type Finding = { id: number; title: string; severity: string; status: string; status_label: string };
type Remediation = { total: number; open: number; doing: number; done: number; accepted: number; closed: number };
type Item = {
  project_name: string;
  project_code: string;
  report_id: string;
  version_id: string;
  number: number;
  title: string;
  conclusion: string;
  issued_at: string;
  issuer_name: string;
  file_hash: string;
  task_id?: number;
  task_name?: string;
  findings: Finding[];
  remediation: Remediation;
};

function when(value: string) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString("zh-CN");
}

export default function ConclusionDetailPage() {
  const params = useParams<{ reportId: string }>();
  const [items, setItems] = useState<Item[] | null>(null);
  const [error, setError] = useState("");
  const [downloadNote, setDownloadNote] = useState("");

  useEffect(() => {
    ent<{ items: Item[] }>(`/conclusions/${params.reportId}`)
      .then((data) => setItems(data.items ?? []))
      .catch((reason: Error) => setError(reason.message));
  }, [params.reportId]);

  async function download(item: Item) {
    setDownloadNote("");
    setError("");
    const response = await apiFetch(`/api/enterprise/reports/${item.report_id}/versions/${item.version_id}/download`);
    if (!response.ok) {
      setError("没有下载到签发文件");
      return;
    }
    const bytes = await response.arrayBuffer();
    const hash = await sha256(bytes);
    const name = response.headers.get("content-disposition")?.match(/filename=\"?([^\";]+)\"?/)?.[1] || "签发版.md";
    const url = URL.createObjectURL(new Blob([bytes]));
    const link = document.createElement("a");
    link.href = url;
    link.download = name;
    link.click();
    URL.revokeObjectURL(url);
    setDownloadNote(
      hash === item.file_hash
        ? `已下载 v${item.number}。文件哈希与签发记录一致：${hash}`
        : `下载的哈希 ${hash} 和签发记录 ${item.file_hash} 不一致`,
    );
  }

  const latest = items?.[0];

  return (
    <div className="flex flex-1 flex-col gap-4 p-6">
      <div className="flex items-center justify-between gap-3">
        <h1 className="text-xl font-semibold">{latest?.title || "已签发报告"}</h1>
        <Button asChild variant="ghost" size="sm">
          <Link href="/enterprise/conclusions">返回报告与结论</Link>
        </Button>
      </div>
      {error && <p className="text-sm text-destructive">{error}</p>}
      {items === null && !error && <p className="text-sm text-muted-foreground">正在读取签发版…</p>}
      {latest && (
        <p className="text-sm text-muted-foreground">
          {latest.project_name}（{latest.project_code}）
          {latest.task_id ? ` · 来源任务 ${latest.task_name || latest.task_id}` : ""}
        </p>
      )}
      <div className="space-y-4">
        {items?.map((item) => (
          <article key={item.version_id} className="space-y-3 rounded-xl border p-4">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <h2 className="font-medium">v{item.number}</h2>
              <p className="text-xs text-muted-foreground">
                {item.issuer_name || "未记录签发人"} · {when(item.issued_at)}
              </p>
            </div>
            <pre className="whitespace-pre-wrap rounded-lg bg-muted/40 p-3 text-sm">{item.conclusion}</pre>
            <div className="text-sm">
              <p className="font-medium">发现和整改</p>
              {item.findings.length === 0 && <p className="text-muted-foreground">这份签发版没有关联发现。</p>}
              {item.findings.map((finding) => (
                <p key={finding.id}>
                  {finding.title} · {finding.severity || "未分级"} · {finding.status_label}
                </p>
              ))}
              <p className="mt-1 text-muted-foreground">
                共 {item.remediation.total} 条，待整改 {item.remediation.open}，整改中 {item.remediation.doing}，已整改{" "}
                {item.remediation.done}，风险接受 {item.remediation.accepted}，关闭 {item.remediation.closed}
              </p>
            </div>
            <p className="break-all font-mono text-xs text-muted-foreground">签发哈希 {item.file_hash}</p>
            <div className="flex flex-wrap gap-2">
              <Button size="sm" onClick={() => void download(item)}>
                下载签发版
              </Button>
              {item.task_id ? (
                <Button asChild size="sm" variant="outline">
                  <Link href={`/function/tasks/detail?id=${item.task_id}`}>查看来源任务</Link>
                </Button>
              ) : null}
            </div>
          </article>
        ))}
      </div>
      {downloadNote && <p className="text-sm">{downloadNote}</p>}
    </div>
  );
}
