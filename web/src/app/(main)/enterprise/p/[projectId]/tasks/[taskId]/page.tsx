import { TaskDetailClientPage } from "./task-detail-client";

export function generateStaticParams() {
  return [{ projectId: "_", taskId: "_" }];
}

export default function TaskDetailPage() {
  return <TaskDetailClientPage />;
}
