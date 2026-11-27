"use client";

import * as React from "react";

import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { useUITranslation } from "@/i18n/use-ui-translation";
import { api } from "@/lib/api";

const MAX_HAR_IMPORT_BYTES = 32 * 1024 * 1024;

export { TamperHooksPanel } from "@/components/tamper-hooks-panel";
export { ReplayPanel } from "@/components/traffic-replay-panel";
export { StreamMessagesPanel } from "@/components/traffic-stream-messages";

export function TrafficHarActions({
  query,
  allowImport = true,
  onImported,
}: {
  query: string;
  allowImport?: boolean;
  onImported: () => void;
}) {
  const { ui } = useUITranslation();
  const fileRef = React.useRef<HTMLInputElement>(null);
  const [busy, setBusy] = React.useState(false);

  return (
    <>
      {allowImport ? (
        <>
          <input
            ref={fileRef}
            type="file"
            accept="application/json,.har"
            className="hidden"
            onChange={(event) => {
              const file = event.target.files?.[0];
              event.target.value = "";
              if (!file) return;
              if (file.size > MAX_HAR_IMPORT_BYTES) {
                toast.error(ui("traffic.harFileTooLarge"));
                return;
              }
              setBusy(true);
              file
                .text()
                .then((raw) => api.trafficImportHAR(raw))
                .then((res) => {
                  if ((res.skipped ?? 0) > 0 || (res.failed ?? 0) > 0) {
                    toast.success(
                      ui("traffic.harImportSummary", {
                        imported: res.imported,
                        skipped: res.skipped ?? 0,
                        failed: res.failed ?? 0,
                      }),
                    );
                  } else {
                    toast.success(ui("traffic.harImported", { count: res.imported }));
                  }
                  onImported();
                })
                .catch((err: Error) => toast.error(err.message))
                .finally(() => setBusy(false));
            }}
          />
          <Button variant="outline" size="sm" className="h-8" disabled={busy} onClick={() => fileRef.current?.click()}>
            {ui("traffic.harImport")}
          </Button>
        </>
      ) : null}
      <Button
        variant="outline"
        size="sm"
        className="h-8"
        disabled={busy}
        onClick={() => {
          setBusy(true);
          api
            .trafficExportHAR(query)
            .then((meta) => {
              if (meta.truncated) {
                toast.warning(
                  ui("traffic.harExportTruncated", { exported: meta.exported, matched: meta.matched }),
                );
              } else if (meta.exported > 0) {
                toast.success(ui("traffic.harExportDone", { exported: meta.exported }));
              }
            })
            .catch((err: Error) => toast.error(err.message))
            .finally(() => setBusy(false));
        }}
      >
        {ui("traffic.harExport")}
      </Button>
    </>
  );
}
