import { ConclusionDetailClientPage } from "./conclusion-detail-client";

export function generateStaticParams() {
  return [{ reportId: "_" }];
}

export default function ConclusionDetailPage() {
  return <ConclusionDetailClientPage />;
}
