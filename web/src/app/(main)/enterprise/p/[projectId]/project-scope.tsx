"use client";

import { setProject, useEntParams } from "@/lib/enterprise-api";

export function ProjectScope({ children }: { children: React.ReactNode }) {
  const params = useEntParams<{ projectId?: string }>();
  if (params.projectId) setProject(params.projectId);
  return children;
}
