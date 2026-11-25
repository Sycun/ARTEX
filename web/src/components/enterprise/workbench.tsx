"use client";

import { useEffect, useState } from "react";

import { auth } from "@/lib/auth";
import { ent } from "@/lib/enterprise-api";

export type Workbench = {
  active: boolean;
  name: string;
  post: string;
  post_label: string;
  platform: boolean;
  project_id?: string;
  project_name?: string;
  project_code?: string;
  can_edit: boolean;
  can_run: boolean;
  can_issue: boolean;
};

export function useWorkbench(): Workbench | null {
  const [bench, setBench] = useState<Workbench | null>(null);
  useEffect(() => {
    if (!auth.hasEnterpriseSession()) return;
    ent<Workbench>("/workbench")
      .then(setBench)
      .catch(() => setBench(null));
  }, []);
  return bench;
}

export function WorkbenchBanner({ bench }: { bench: Workbench | null }) {
  if (!bench?.active || !bench.post_label) return null;
  const project = bench.project_name || "尚未选择项目";
  return (
    <p className="mx-4 rounded-lg border bg-muted/40 px-3 py-2 text-sm text-muted-foreground lg:mx-6">
      当前项目 {project}
      {bench.project_code ? `（${bench.project_code}）` : ""} · 当前身份 {bench.name} · {bench.post_label}
      {bench.can_edit ? " · 可以保存本项目的任务资料、处理发现并送审报告" : " · 当前岗位不能修改或运行"}
    </p>
  );
}
