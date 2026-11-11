"use client";

// Light and dark theme. Every color lives in globals.css as a CSS variable,
// once per theme; <html data-theme="light|dark"> picks the set. CSS and SVG
// use the variables directly. The canvas and the 3D scene cannot, so they
// read the same variables through usePalette.

import { useMemo, useSyncExternalStore } from "react";

// The two themes.
// Returned by: useTheme. Used by: setTheme, ThemeToggle, usePalette.
export type Theme = "dark" | "light";

const STORAGE_KEY = "theme"; // the same key RootLayout's script reads

// currentTheme reads the theme from <html>.
function currentTheme(): Theme {
  return document.documentElement.dataset.theme === "light" ? "light" : "dark";
}

// subscribe calls onChange whenever <html data-theme> changes.
function subscribe(onChange: () => void): () => void {
  const observer = new MutationObserver(onChange);
  observer.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  return () => observer.disconnect();
}

// useTheme returns the theme in use and re-renders when it changes.
// Used by: ThemeToggle, usePalette.
export function useTheme(): Theme {
  return useSyncExternalStore(subscribe, currentTheme, () => "dark" as Theme);
}

// setTheme switches the theme and remembers the choice in this browser.
// Used by: ThemeToggle.
export function setTheme(theme: Theme) {
  document.documentElement.dataset.theme = theme;
  try {
    localStorage.setItem(STORAGE_KEY, theme);
  } catch { /* private browsing: the choice lasts until the page is closed */ }
}

// The colors the canvas and the 3D scene draw with, as CSS color strings.
// Built by: usePalette. Used by: HeadScene, Head3D, RawTraces.
export type Palette = {
  light: boolean; // the light theme is on
  accent: string; // lines and traces
  scaleLow: string; // a sensor dot at zero power
  scaleHigh: string; // a sensor dot at full power
  noData: string; // a sensor dot before the first frame
  red: string; // bad sensors
  grid: string; // hairlines
  text: string; // small labels
  brain: string; // tint over the brain's own shading
  glass: string; // the glass head's edge
  head: string; // the stand-in head when the brain model is missing
};

// usePalette reads the theme's colors from the CSS variables in globals.css.
// It only works in the browser, so only components that draw after mounting
// (canvas, WebGL) use it.
export function usePalette(): Palette {
  const theme = useTheme();
  return useMemo(() => {
    // On the server there is no page to read from; nothing is drawn there either.
    const css = typeof document === "undefined" ? null : getComputedStyle(document.documentElement);
    const read = (name: string) => css?.getPropertyValue(name).trim() || "#808080";
    return {
      light: theme === "light",
      accent: read("--accent"),
      scaleLow: read("--scale-low"),
      scaleHigh: read("--scale-high"),
      noData: read("--no-data"),
      red: read("--red"),
      grid: read("--line"),
      text: read("--dim"),
      brain: read("--brain"),
      glass: read("--glass"),
      head: read("--head"),
    };
  }, [theme]);
}
