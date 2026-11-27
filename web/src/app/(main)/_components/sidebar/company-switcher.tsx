"use client";

import { useEffect, useState } from "react";

import { Building2Icon } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { api } from "@/lib/api";
import type { Company } from "@/lib/types";
import { useCompanyStore } from "@/stores/company/company-store";

const ALL = "all";

export function CompanySwitcher() {
  const { t } = useTranslation();
  const companyId = useCompanyStore((s) => s.companyId);
  const setCompany = useCompanyStore((s) => s.setCompany);
  const [companies, setCompanies] = useState<Company[]>([]);

  useEffect(() => {
    let live = true;
    api
      .companies()
      .then((rows) => {
        if (live) setCompanies(rows);
      })
      .catch(() => {
        if (live) setCompanies([]);
      });
    return () => {
      live = false;
    };
  }, []);

  return (
    <div className="px-2 pb-2">
      <Select
        value={companyId === null ? ALL : String(companyId)}
        onValueChange={(value) => {
          if (value === ALL) {
            setCompany(null);
            return;
          }
          const id = Number(value);
          setCompany(id, companies.find((item) => item.id === id)?.name ?? "");
        }}
      >
        <SelectTrigger className="h-8 w-full" aria-label={t("company.switcher.label")}>
          <Building2Icon className="size-3.5" />
          <SelectValue placeholder={t("company.switcher.all")} />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value={ALL}>{t("company.switcher.all")}</SelectItem>
          {companies.length === 0 ? (
            <SelectItem value="none" disabled>
              {t("company.switcher.empty")}
            </SelectItem>
          ) : (
            companies.map((company) => (
              <SelectItem key={company.id} value={String(company.id)}>
                {company.name}
              </SelectItem>
            ))
          )}
        </SelectContent>
      </Select>
    </div>
  );
}
