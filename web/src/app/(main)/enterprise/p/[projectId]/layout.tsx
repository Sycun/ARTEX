import { ProjectScope } from "./project-scope";

export function generateStaticParams() {
  return [{ projectId: "_" }];
}

export default function ProjectLayout({ children }: { children: React.ReactNode }) {
  return <ProjectScope>{children}</ProjectScope>;
}
