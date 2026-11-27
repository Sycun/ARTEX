"use client";

import { Suspense, useEffect, useState } from "react";

import Link from "next/link";
import { useSearchParams } from "next/navigation";

import { Page } from "@/components/enterprise/shell";
import { Button } from "@/components/ui/button";
import { ent, projectPath, useEntParams } from "@/lib/enterprise-api";

type Finding = { id: number; title: string; summary: string; report_id?: string; issued: boolean };
type Asset = { id: number; name: string; kind: string; url?: string; title?: string };
type Detail = {
  id: number;
  name: string;
  description: string;
  goal: string;
  status_label: string;
  queued: boolean;
  running: boolean;
  completed_at?: string;
  sample_label?: string;
  project_name: string;
  findings: Finding[];
  assets: Asset[];
  limit_note?: string;
};

const TABS = [
  { id: "overview", label: "概览" },
  { id: "assets", label: "关联资产" },
  { id: "findings", label: "发现" },
];

function TaskDetail() {
  const params = useEntParams<{ projectId: string; taskId: string }>();
  const search = useSearchParams();
  const tab = TABS.some((item) => item.id === search.get("tab")) ? (search.get("tab") as string) : "overview";
  const [detail, setDetail] = useState<Detail | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!params.projectId || !params.taskId) return;
    void ent<Detail>(`/projects/${params.projectId}/tasks/${params.taskId}`)
      .then(setDetail)
      .catch((reason: Error) => setError(reason.message));
  }, [params.projectId, params.taskId]);

  const base = projectPath(params.projectId || "", `tasks/${params.taskId || ""}`);

  return (
    <Page title={detail?.name || "任务"}>
      <div className="max-w-3xl space-y-4">
        <p className="text-sm text-muted-foreground">
          <Link className="underline-offset-4 hover:underline" href={projectPath(params.projectId || "")}>
            项目首页
          </Link>
          {" / "}
          <Link className="underline-offset-4 hover:underline" href={projectPath(params.projectId || "", "tasks")}>
            任务
          </Link>
          {detail ? ` / ${detail.name}` : ""}
        </p>
        {error && (
          <div className="space-y-2 text-sm">
            <p>{error === "找不到" ? "找不到这个任务。" : error}</p>
            <Button asChild variant="outline" size="sm">
              <Link href={projectPath(params.projectId || "", "tasks")}>返回任务</Link>
            </Button>
          </div>
        )}
        {detail && (
          <>
            <div className="flex flex-wrap gap-2">
              {TABS.map((item) => (
                <Button key={item.id} asChild size="sm" variant={tab === item.id ? "default" : "outline"}>
                  <Link href={item.id === "overview" ? base : `${base}?tab=${item.id}`}>{item.label}</Link>
                </Button>
              ))}
            </div>
            {tab === "overview" && (
              <section className="space-y-2 text-sm">
                <p>状态：{detail.status_label}</p>
                <p>目标：{detail.goal}</p>
                <p>{detail.description}</p>
                {detail.sample_label && <p>{detail.sample_label}</p>}
                <p>完成时间：{detail.completed_at ? new Date(detail.completed_at).toLocaleString() : "尚未完成"}</p>
                <p>
                  {detail.running ? "当前正在运行。" : "当前没有运行。"}
                  {detail.queued ? "已在队列中。" : "没有排队。"}
                </p>
              </section>
            )}
            {tab === "assets" && (
              <section className="space-y-3">
                {detail.assets.length === 0 && <p className="text-sm">当前任务没有关联资产。</p>}
                {detail.assets.map((asset) => (
                  <article key={asset.id} className="space-y-1 rounded-xl border p-4 text-sm">
                    <h2 className="font-medium">{asset.name}</h2>
                    {asset.title && <p>{asset.title}</p>}
                    {asset.url && <p className="text-muted-foreground">{asset.url}</p>}
                  </article>
                ))}
              </section>
            )}
            {tab === "findings" && (
              <section className="space-y-3">
                {detail.findings.length === 0 && <p className="text-sm">当前任务没有关联发现。</p>}
                {detail.findings.map((finding) => (
                  <article key={finding.id} className="space-y-2 rounded-xl border p-4 text-sm">
                    <h2 className="font-medium">{finding.title}</h2>
                    <p>{finding.summary}</p>
                    <div className="flex flex-wrap gap-2">
                      <Button asChild size="sm" variant="outline">
                        <Link href={projectPath(params.projectId || "", `findings/${finding.id}`)}>打开发现</Link>
                      </Button>
                      {finding.report_id && finding.issued && (
                        <Button asChild size="sm">
                          <Link href={projectPath(params.projectId || "", `reports/${finding.report_id}`)}>打开报告</Link>
                        </Button>
                      )}
                    </div>
                  </article>
                ))}
              </section>
            )}
            {detail.limit_note && (
              <p className="rounded-lg border bg-muted/40 px-3 py-2 text-sm">{detail.limit_note}</p>
            )}
            <Button asChild variant="outline">
              <Link href={projectPath(params.projectId || "", "tasks")}>返回任务</Link>
            </Button>
          </>
        )}
      </div>
    </Page>
  );
}

export function TaskDetailClientPage() {
  return (
    <Suspense fallback={<p className="p-6 text-sm">正在打开任务…</p>}>
      <TaskDetail />
    </Suspense>
  );
}
