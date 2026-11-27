"use client";

import { create } from "zustand";
import { persist } from "zustand/middleware";

// Sidebar company view. null means every company. This filter is independent of
// the enterprise project the workbench has selected.
type CompanyState = {
  companyId: number | null;
  companyName: string;
  setCompany: (id: number | null, name?: string) => void;
};

export const useCompanyStore = create<CompanyState>()(
  persist(
    (set) => ({
      companyId: null,
      companyName: "",
      setCompany: (id, name = "") => set({ companyId: id, companyName: id === null ? "" : name }),
    }),
    { name: "artex-active-company" },
  ),
);

export function useActiveCompany(): string | undefined {
  const companyId = useCompanyStore((s) => s.companyId);
  return companyId === null ? undefined : String(companyId);
}
