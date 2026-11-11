// The color scale for band strength: "inferno", a scale built for scientific
// figures. It runs dark purple -> red -> orange -> pale yellow, gets steadily
// lighter, and stays readable with color-blindness. Low values fade into the
// anatomy underneath, so only real activity is painted.

const INFERNO = [
  "#000004", "#160b39", "#420a68", "#6a176e", "#932667", "#bc3754",
  "#dd513a", "#f37819", "#fca50a", "#f6d746", "#fcffa4",
].map((hex) => [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255));

// The scale starts a little way in, because pure black reads as shadow on a brain.
const START = 0.16;

// toLinear converts one sRGB channel (0..1) to linear light, which is what
// three.js expects for vertex colors.
export function toLinear(c: number): number {
  return c <= 0.04045 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4);
}

// rampColor maps t in 0..1 to [r, g, b] in 0..1 (sRGB).
export function rampColor(t: number): [number, number, number] {
  const x = (START + (1 - START) * Math.min(1, Math.max(0, t))) * (INFERNO.length - 1);
  const i = Math.min(INFERNO.length - 2, Math.floor(x));
  const f = x - i;
  const a = INFERNO[i];
  const b = INFERNO[i + 1];
  return [a[0] + (b[0] - a[0]) * f, a[1] + (b[1] - a[1]) * f, a[2] + (b[2] - a[2]) * f];
}

// RAMP_LINEAR is the scale as 256 linear-light colors, for painting surfaces fast.
export const RAMP_LINEAR = (() => {
  const out = new Float32Array(256 * 3);
  for (let i = 0; i < 256; i++) out.set(rampColor(i / 255).map(toLinear), i * 3);
  return out;
})();

// RAMP_CSS is the scale as a CSS gradient, for the legend.
export const RAMP_CSS = `linear-gradient(to right, ${Array.from({ length: 11 }, (_, i) => {
  const [r, g, b] = rampColor(i / 10).map((c) => Math.round(c * 255));
  return `rgb(${r} ${g} ${b})`;
}).join(", ")})`;

// NO_DATA is the color of a sensor with no value yet (sRGB).
export const NO_DATA: [number, number, number] = [0.42, 0.42, 0.4];
