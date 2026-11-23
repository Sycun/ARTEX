"use client";

import { useEffect, useState } from "react";

import { Button } from "@/components/ui/button";
import { ent } from "@/lib/enterprise-api";

type Project = { id: string; code: string; name: string; post: string };
type Entry = { projects: Project[]; name: string };

export default function PickProjectPage() {
  const [entry, setEntry] = useState<Entry | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    ent<Entry>("/entry")
      .then(setEntry)
      .catch((reason: Error) => setError(reason.message));
  }, []);

  async function choose(id: string) {
    setError("");
    try {
      await ent("/session/project", { method: "POST", body: JSON.stringify({ project_id: id }) });
      window.location.assign("/function/tasks");
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "没有进入项目");
    }
  }

  return (
    <div className="mx-auto flex max-w-lg flex-1 flex-col gap-4 p-8">
      <h1 className="text-xl font-semibold">选择项目</h1>
      <p className="text-sm text-muted-foreground">
        {entry?.name ? `${entry.name}，` : ""}进入的是原来的任务工作台。选中的项目由服务端记在本次登录上。
      </p>
      {error && <p className="text-sm text-destructive">{error}</p>}
      <div className="space-y-2">
        {entry?.projects.map((project) => (
          <Button key={project.id} variant="outline" className="w-full justify-start" onClick={() => void choose(project.id)}>
            {project.name} · {project.code}
          </Button>
        ))}
      </div>
    </div>
  );
}
