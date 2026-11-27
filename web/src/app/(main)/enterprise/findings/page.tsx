"use client";

import { Suspense, useEffect, useState } from "react";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";

import { Page } from "@/components/enterprise/shell";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { currentProject, ent, projectPath, useEntParams } from "@/lib/enterprise-api";

type Finding = {
  id: number;
  title: string;
  summary: string;
  task_name?: string;
  issued: boolean;
};

type PageData = { items: Finding[]; total: number; page: number; page_size: number };

function Findings() {
  const params = useEntParams<{ projectId?: string }>();
  const project = params.projectId || currentProject();
  const search = useSearchParams();
  const router = useRouter();
  const q = search.get("q") ?? "";
  const page = Number(search.get("page") ?? "1") || 1;
  const [draft, setDraft] = useState(q);
  const [data, setData] = useState<PageData | null>(null);
  const [error, setError] = useState("");

  useEffect(() => setDraft(q), [q]);

  useEffect(() => {
    if (!project) {
      setError("还没有进入项目。请先从首页进入。");
      return;
    }
    const params = new URLSearchParams({ project, page: String(page), page_size: "20" });
    if (q) params.set("q", q);
    void ent<PageData>(`/findings?${params.toString()}`)
      .then(setData)
      .catch((reason: Error) => setError(reason.message));
  }, [project, q, page]);

  function apply(nextQ: string, nextPage: number) {
    const params = new URLSearchParams();
    if (nextQ) params.set("q", nextQ);
    if (nextPage > 1) params.set("page", String(nextPage));
    const query = params.toString();
    const href = project ? projectPath(project, "findings") : "/enterprise/findings";
    router.push(query ? `${href}?${query}` : href);
  }

  const total = data?.total ?? 0;
  const pages = Math.max(1, Math.ceil(total / (data?.page_size || 20)));

  return (
    <Page title="发现">
      <div className="max-w-3xl space-y-4">
        <form
          className="flex gap-2"
          onSubmit={(event) => {
            event.preventDefault();
            apply(draft, 1);
          }}
        >
          <Input
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            placeholder="搜索发现标题或摘要"
            aria-label="搜索发现"
          />
          <Button type="submit">搜索</Button>
        </form>
        {error && <p className="text-sm">{error}</p>}
        {data && total === 0 && (
          <div className="space-y-2 text-sm">
            <p>当前项目暂无{q ? "符合搜索的" : ""}发现。</p>
            {project && (
              <Button asChild variant="outline" size="sm">
                <Link href={projectPath(project)}>返回项目首页</Link>
              </Button>
            )}
          </div>
        )}
        <div className="space-y-3">
          {data?.items.map((item) => (
            <article key={item.id} className="space-y-2 rounded-xl border p-4">
              <h2 className="font-medium">{item.title}</h2>
              <p className="text-sm text-muted-foreground">{item.summary}</p>
              {item.task_name && <p className="text-sm">关联任务：{item.task_name}</p>}
              <Button asChild size="sm">
                <Link
                  href={
                    project ? projectPath(project, `findings/${item.id}`) : `/enterprise/findings/detail?id=${item.id}`
                  }
                >
                  打开详情
                </Link>
              </Button>
            </article>
          ))}
        </div>
        {data && total > 0 && (
          <div className="flex items-center gap-2 text-sm">
            <span>
              共 {total} 条，第 {data.page} / {pages} 页
            </span>
            <Button variant="outline" size="sm" disabled={data.page <= 1} onClick={() => apply(q, data.page - 1)}>
              上一页
            </Button>
            <Button variant="outline" size="sm" disabled={data.page >= pages} onClick={() => apply(q, data.page + 1)}>
              下一页
            </Button>
          </div>
        )}
      </div>
    </Page>
  );
}

export default function FindingsPage() {
  return (
    <Suspense fallback={<p className="p-6 text-sm">正在读取发现…</p>}>
      <Findings />
    </Suspense>
  );
}
