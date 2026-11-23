import { useLayoutEffect, useRef, useState, type ReactNode } from "react";

// The date page's answer bar (M8.1 brief 5.2): fixed above the shell's phone Tabs, or above the bottom
// safe inset where there are none, and as wide as the reading column. Layout is eager and stays
// untouched, so the bar measures the Tabs nav instead of sharing its classes, and reserves its own
// height in the page (a spacer) and in the root's scroll padding, so the last content and a focused
// control are never under it.
export function StickyRsvpBar({ label, children }: { label: string; children: ReactNode }) {
  const bar = useRef<HTMLDivElement>(null);
  const [geometry, setGeometry] = useState({ above: 0, height: 0 });

  useLayoutEffect(() => {
    const tabs = document.querySelector<HTMLElement>('nav[aria-label="Tabs"]');

    const measure = () => {
      const above = tabs?.isConnected && getComputedStyle(tabs).display !== "none" ? tabs.getBoundingClientRect().height : 0;
      const height = bar.current?.getBoundingClientRect().height ?? 0;
      setGeometry((g) => (g.above === above && g.height === height ? g : { above, height }));
    };

    measure();
    // jsdom has no ResizeObserver; a browser re-measures on wrap, zoom, fonts and an Undo coming or going.
    const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(measure);

    if (bar.current) observer?.observe(bar.current);

    if (tabs) observer?.observe(tabs);
    window.addEventListener("resize", measure);

    return () => {
      observer?.disconnect();
      window.removeEventListener("resize", measure);
    };
  }, []);

  useLayoutEffect(() => {
    const root = document.documentElement;
    root.style.setProperty("--fold-date-bar", `${geometry.above + geometry.height}px`);
    root.classList.add("fold-date-bar");

    return () => {
      root.classList.remove("fold-date-bar");
      root.style.removeProperty("--fold-date-bar");
    };
  }, [geometry]);

  return <>
    <div aria-hidden="true" style={{ height: geometry.height }} />
    <div ref={bar} role="region" aria-label={label} className={`date-bar${geometry.above ? "" : " date-bar-inset"}`} style={{ bottom: geometry.above }}>
      <div className="date-bar-row">{children}</div>
    </div>
  </>;
}
