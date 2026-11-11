import type { Metadata } from "next";
import { IBM_Plex_Mono, IBM_Plex_Sans } from "next/font/google";
import "./globals.css";

// THEME_SCRIPT runs in <head> before the first paint, so the page never
// flashes the wrong theme: the choice saved by setTheme (lib/theme.ts),
// otherwise the system's setting.
const THEME_SCRIPT = `try{var t=localStorage.getItem("theme");if(t!=="light"&&t!=="dark")t=matchMedia("(prefers-color-scheme: light)").matches?"light":"dark";document.documentElement.dataset.theme=t}catch(e){}`;

// Fonts are downloaded at build time and served by the app itself.
const sans = IBM_Plex_Sans({ subsets: ["latin"], weight: ["400", "500", "600"], variable: "--font-sans" });
const mono = IBM_Plex_Mono({ subsets: ["latin"], weight: ["400", "500"], variable: "--font-mono" });

export const metadata: Metadata = {
  title: "NeuroHalo",
  description: "Replay EEG recordings as live streams and explore them on a 3D brain.",
};

// RootLayout sets the fonts and the theme, and nothing else. The home page draws its own top
// bar; the live session page is a bare full-height canvas.
export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    // The script sets data-theme on <html> before React starts, so the attribute differs from the server's.
    <html lang="en" className={`${sans.variable} ${mono.variable}`} suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: THEME_SCRIPT }} />
      </head>
      <body>{children}</body>
    </html>
  );
}
