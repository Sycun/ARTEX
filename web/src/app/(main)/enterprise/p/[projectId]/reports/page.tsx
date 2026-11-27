"use client";

import { useEffect, useState } from "react";

import Link from "next/link";

import { Page } from "@/components/enterprise/shell";
import { Button } from "@/components/ui/button";
import { ent, projectPath, useEntParams } from "@/lib/enterprise-api";

type Report = {
  id: string;
  title: string;
  number: number;
  status_label: string;
  task_id?: number;
  task_name?: string;
  sample_label?: string;
};

export default function ReportsPage() {
  const projectId = useEntParams<{ projectId: string }>().projectId;
  const [items, setItems] = useState<Report[] | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!projectId) return;
    void ent<{ items: Report[] }>(`/projects/${projectId}/reports`)
      .then((data) => setItems(data.items))
      .catch((reason: Error) => setError(reason.message));
  }, [projectId]);

  return (
    <Page title="报告">
      <div className="max-w-3xl space-y-3">
        {error && <p className="text-sm">{error}</p>}
        {items?.length === 0 && (
          <div className="space-y-2 text-sm">
            <p>当前项目还没有可以查看的报告。</p>
            <p className="text-muted-foreground">只读审核只能看到已签发的脱敏版。未签发的草稿不会出现在这里。</p>
            <Button asChild variant="outline" size="sm">
              <Link href={projectPath(projectId, "tasks")}>返回任务</Link>
            </Button>
          </div>
        )}
        {items?.map((item) => (
          <article key={item.id} className="space-y-2 rounded-xl border p-4 text-sm">
            <h2 className="font-medium">{item.title}</h2>
            <p className="text-muted-foreground">
              v{item.number} {item.status_label}
              {item.sample_label ? ` · ${item.sample_label}` : ""}
            </p>
            {item.task_name && <p>来自任务：{item.task_name}</p>}
            <Button asChild size="sm">
              <Link href={projectPath(projectId, `reports/${item.id}`)}>打开报告</Link>
            </Button>
          </article>
        ))}
      </div>
    </Page>
  );
}
