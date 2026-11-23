"use client";

import { useEffect, useState } from "react";

import Link from "next/link";

import { Button } from "@/components/ui/button";
import { ent, leaveEnterprise } from "@/lib/enterprise-api";

type Finding = { id: number; title: string; severity: string; status: string; status_label: string };
type Remediation = { total: number; open: number; doing: number; done: number; accepted: number; closed: number };
type Item = {
  project_id: string;
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

export default function ConclusionsPage() {
  const [items, setItems] = useState<Item[] | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    ent<{ items: Item[] }>("/conclusions")
      .then((data) => setItems(data.items ?? []))
      .catch((reason: Error) => setError(reason.message));
  }, []);

  return (
    <div className="flex flex-1 flex-col gap-4 p-6">
      <div className="flex items-center justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">报告与结论</h1>
          <p className="text-sm text-muted-foreground">只显示授权项目里已经签发的版本。</p>
        </div>
        <Button variant="outline" size="sm" onClick={() => void leaveEnterprise("/login")}>
          退出
        </Button>
      </div>
      {error && <p className="text-sm text-destructive">{error}</p>}
      {items === null && !error && <p className="text-sm text-muted-foreground">正在读取已签发报告…</p>}
      {items && items.length === 0 && (
        <p className="rounded-lg border border-dashed px-4 py-10 text-sm text-muted-foreground">
          授权项目里还没有已签发的报告。草稿和退回的版本不会出现在这里。
        </p>
      )}
      <div className="grid gap-3">
        {items?.map((item) => {
          const important = item.findings.filter((finding) => finding.severity === "critical" || finding.severity === "high");
          return (
            <article key={item.version_id} className="space-y-2 rounded-xl border p-4">
              <div className="flex flex-wrap items-baseline justify-between gap-2">
                <h2 className="font-medium">
                  {item.project_name}
                  <span className="ml-2 text-sm font-normal text-muted-foreground">{item.project_code}</span>
                </h2>
                <p className="text-xs text-muted-foreground">
                  v{item.number} · {item.issuer_name || "未记录签发人"} · {when(item.issued_at)}
                </p>
              </div>
              <p className="text-sm">{item.title}</p>
              <p className="line-clamp-3 text-sm text-muted-foreground">{item.conclusion}</p>
              <p className="text-xs text-muted-foreground">
                发现 {item.remediation.total} 条，待整改 {item.remediation.open}，整改中 {item.remediation.doing}，已整改{" "}
                {item.remediation.done}
                {important.length > 0 ? `，需关注 ${important.map((finding) => finding.title).join("、")}` : "，没有严重或高危发现"}
              </p>
              <Button asChild size="sm" variant="outline">
                <Link href={`/enterprise/conclusions/${item.report_id}`}>打开报告</Link>
              </Button>
            </article>
          );
        })}
      </div>
    </div>
  );
}
