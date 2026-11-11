// Brain regions and sides from 10-10 sensor names: "F3" is frontal and left,
// "C4" central and right, "Pz" parietal on the midline.

export const REGIONS = ["Frontal", "Central", "Temporal", "Parietal", "Occipital"] as const;
export type Region = (typeof REGIONS)[number];

// Which region each name prefix belongs to.
const PREFIX_REGION: Record<string, Region> = {
  FP: "Frontal", AF: "Frontal", F: "Frontal",
  FC: "Central", C: "Central", CP: "Central",
  FT: "Temporal", T: "Temporal", TP: "Temporal",
  P: "Parietal",
  PO: "Occipital", O: "Occipital", I: "Occipital",
};

// regionOf returns a sensor's region, or null for a name outside the 10-10 system.
// Used by: groupChannels.
export function regionOf(name: string): Region | null {
  const prefix = name.replace(/(\d+|z)$/i, "").toUpperCase();
  return PREFIX_REGION[prefix] ?? null;
}

// sideOf returns "left" for odd numbers, "right" for even ones, null for the
// midline (z) and unknown names.
// Used by: groupChannels.
export function sideOf(name: string): "left" | "right" | null {
  const m = name.match(/(\d+)$/);
  if (!m) return null;
  return Number(m[1]) % 2 === 1 ? "left" : "right";
}

// groupChannels lists the channel indexes in each region and on each side.
// Used by: the session page's chart lines.
export function groupChannels(names: string[]) {
  const regions = Object.fromEntries(REGIONS.map((r) => [r, [] as number[]])) as Record<Region, number[]>;
  const left: number[] = [];
  const right: number[] = [];
  names.forEach((name, i) => {
    const r = regionOf(name);
    if (r) regions[r].push(i);
    const s = sideOf(name);
    if (s === "left") left.push(i);
    if (s === "right") right.push(i);
  });
  return { regions, left, right };
}
