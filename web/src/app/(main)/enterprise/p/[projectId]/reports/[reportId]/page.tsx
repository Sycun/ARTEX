import ReportDetailPage from "../../../../reports/detail/page";

export function generateStaticParams() {
  return [{ projectId: "_", reportId: "_" }];
}

export default function ProjectReportDetailPage() {
  return <ReportDetailPage />;
}
