"use client";

import { useState } from "react";

import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { ent } from "@/lib/enterprise-api";

type ReportRef = { id: string; version_id: string; status: string; title: string };
type Version = { id: string; number: number; status: string; body?: string; body_hash?: string };
type Report = { id: string; title: string; versions: Version[]; focus_id: string; actions?: Record<string, boolean> };

export function FindingReport({ findingID, title }: { findingID: string; title: string }) {
  const [open, setOpen] = useState(false);
  const [report, setReport] = useState<Report | null>(null);
  const [body, setBody] = useState("");
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function load(id: string) {
    const view = await ent<Report>(`/reports/${id}`);
    setReport(view);
    const focus = view.versions.find((item) => item.id === view.focus_id) ?? view.versions.at(-1);
    setBody(focus?.status === "draft" ? (focus.body ?? "") : "");
    return view;
  }

  async function start() {
    setBusy(true);
    setError("");
    try {
      const ref = await ent<ReportRef>(`/findings/${findingID}/report`, { method: "POST" });
      await load(ref.id);
      setOpen(true);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "没有打开报告");
    } finally {
      setBusy(false);
    }
  }

  const focus = report?.versions.find((item) => item.id === report.focus_id) ?? report?.versions.at(-1);
  const draft = report?.versions.find((item) => item.status === "draft");

  async function nextVersion() {
    if (!report) return;
    setBusy(true);
    setError("");
    try {
      await ent(`/reports/${report.id}/supplement`, { method: "POST" });
      await load(report.id);
      setMessage("已打开下一版草稿。已经签发的文件没有改变。");
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "没有打开下一版");
    } finally {
      setBusy(false);
    }
  }

  async function save() {
    if (!report) return;
    setBusy(true);
    setError("");
    try {
      const saved = await ent<Version>(`/reports/${report.id}/draft`, {
        method: "PUT",
        body: JSON.stringify({ body }),
      });
      await load(report.id);
      setMessage(`草稿已保存。版本 ${saved.id}`);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "没有保存");
    } finally {
      setBusy(false);
    }
  }

  async function submit() {
    if (!report) return;
    setBusy(true);
    setError("");
    try {
      const saved = await ent<Version>(`/reports/${report.id}/draft`, {
        method: "PUT",
        body: JSON.stringify({ body }),
      });
      const result = await ent<{ message: string; version_id?: string }>(
        `/reports/${report.id}/versions/${saved.id}/submit`,
        { method: "POST", body: JSON.stringify({ body_hash: saved.body_hash }) },
      );
      await load(report.id);
      setMessage(`${result.message} 报告 ${report.id}，版本 ${saved.id}`);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "没有送审");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="mt-3 space-y-2">
      {!open && (
        <Button size="sm" variant="outline" disabled={busy} onClick={() => void start()}>
          从这条发现编写报告
        </Button>
      )}
      {open && report && (
        <div className="space-y-2 rounded-lg border p-3">
          <p className="text-sm font-medium">
            {report.title || title} · 报告 {report.id}
          </p>
          {focus && focus.status !== "draft" && (
            <p className="text-xs text-muted-foreground">
              当前最新状态是 v{focus.number} {focus.status === "issued" ? "已签发" : focus.status}
              {focus.status === "issued" ? "。签发文件已冻结，编写下一版不会改掉它。" : "。"}
            </p>
          )}
          {draft && (
            <>
              <p className="text-xs text-muted-foreground">正在编辑 v{draft.number} 草稿 {draft.id}</p>
              <Textarea value={body} onChange={(event) => setBody(event.target.value)} rows={5} />
              <div className="flex gap-2">
                <Button size="sm" variant="outline" disabled={busy} onClick={() => void save()}>
                  保存草稿
                </Button>
                <Button size="sm" disabled={busy || body.trim() === ""} onClick={() => void submit()}>
                  送审
                </Button>
              </div>
            </>
          )}
          {!draft && focus?.status === "issued" && (
            <Button size="sm" variant="outline" disabled={busy} onClick={() => void nextVersion()}>
              编写下一版
            </Button>
          )}
          {message && <p className="text-sm">{message}</p>}
        </div>
      )}
      {error && <p className="text-sm text-destructive">{error}</p>}
    </div>
  );
}
