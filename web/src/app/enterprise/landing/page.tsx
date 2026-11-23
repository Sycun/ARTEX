"use client";

import { useEffect } from "react";

import { auth } from "@/lib/auth";

export default function LandingPage() {
  useEffect(() => {
    const token = document.cookie
      .split("; ")
      .find((item) => item.startsWith("artex_token="))
      ?.slice("artex_token=".length);
    if (!token) {
      window.location.replace("/login");
      return;
    }
    auth.setToken(decodeURIComponent(token));
    const headers = { Authorization: `Bearer ${decodeURIComponent(token)}` };
    fetch("/api/enterprise/entry", { headers })
      .then((response) => (response.ok ? response.json() : null))
      .then((entry: { href?: string } | null) => {
        window.location.replace(entry?.href || "/enterprise");
      })
      .catch(() => window.location.replace("/enterprise"));
  }, []);
  return <p className="p-8 text-sm text-muted-foreground">正在按岗位进入…</p>;
}
