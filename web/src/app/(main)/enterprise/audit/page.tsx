"use client";

import { useEffect, useState } from "react";

import Link from "next/link";
import { useParams } from "next/navigation";

import { Page } from "@/components/enterprise/shell";
import { Button } from "@/components/ui/button";
import { auth } from "@/lib/auth";
import { currentProject, ent, type Me, projectPath, sha256 } from "@/lib/enterprise-api";

type Event = {
  id: string;
  action: string;
  structured_reason: string;
  summary: string;
  actor: string;
  version_no?: number;
  content_hash?: string;
  created_at: string;
  note?: string;
  subject_id: string;
  report_id?: string;
};

export default function AuditPage() {
  const params = useParams<{ projectId?: string }>();
  const project = params.projectId || currentProject();
  const [allowed, setAllowed] = useState<boolean | null>(null);
  const [items, setItems] = useState<Event[] | null>(null);
  const [notice, setNotice] = useState("");
  const [history, setHistory] = useState("");
  const [hash, setHash] = useState("");
  const [error, setError] = useState("");
  const [reportID, setReportID] = useState("");
  const [versionID, setVersionID] = useState("");

  useEffect(() => {
    if (!project) {
      setError("请先进入项目。");
      setAllowed(false);
      return;
    }
    void ent<Me>("/me")
      .then((me) => {
        const current = me.projects.find((item) => item.id === project);
        const can = Boolean(current && (current.role === "owner" || current.duties.includes("audit")));
        setAllowed(can);
        if (!can) return;
        return ent<{ items: Event[]; no_issued_notice?: string; history_note?: string }>(
          `/projects/${project}/audit`,
        ).then((data) => {
          setItems(data.items);
          setNotice(data.no_issued_notice ?? "");
          setHistory(data.history_note ?? "");
          const issued = data.items.find((item) => item.action === "report_issued" && item.report_id);
          if (issued?.subject_id && issued.report_id) {
            setReportID(issued.report_id);
            setVersionID(issued.subject_id);
          }
        });
      })
      .catch((reason: Error) => setError(reason.message));
  }, [project]);

  async function download() {
    if (!reportID || !versionID) {
      setError("尚无签发版，现在没有可下载的文件。");
      return;
    }
    const token = auth.getToken();
    const response = await fetch(`/api/enterprise/reports/${reportID}/versions/${versionID}/download`, {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    });
    if (!response.ok) {
      setError("找不到可下载的签发文件");
      return;
    }
    const bytes = await response.arrayBuffer();
    setHash(await sha256(bytes));
    const link = document.createElement("a");
    link.href = URL.createObjectURL(new Blob([bytes], { type: "text/markdown" }));
    link.download = "脱敏报告.md";
    link.click();
  }

  async function exportAudit() {
    const data = await ent<{ items: Event[] }>(`/projects/${project}/audit/export`);
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: "application/json" });
    const link = document.createElement("a");
    link.href = URL.createObjectURL(blob);
    link.download = "项目审计.json";
    link.click();
  }

  return (
    <Page title="项目审计">
      {allowed === null && !error && <p className="text-sm text-muted-foreground">正在确认权限…</p>}
      {allowed === false && !error && (
        <div className="max-w-xl space-y-2 text-sm">
          <p>查看项目审计需要审计职责。只读审核没有这项职责，也不能从这里下载。</p>
          <p className="text-muted-foreground">
            请项目负责人在「成员与职责」里授予审计。已签发报告仍可从任务或报告页下载。
          </p>
          {project && (
            <Button asChild variant="outline" size="sm">
              <Link href={projectPath(project, "reports")}>打开报告</Link>
            </Button>
          )}
        </div>
      )}
      {error && <p className="text-sm">{error}</p>}
      {allowed && notice && <p className="text-sm font-medium">{notice}</p>}
      {allowed && history && <p className="max-w-3xl text-sm text-muted-foreground">{history}</p>}
      {allowed && (
        <div className="flex gap-2">
          {versionID && reportID ? (
            <Button onClick={() => void download()}>下载脱敏版</Button>
          ) : (
            <p className="text-sm">尚无签发版，现在没有可下载的文件。</p>
          )}
          <Button variant="outline" onClick={() => void exportAudit()}>
            导出审计摘要
          </Button>
        </div>
      )}
      {hash && <p className="font-mono text-xs">本次下载 SHA-256：{hash}</p>}
      <div className="max-w-3xl space-y-3">
        {items?.map((item) => (
          <article key={item.id} className="space-y-1 rounded-xl border p-4 text-sm">
            <p className="font-medium">{item.structured_reason || item.summary}</p>
            <p>操作者：{item.actor}</p>
            {item.version_no ? <p>版本：v{item.version_no}</p> : null}
            {item.content_hash ? <p className="font-mono text-xs">哈希：{item.content_hash}</p> : null}
            <p className="text-muted-foreground">{new Date(item.created_at).toLocaleString()}</p>
            {item.note && <p>{item.note}</p>}
            <p className="text-muted-foreground">从这里不能打开截图或原件。</p>
          </article>
        ))}
      </div>
    </Page>
  );
}
