import { createContext, useContext, useEffect, useState, type ReactNode } from "react";

export interface Crumb {
  label: string;
  to?: string;
}

const Ctx = createContext<{ crumbs: Crumb[]; setCrumbs: (c: Crumb[]) => void }>({
  crumbs: [],
  setCrumbs: () => {},
});

export function PageMetaProvider({ children }: { children: ReactNode }) {
  const [crumbs, setCrumbs] = useState<Crumb[]>([]);
  return <Ctx.Provider value={{ crumbs, setCrumbs }}>{children}</Ctx.Provider>;
}

export function useCrumbs() {
  return useContext(Ctx).crumbs;
}

/** Sets breadcrumbs and the document title for the current page. */
export function usePageMeta(crumbs: Crumb[]) {
  const { setCrumbs } = useContext(Ctx);
  const key = crumbs.map((c) => `${c.label}|${c.to ?? ""}`).join("/");
  useEffect(() => {
    setCrumbs(crumbs);
    const last = crumbs[crumbs.length - 1]?.label;
    document.title = last ? `${last} · TaskFlow` : "TaskFlow";
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, setCrumbs]);
}
