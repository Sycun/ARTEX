"use client";

import { Suspense, useEffect, useState } from "react";

import Link from "next/link";
import { useParams, useSearchParams } from "next/navigation";

import { Page } from "@/components/enterprise/shell";
import { Button } from "@/components/ui/button";
import { currentProject, ent, projectPath } from "@/lib/enterprise-api";

type Finding = {
  id: number;
  title: string;
  summary: string;
  project_id?: string;
  case_id: string;
  task_id?: number;
  task_name?: string;
  report_id?: string;
  issued: boolean;
  can_compose: boolean;
  original?: { id: string; filename: string; created_at: string; notice: string; previewable: boolean };
};

function Detail() {
  const params = useParams<{ projectId?: string; findingId?: string }>();
  const search = useSearchParams();
  const id = params.findingId || search.get("id") || "";
  const [finding, setFinding] = useState<Finding | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const project = params.projectId || finding?.project_id || currentProject();

  useEffect(() => {
    if (!id) return;
    void ent<Finding>(`/findings/${id}`)
      .then(setFinding)
      .catch((reason: Error) => setError(reason.message));
  }, [id]);

  async function writeReport() {
    setBusy(true);
    try {
      const report = await ent<{ id: string }>(`/findings/${id}/report`, { method: "POST" });
      const target = project
        ? projectPath(project, `reports/${report.id}`)
        : `/enterprise/reports/detail?id=${report.id}`;
      window.location.href = target;
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "没有打开报告");
      setBusy(false);
    }
  }

  return (
    <Page title={finding?.title || "发现详情"}>
      <div className="max-w-3xl space-y-4">
        {error && <p className="text-sm">{error}</p>}
        {finding && (
          <>
            <p className="text-sm">{finding.summary}</p>
            <p className="text-sm text-muted-foreground">归并组：{finding.case_id ? finding.case_id : "无"}。</p>
            {finding.task_name && <p className="text-sm">关联任务：{finding.task_name}</p>}
            {finding.original && (
              <div className="rounded-xl border p-4 text-sm">
                <p>原件文件名：{finding.original.filename}</p>
                <p>时间：{new Date(finding.original.created_at).toLocaleString()}</p>
                <p>{finding.original.notice}</p>
                <p>{finding.original.previewable ? "可以预览" : "现在不能预览画面。"}</p>
              </div>
            )}
            <div className="flex flex-wrap gap-2">
              {finding.report_id && (
                <Button asChild>
                  <Link
                    href={
                      project
                        ? projectPath(project, `reports/${finding.report_id}`)
                        : `/enterprise/reports/detail?id=${finding.report_id}`
                    }
                  >
                    {finding.issued ? "查看报告" : "打开报告"}
                  </Link>
                </Button>
              )}
              {!finding.report_id && finding.can_compose && (
                <Button disabled={busy} onClick={() => void writeReport()}>
                  编写报告
                </Button>
              )}
              {finding.task_id && project && (
                <Button asChild variant="outline">
                  <Link href={projectPath(project, `tasks/${finding.task_id}`)}>返回任务</Link>
                </Button>
              )}
              <Button asChild variant="outline">
                <Link href={project ? projectPath(project, "findings") : "/enterprise/findings"}>返回发现</Link>
              </Button>
            </div>
            {!finding.report_id && !finding.can_compose && (
              <p className="text-sm">
                这份发现还没有签发报告。编写和签发需要项目成员及相应职责。只读审核不能发起，请联系项目负责人。
              </p>
            )}
          </>
        )}
      </div>
    </Page>
  );
}

export default function FindingDetailPage() {
  return (
    <Suspense fallback={<p className="p-6 text-sm">正在打开发现…</p>}>
      <Detail />
    </Suspense>
  );
}
