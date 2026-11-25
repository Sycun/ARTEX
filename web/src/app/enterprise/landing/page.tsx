"use client";

import { useEffect, useState } from "react";

import { auth } from "@/lib/auth";
import { ent, EntError } from "@/lib/enterprise-api";

export default function LandingPage() {
  const [error, setError] = useState("");
  useEffect(() => {
    auth.clearStoredToken();
    ent<{ href: string }>("/entry")
      .then((entry) => window.location.replace(entry.href))
      .catch((reason: Error) => {
        if (reason instanceof EntError && (reason.status === 401 || reason.status === 403)) {
          window.location.replace(reason.status === 401 ? "/login" : "/enterprise");
          return;
        }
        setError(reason.message);
      });
  }, []);
  if (error) return <p role="alert" className="p-8 text-sm text-destructive">{error}</p>;
  return <p className="p-8 text-sm text-muted-foreground">正在按岗位进入…</p>;
}
