import FindingDetailPage from "../../../../findings/detail/page";

export function generateStaticParams() {
  return [{ projectId: "_", findingId: "_" }];
}

export default function ProjectFindingDetailPage() {
  return <FindingDetailPage />;
}
