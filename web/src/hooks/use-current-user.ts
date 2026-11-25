"use client";

import { createContext, useContext } from "react";

import type { CurrentUser } from "@/lib/auth";

export const fallbackCurrentUser: CurrentUser = { id: "1", name: "ARTEX", username: "artex", email: "", avatar: "", role: "operator" };
export const CurrentUserContext = createContext<CurrentUser>(fallbackCurrentUser);

export function useCurrentUser(): CurrentUser {
  return useContext(CurrentUserContext);
}
