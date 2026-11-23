"use client";

import { useEffect, useState } from "react";

import { useParams } from "next/navigation";

import { Page } from "@/components/enterprise/shell";
import { currentProject, ent } from "@/lib/enterprise-api";

type Asset = { name: string; observation: string; observed_at: string };
type Summary = { findings: number };

export default function AssetsPage() {
  const params = useParams<{ projectId?: string }>();
  const project = params.projectId || currentProject();
  const [items, setItems] = useState<Asset[] | null>(null);
  const [findings, setFindings] = useState<number | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    if (!project) {
      setError("请先进入项目。");
      return;
    }
    void ent<{ items: Asset[] }>(`/projects/${project}/assets`)
      .then((data) => setItems(data.items))
      .catch((reason: Error) => setError(reason.message));
    void ent<Summary>(`/projects/${project}/summary`).then((item) => setFindings(item.findings));
  }, [project]);
  return (
    <Page title="资产">
      {error && <p className="text-sm">{error}</p>}
      {findings !== null && <p className="text-sm text-muted-foreground">这个项目的发现数是 {findings}。</p>}
      {items?.length === 0 && <p className="text-sm">当前项目暂无资产观察。</p>}
      <div className="max-w-2xl space-y-3">
        {items?.map((item) => (
          <article key={item.name} className="space-y-1 rounded-xl border p-4 text-sm">
            <h2 className="font-medium">{item.name}</h2>
            <p>{item.observation}</p>
            <p className="text-muted-foreground">{new Date(item.observed_at).toLocaleString()}</p>
          </article>
        ))}
      </div>
    </Page>
  );
}
