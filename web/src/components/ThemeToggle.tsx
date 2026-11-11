"use client";

// The theme switch: a text button that names the theme it switches to.

import { setTheme, useTheme } from "@/lib/theme";

// ThemeToggle shows "Light" in the dark theme and "Dark" in the light one.
// Used by: the home page's top bar.
export default function ThemeToggle() {
  const theme = useTheme();
  const other = theme === "dark" ? "light" : "dark";
  return (
    <button type="button" className="text-button" onClick={() => setTheme(other)} suppressHydrationWarning>
      {other === "light" ? "Light" : "Dark"}
    </button>
  );
}
