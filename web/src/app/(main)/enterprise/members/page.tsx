"use client";

import { useEffect, useState } from "react";

import Link from "next/link";
import { useParams } from "next/navigation";

import { Page } from "@/components/enterprise/shell";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { currentProject, ent, type Me, projectPath } from "@/lib/enterprise-api";

type Hit = { id: string; name: string; addable: boolean; reason?: string };
type Member = { id: string; name: string; role: string; duties: string[] };

const ROLES = [
  { id: "member", label: "成员" },
  { id: "reader", label: "只读" },
  { id: "owner", label: "负责人" },
  { id: "leader", label: "领导" },
];

export default function MembersPage() {
  const params = useParams<{ projectId?: string }>();
  const project = params.projectId || currentProject();
  const [owner, setOwner] = useState<boolean | null>(null);
  const [query, setQuery] = useState("");
  const [hits, setHits] = useState<Hit[]>([]);
  const [members, setMembers] = useState<Member[]>([]);
  const [role, setRole] = useState("member");
  const [duties, setDuties] = useState<string[]>([]);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");

  async function loadMembers() {
    const data = await ent<{ items: Member[] }>(`/projects/${project}/members`);
    setMembers(data.items);
  }
  // biome-ignore lint/correctness/useExhaustiveDependencies: reload uses the current project already listed
  useEffect(() => {
    if (!project) {
      setError("请先进入项目。");
      setOwner(false);
      return;
    }
    void ent<Me>("/me")
      .then((me) => {
        const current = me.projects.find((item) => item.id === project);
        const isOwner = current?.role === "owner";
        setOwner(Boolean(isOwner));
        if (!isOwner) return;
        return loadMembers();
      })
      .catch((reason: Error) => setError(reason.message));
  }, [project]);

  async function search() {
    setError("");
    try {
      const data = await ent<{ items: Hit[] }>(`/directory?project=${project}&q=${encodeURIComponent(query)}`);
      setHits(data.items);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "没有搜到");
    }
  }

  async function add(id: string) {
    setError("");
    try {
      const result = await ent<{ message: string }>(`/projects/${project}/members`, {
        method: "POST",
        body: JSON.stringify({ user_id: id, role, duties }),
      });
      setMessage(result.message);
      await loadMembers();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "没有加入");
    }
  }

  function toggle(duty: string) {
    setDuties((current) => (current.includes(duty) ? current.filter((item) => item !== duty) : [...current, duty]));
  }

  if (owner === false) {
    return (
      <Page title="成员与职责">
        <div className="max-w-xl space-y-2 text-sm">
          <p>管理成员和职责需要项目负责人。只读审核不能添加成员，也不能授予审批、复核或审计。</p>
          <p className="text-muted-foreground">如需变更，请联系项目负责人。你仍可以浏览任务、发现和已签发报告。</p>
          {project && (
            <Button asChild variant="outline" size="sm">
              <Link href={projectPath(project)}>返回项目首页</Link>
            </Button>
          )}
        </div>
      </Page>
    );
  }

  return (
    <Page title="成员与职责">
      {owner === null && <p className="text-sm text-muted-foreground">正在确认权限…</p>}
      {error && <p className="text-sm">{error}</p>}
      {message && <p className="text-sm">{message}</p>}
      {owner && (
        <div className="max-w-2xl space-y-3">
          {members.map((member) => (
            <p key={member.id} className="text-sm">
              {member.name} · {ROLES.find((item) => item.id === member.role)?.label ?? member.role}
              {member.duties.length
                ? ` · ${member.duties.map((duty) => ({ approve: "审批", review: "复核签发", audit: "审计", lead: "领导" })[duty] ?? duty).join("、")}`
                : ""}
            </p>
          ))}
        </div>
      )}
      {owner && (
        <div className="max-w-xl space-y-3 rounded-xl border p-4">
          <p className="text-sm text-muted-foreground">
            只能搜到已核验账号。未核验的人不在列表里。已停用的人不能添加。
          </p>
          <div className="flex gap-2">
            <Input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="按显示名搜索" />
            <Button variant="outline" onClick={() => void search()}>
              搜索
            </Button>
          </div>
          <div className="flex gap-2 text-sm">
            {ROLES.map((item) => (
              <Button
                key={item.id}
                size="sm"
                variant={role === item.id ? "default" : "outline"}
                onClick={() => setRole(item.id)}
              >
                {item.label}
              </Button>
            ))}
          </div>
          <div className="flex gap-2 text-sm">
            {[
              ["approve", "审批"],
              ["review", "复核签发"],
              ["audit", "审计"],
              ["lead", "领导"],
            ].map(([id, label]) => (
              <Button
                key={id}
                size="sm"
                variant={duties.includes(id) ? "default" : "outline"}
                onClick={() => toggle(id)}
              >
                {label}
              </Button>
            ))}
          </div>
          {hits.map((hit) => (
            <div key={hit.id} className="flex items-center justify-between gap-3 text-sm">
              <span>
                {hit.name}
                {hit.reason ? ` · ${hit.reason}` : ""}
              </span>
              <Button size="sm" disabled={!hit.addable} onClick={() => void add(hit.id)}>
                添加
              </Button>
            </div>
          ))}
        </div>
      )}
    </Page>
  );
}
