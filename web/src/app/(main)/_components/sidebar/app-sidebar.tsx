"use client";

import { useEffect, useState } from "react";

import Link from "next/link";
import { usePathname } from "next/navigation";

import { useTranslation } from "react-i18next";
import { useShallow } from "zustand/react/shallow";

import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
} from "@/components/ui/sidebar";
import { APP_CONFIG } from "@/config/app-config";
import { useCurrentUser } from "@/hooks/use-current-user";
import { auth } from "@/lib/auth";
import { useWorkbench } from "@/components/enterprise/workbench";
import { clearProject, ent, type ProjectBrief, projectPath } from "@/lib/enterprise-api";
import { getSidebarItems, type NavGroup, type NavMainLinkItem } from "@/navigation/sidebar/sidebar-items";
import { usePreferencesStore } from "@/stores/preferences/preferences-provider";

import { NavMain } from "./nav-main";
import { NavUser } from "./nav-user";

export function AppSidebar({ ...props }: React.ComponentProps<typeof Sidebar>) {
  const { t } = useTranslation();
  const currentUser = useCurrentUser();
  const { sidebarVariant, sidebarCollapsible, isSynced } = usePreferencesStore(
    useShallow((s) => ({
      sidebarVariant: s.sidebarVariant,
      sidebarCollapsible: s.sidebarCollapsible,
      isSynced: s.isSynced,
    })),
  );

  const variant = isSynced ? sidebarVariant : props.variant;
  const collapsible = isSynced ? sidebarCollapsible : props.collapsible;
  const path = usePathname();
  const bench = useWorkbench();
  const { enterpriseOn, items: enterpriseNav } = useEnterpriseNav(bench?.post ?? "");
  const employeeBench = bench?.post === "employee" || (bench == null && path.startsWith("/function"));
  const home = employeeBench
    ? "/function/tasks"
    : bench?.post === "leader"
      ? "/enterprise/conclusions"
      : bench?.post === "reviewer"
        ? "/enterprise/todos"
        : bench?.post === "admin"
          ? "/enterprise/admin"
          : enterpriseOn
            ? "/enterprise"
            : "/function/tasks";
  const navItems = employeeBench ? getSidebarItems(t) : enterpriseOn ? (enterpriseNav ?? [{ id: 1, label: "企业", items: [] }]) : getSidebarItems(t);

  return (
    <Sidebar {...props} variant={variant} collapsible={collapsible}>
      <SidebarHeader>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton asChild>
              <Link prefetch={false} href={home}>
                {/* eslint-disable-next-line @next/next/no-img-element */}
                <img src="/logo.png" alt="ARTEX" width={40} height={40} className="shrink-0" />
                <span className="font-semibold text-base">{APP_CONFIG.name}</span>
              </Link>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>
      <SidebarContent>
        {bench?.active && (
          <p className="px-3 pb-1 text-xs text-muted-foreground">
            {bench.project_name ? `${bench.project_name} · ` : ""}
            {bench.name} · {bench.post_label}
          </p>
        )}
        <NavMain items={navItems} />
        {/* <NavDocuments items={data.documents} /> */}
        {/* <NavSecondary items={data.navSecondary} className="mt-auto" /> */}
      </SidebarContent>
      <SidebarFooter>
        <NavUser user={currentUser} />
      </SidebarFooter>
    </Sidebar>
  );
}

function useEnterpriseNav(post: string): { enterpriseOn: boolean; items: NavGroup[] | null } {
  const path = usePathname();
  const [enterpriseOn, setEnterpriseOn] = useState(false);
  const [items, setItems] = useState<NavGroup[] | null>(null);
  useEffect(() => {
    const token = auth.getToken();
    const on = Boolean(token) && document.cookie.includes("artex_enterprise=1");
    setEnterpriseOn(on);
    if (!on) {
      setItems(null);
      return;
    }
    void ent<{ gate: string; platform: boolean; name: string; projects: ProjectBrief[] }>("/me")
      .then((me) => {
        if (me.gate !== "ready") {
          setItems([{ id: 1, label: me.name || "企业", items: [] }]);
          return;
        }
        const routeId = path.match(/^\/enterprise\/p\/([^/]+)/)?.[1] ?? "";
        const member = me.projects.find((item) => item.id === routeId);
        if (routeId && !member) clearProject();
        const links: NavMainLinkItem[] = [{ id: "enterprise-home", title: "企业首页", url: "/enterprise" }];
        if (post === "leader") {
          links[0] = { id: "conclusions", title: "报告与结论", url: "/enterprise/conclusions" };
        }
        if (post === "reviewer") {
          links[0] = { id: "review-todos", title: "待复核", url: "/enterprise/todos" };
        }
        if (me.platform) links.push({ id: "enterprise-admin", title: "平台管理", url: "/enterprise/admin" });
        if (!member && me.projects.length === 0 && !me.platform) {
          links[0] = { id: "enterprise-home", title: "请求加入", url: "/enterprise" };
        }
        if (post === "leader" || post === "reviewer") {
          setItems([{ id: 1, label: post === "leader" ? "报告与结论" : "复核", items: links }]);
          return;
        }
        const groups: NavGroup[] = [{ id: 1, label: me.platform && !member ? "平台管理" : "企业", items: links }];
        if (member) {
          groups.push({
            id: 2,
            label: member.name,
            items: projectLinks(member),
          });
        }
        const others = me.projects.filter((item) => item.id !== member?.id);
        if (others.length > 0) {
          groups.push({
            id: 3,
            label: "切换项目",
            items: others.map((item) => ({
              id: `project-${item.id}`,
              title: item.name,
              url: projectPath(item.id),
            })),
          });
        }
        setItems(groups);
      })
      .catch(() =>
        setItems([{ id: 1, label: "企业", items: [{ id: "enterprise-home", title: "企业首页", url: "/enterprise" }] }]),
      );
  }, [path, post]);
  return { enterpriseOn: enterpriseOn || path.startsWith("/enterprise") || post === "leader" || post === "reviewer" || post === "admin", items };
}

function projectLinks(project: ProjectBrief): NavMainLinkItem[] {
  const links: NavMainLinkItem[] = [
    { id: "project-home", title: "项目首页", url: projectPath(project.id) },
    { id: "project-tasks", title: "任务", url: projectPath(project.id, "tasks") },
    { id: "project-findings", title: "发现", url: projectPath(project.id, "findings") },
    { id: "project-assets", title: "资产", url: projectPath(project.id, "assets") },
    { id: "project-reports", title: "报告", url: projectPath(project.id, "reports") },
    { id: "project-todos", title: "待办", url: projectPath(project.id, "todos") },
  ];
  if (project.role === "owner") {
    links.push({ id: "project-members", title: "成员与职责", url: projectPath(project.id, "members") });
  }
  if (project.role === "owner" || project.duties.includes("audit")) {
    links.push({ id: "project-audit", title: "项目审计", url: projectPath(project.id, "audit") });
  }
  return links;
}
