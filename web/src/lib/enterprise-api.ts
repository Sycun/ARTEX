import { auth } from "@/lib/auth";

export class EntError extends Error {
  status: number;
  data: Record<string, unknown>;

  constructor(status: number, data: Record<string, unknown>) {
    super(String(data.message || data.error || "这一步没有完成"));
    this.status = status;
    this.data = data;
  }
}

export async function ent<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers);
  const token = auth.getToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);
  if (init?.body && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");
  const response = await fetch(`/api/enterprise${path}`, { ...init, headers });
  const text = await response.text();
  const data = text ? (JSON.parse(text) as Record<string, unknown>) : {};
  if (!response.ok) throw new EntError(response.status, data);
  return data as T;
}

export type ProjectBrief = {
  id: string;
  code: string;
  name: string;
  role: string;
  duties: string[];
};

export type Receipt = {
  id: string;
  receipt_no: string;
  project_code: string;
  status: string;
  status_label: string;
  purpose?: string;
  credential_notice?: string;
  project_id?: string;
  project_name?: string;
  reused?: boolean;
  structured_reason?: string;
};

export type Me = {
  gate: "ready" | "pending_bind" | "disabled";
  name: string;
  user_id: string;
  platform: boolean;
  projects: ProjectBrief[];
  receipts: Receipt[];
  init_grant?: string;
  grant_ready?: boolean;
  contact_name: string;
};

const PROJECT_KEY = "artex_project";

export function currentProject(): string {
  if (typeof window === "undefined") return "";
  return localStorage.getItem(PROJECT_KEY) ?? "";
}

export function setProject(id: string) {
  localStorage.setItem(PROJECT_KEY, id);
}

export function clearProject() {
  localStorage.removeItem(PROJECT_KEY);
}

export function projectPath(projectId: string, suffix = "") {
  const base = `/enterprise/p/${projectId}`;
  return suffix ? `${base}/${suffix.replace(/^\//, "")}` : base;
}

export async function leaveEnterprise(next = "/login") {
  const token = auth.getToken();
  try {
    await fetch("/api/enterprise/logout", {
      method: "POST",
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    });
  } catch {
    // 退出请求失败时仍清掉本机会话，避免停在半登录状态。
  }
  auth.clearToken();
  clearProject();
  document.cookie = "artex_enterprise=; path=/; max-age=0";
  window.location.href = next;
}

export async function sha256(bytes: ArrayBuffer): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", bytes);
  return [...new Uint8Array(digest)].map((item) => item.toString(16).padStart(2, "0")).join("");
}
