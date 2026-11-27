"use client";

import { Suspense, useEffect, useState } from "react";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";

import { Page } from "@/components/enterprise/shell";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { ent, projectPath, useEntParams } from "@/lib/enterprise-api";

type Task = {
  id: number;
  name: string;
  status_label: string;
  completed_at?: string;
  findings: number;
  sample_label?: string;
};

type List = { items: Task[]; total: number; page: number; page_size: number; project_name: string };

const STATUSES = [
  { id: "", label: "全部状态" },
  { id: "done", label: "已完成" },
  { id: "created", label: "未启动" },
  { id: "running", label: "运行中" },
  { id: "paused", label: "已暂停" },
];

function TaskList() {
  const projectId = useEntParams<{ projectId: string }>().projectId;
  const search = useSearchParams();
  const router = useRouter();
  const q = search.get("q") ?? "";
  const status = search.get("status") ?? "";
  const page = Number(search.get("page") ?? "1") || 1;
  const [draft, setDraft] = useState(q);
  const [data, setData] = useState<List | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    setDraft(q);
  }, [q]);

  useEffect(() => {
    const params = new URLSearchParams();
    if (q) params.set("q", q);
    if (status) params.set("status", status);
    if (page > 1) params.set("page", String(page));
    const query = params.toString();
    void ent<List>(`/projects/${projectId}/tasks${query ? `?${query}` : ""}`)
      .then(setData)
      .catch((reason: Error) => setError(reason.message));
  }, [projectId, q, status, page]);

  function apply(next: { q?: string; status?: string; page?: number }) {
    const params = new URLSearchParams();
    const text = next.q ?? q;
    const nextStatus = next.status ?? status;
    const nextPage = next.page ?? 1;
    if (text) params.set("q", text);
    if (nextStatus) params.set("status", nextStatus);
    if (nextPage > 1) params.set("page", String(nextPage));
    const query = params.toString();
    router.push(`${projectPath(projectId, "tasks")}${query ? `?${query}` : ""}`);
  }

  const total = data?.total ?? 0;
  const size = data?.page_size ?? 20;
  const pages = Math.max(1, Math.ceil(total / size));
  let intro = "正在读取任务…打开任务只查看记录，不会启动，也不会进入队列。";
  if (data?.project_name)
    intro = `${data.project_name}里可以查看的任务。打开任务只查看记录，不会启动，也不会进入队列。`;
  if (error) intro = "";

  return (
    <Page title="任务">
      <div className="max-w-4xl space-y-4">
        {intro && <p className="text-muted-foreground text-sm">{intro}</p>}
        <form
          className="flex flex-wrap items-center gap-2"
          onSubmit={(event) => {
            event.preventDefault();
            apply({ q: draft, page: 1 });
          }}
        >
          <Input
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            placeholder="搜索任务名称或说明"
            aria-label="搜索任务"
            className="max-w-xs"
          />
          <label className="text-sm" htmlFor="task-status">
            状态
          </label>
          <select
            id="task-status"
            className="h-9 rounded-md border bg-background px-2 text-sm"
            value={status}
            onChange={(event) => apply({ status: event.target.value, page: 1 })}
          >
            {STATUSES.map((item) => (
              <option key={item.id || "all"} value={item.id}>
                {item.label}
              </option>
            ))}
          </select>
          <Button type="submit">搜索</Button>
        </form>
        {error && (
          <div className="space-y-2 text-sm">
            <p>{error === "找不到" ? "找不到这个项目的任务。请从企业首页进入自己有资格的项目。" : error}</p>
            <Button asChild variant="outline" size="sm">
              <Link href="/enterprise">返回企业首页</Link>
            </Button>
          </div>
        )}
        {data && total === 0 && (
          <div className="space-y-2 rounded-xl border p-4 text-sm">
            <p>当前项目暂无{status ? STATUSES.find((item) => item.id === status)?.label : ""}任务。</p>
            <Button asChild variant="outline" size="sm">
              <Link href={projectPath(projectId)}>返回项目首页</Link>
            </Button>
          </div>
        )}
        {data && total > 0 && (
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="py-2 font-medium">任务</th>
                <th className="py-2 font-medium">状态</th>
                <th className="py-2 font-medium">发现</th>
                <th className="py-2 font-medium">完成时间</th>
              </tr>
            </thead>
            <tbody>
              {data.items.map((item) => (
                <tr key={item.id} className="border-b">
                  <td className="py-3">
                    <Link
                      className="font-medium underline-offset-4 hover:underline"
                      href={projectPath(projectId, `tasks/${item.id}`)}
                    >
                      {item.name}
                    </Link>
                    {item.sample_label && <p className="text-xs text-muted-foreground">{item.sample_label}</p>}
                  </td>
                  <td>{item.status_label}</td>
                  <td>{item.findings}</td>
                  <td>{item.completed_at ? new Date(item.completed_at).toLocaleString() : "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {data && (
          <div className="flex items-center gap-2 text-sm">
            <span>
              共 {total} 条，第 {data.page} / {pages} 页
            </span>
            <Button
              variant="outline"
              size="sm"
              disabled={data.page <= 1}
              onClick={() => apply({ page: data.page - 1 })}
            >
              上一页
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={data.page >= pages}
              onClick={() => apply({ page: data.page + 1 })}
            >
              下一页
            </Button>
          </div>
        )}
      </div>
    </Page>
  );
}

export default function TasksPage() {
  return (
    <Suspense fallback={<p className="p-6 text-sm">正在读取任务…</p>}>
      <TaskList />
    </Suspense>
  );
}
