"use client";

import { useEffect, useState } from "react";

import Link from "next/link";
import { useParams } from "next/navigation";

import { Page } from "@/components/enterprise/shell";
import { Button } from "@/components/ui/button";
import { ent, projectPath } from "@/lib/enterprise-api";

type Home = {
  name: string;
  code: string;
  role_label: string;
  tasks: number;
  findings: number;
  cases: number;
  issued: number;
  running: number;
  queued: number;
  notice: string;
  limit_note?: string;
};

export default function ProjectHomePage() {
  const projectId = useParams<{ projectId: string }>().projectId;
  const [home, setHome] = useState<Home | null>(null);
  const [error, setError] = useState("");
  const [platform, setPlatform] = useState(false);

  useEffect(() => {
    void ent<{ platform: boolean }>("/me")
      .then((me) => setPlatform(me.platform))
      .catch(() => setPlatform(false));
    void ent<Home>(`/projects/${projectId}/home`)
      .then(setHome)
      .catch((reason: Error) => setError(reason.message));
  }, [projectId]);

  if (error) {
    return (
      <Page title="项目">
        <div className="max-w-xl space-y-3 text-sm">
          <p>{platform ? "没有进入项目工作区。" : "找不到这个项目。"}</p>
          <p className="text-muted-foreground">
            {platform
              ? "平台管理员负责导入样例，不会因此获得项目里的任务、发现或报告。要审核项目，请退出后使用梁阅登录。"
              : "请从企业首页进入自己有资格的项目。"}
          </p>
          <Button asChild>
            <Link href="/enterprise">返回企业首页</Link>
          </Button>
        </div>
      </Page>
    );
  }
  if (!home) return <Page title="项目">正在打开项目…</Page>;

  const cards = [
    { title: "任务", count: home.tasks, href: projectPath(projectId, "tasks"), hint: "查看任务列表" },
    { title: "发现", count: home.findings, href: projectPath(projectId, "findings"), hint: "查看发现" },
    { title: "已签发报告", count: home.issued, href: projectPath(projectId, "reports"), hint: "查看报告" },
    { title: "资产", count: null, href: projectPath(projectId, "assets"), hint: "查看资产" },
  ];

  return (
    <Page title={home.name}>
      <div className="max-w-3xl space-y-4">
        <p className="text-sm text-muted-foreground">
          {home.name}（{home.code}）· 当前身份 {home.role_label}
        </p>
        <p className="text-sm">{home.notice}</p>
        <div className="grid gap-3 sm:grid-cols-2">
          {cards.map((card) => (
            <article key={card.title} className="space-y-3 rounded-xl border p-4">
              <div>
                <h2 className="font-medium">{card.title}</h2>
                {card.count !== null && <p className="text-2xl font-semibold">{card.count}</p>}
              </div>
              <Button asChild variant={card.title === "任务" ? "default" : "outline"}>
                <Link href={card.href}>{card.hint}</Link>
              </Button>
            </article>
          ))}
        </div>
        <p className="text-sm text-muted-foreground">
          归并组 {home.cases}。任务启动数 {home.running}，排队数 {home.queued}。
        </p>
        {home.limit_note && <p className="rounded-lg border bg-muted/40 px-3 py-2 text-sm">{home.limit_note}</p>}
      </div>
    </Page>
  );
}
