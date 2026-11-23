"use client";

import { useParams } from "next/navigation";

import { setProject } from "@/lib/enterprise-api";

export default function ProjectLayout({ children }: { children: React.ReactNode }) {
  const params = useParams<{ projectId: string }>();
  if (params.projectId) setProject(params.projectId);
  return children;
}
