"use client";

// The whole 3D scene for the live session page: a real brain inside a glass
// head, one dot per channel, a red ring around each bad sensor, and (when
// turned on) a line between each strongly correlated pair of sensors. If the
// brain model cannot be loaded, a matte grey head stands in for it.

import { Canvas, useFrame, useThree, type ThreeEvent } from "@react-three/fiber";
import { OrbitControls } from "@react-three/drei";
import { memo, useEffect, useLayoutEffect, useMemo, useRef, useState, type RefObject } from "react";
import * as THREE from "three";
import { brainBase, haloMaterial, makeGeometry } from "@/components/Head3D";
import type { LiveFrame, LiveLinks } from "@/lib/api";
import { loadHeadModel, type HeadModel } from "@/lib/model";
import { usePalette } from "@/lib/theme";
import positions from "@/positions.json";

// positions.json maps a channel name to x, y, z on a sphere of radius 1.
const POSITIONS = positions as unknown as Record<string, [number, number, number]>;

const HEAD_RADII = new THREE.Vector3(0.88, 1.0, 1.05); // stand-in head: narrower side to side than front to back
const DOT_RADIUS = 0.045;
const FULL_POWER = 0.6; // relative power drawn at full brightness
const QUALITY_OK = 1;
const HIGHLIGHT_SCALE = 1.7; // the highlighted dot's size
const CLICK_SLOP_PX = 4; // a pointer that moved further than this was a drag, not a click

// Every color comes from the theme (usePalette): the dot scale in the page's
// one accent hue, grey for "no data yet", red for bad sensors, and neutral
// tints for the brain and the glass, so the accent and red stay the only
// colors that mean something.

// The dot colors as three.js colors.
// Built by: HeadScene, from the palette. Used by: Sensors.
type DotColors = { low: THREE.Color; high: THREE.Color; noData: THREE.Color };

const LOOK_AT = new THREE.Vector3(0, -0.15, 0);

// Correlation links. What a remembered link touching a bad sensor does: stay
// visible in red ("red"), or disappear ("hidden").
export const BAD_LINK_MODE: "red" | "hidden" = "red";
const MAX_LINKS = 50; // the server sends at most 50; never draw more from one message
const MAX_LINES = MAX_LINKS * 3; // current, fading out and held red, all at once
const ARC_STEPS = 12; // each link is an arc of this many short segments
const ARC_LIFT = 0.12; // how far the middle of an arc rises, as a share of the distance from the centre
const LINK_FADE_PER_S = 4; // how fast a line's opacity follows its target
const BAD_LINK_OPACITY = 0.9;

const Z_AXIS = new THREE.Vector3(0, 0, 1);

// One sensor's place in the scene.
// Built by: placeSensors. Used by: Sensors, Links.
type Placed = { position: THREE.Vector3; outward: THREE.Vector3 };

// One line of the links layer: its sensors, the opacity it is heading for,
// the opacity it has now, and whether it is a red "do not trust" line.
// Kept by: Links (keyed "a-b"). Drawn by: Links' useFrame.
type Line = { a: number; b: number; target: number; shown: number; red: boolean };

// A clicked dot: which channel, and where on screen the click was.
// Sent by: Sensors (through onPick). Used by: the session page's popover.
export type DotPick = { channel: number; clientX: number; clientY: number };

// placeSensors puts each named channel on the head's surface: on the brain
// model's scalp when it is loaded, otherwise on the grey stand-in head using
// positions.json. A channel with no known place goes in a row under the head.
function placeSensors(names: string[], model: HeadModel | null): Placed[] {
  const perRow = 16;
  const known = (name: string) => (model ? !!model.electrodes[name] : !!POSITIONS[name]);
  const unknownTotal = names.filter((name) => !known(name)).length;
  const below = model ? -1.75 : -1.35;
  let unknown = 0;
  return names.map((name) => {
    const e = model?.electrodes[name];
    if (e) {
      const outward = new THREE.Vector3(...e.n).normalize();
      return { position: new THREE.Vector3(...e.p).addScaledVector(outward, 0.01), outward }; // just above the skin
    }
    const p = model ? undefined : POSITIONS[name];
    if (p) {
      const outward = new THREE.Vector3(...p).normalize();
      // Distance from the centre to the stretched sphere's surface along this direction.
      const r = 1 / Math.hypot(outward.x / HEAD_RADII.x, outward.y / HEAD_RADII.y, outward.z / HEAD_RADII.z);
      return { position: outward.clone().multiplyScalar(r * 1.02), outward }; // 1.02: just above the surface
    }
    const row = Math.floor(unknown / perRow);
    const column = unknown % perRow;
    const inThisRow = Math.min(perRow, unknownTotal - row * perRow);
    unknown++;
    return {
      position: new THREE.Vector3((column - (inThisRow - 1) / 2) * 0.14, below - row * 0.14, 0),
      outward: new THREE.Vector3(0, 0, -1),
    };
  });
}

// Sensors draws every dot as one InstancedMesh and every ring as a second
// one. Colors are updated inside useFrame from frameRef, so new data never
// re-renders React. After a theme change the dots ease to the new colors
// like they ease to a new frame.
function Sensors({ placed, frameRef, highlightRef, onPick, colors, ringColor }: {
  placed: Placed[];
  frameRef: RefObject<LiveFrame | null>;
  highlightRef: RefObject<number | null>;
  onPick: (pick: DotPick) => void;
  colors: DotColors;
  ringColor: string;
}) {
  const count = placed.length;
  const colorsRef = useRef(colors); // read inside useFrame, so a theme change does not lay the dots out again
  colorsRef.current = colors;
  const dots = useRef<THREE.InstancedMesh>(null);
  const rings = useRef<THREE.InstancedMesh>(null);
  const shown = useRef(new Float32Array(0)); // the color each dot has on screen right now: r, g, b per dot
  const ringsDrawnFor = useRef<LiveFrame | null>(null); // the frame the rings were last laid out for
  const grown = useRef<number | null>(null); // the dot drawn larger right now
  const scratch = useMemo(() => new THREE.Object3D(), []);
  const color = useMemo(() => new THREE.Color(), []);

  // Dot positions never change. Rings start hidden (scale 0).
  useLayoutEffect(() => {
    const d = dots.current;
    const r = rings.current;
    if (!d || !r) return;
    shown.current = new Float32Array(count * 3);
    const NO_DATA = colorsRef.current.noData;
    placed.forEach((sensor, i) => {
      scratch.position.copy(sensor.position);
      scratch.quaternion.identity();
      scratch.scale.setScalar(1);
      scratch.updateMatrix();
      d.setMatrixAt(i, scratch.matrix);
      scratch.scale.setScalar(0);
      scratch.updateMatrix();
      r.setMatrixAt(i, scratch.matrix);
      // Instance colors must exist before the first draw, or the material compiles without them.
      NO_DATA.toArray(shown.current, i * 3);
      d.setColorAt(i, NO_DATA);
    });
    d.instanceMatrix.needsUpdate = true;
    r.instanceMatrix.needsUpdate = true;
    if (d.instanceColor) d.instanceColor.needsUpdate = true;
    d.computeBoundingSphere(); // clicks are tested against this first
    ringsDrawnFor.current = null;
    grown.current = null;
  }, [placed, count, scratch]);

  useFrame((_, delta) => {
    const d = dots.current;
    const r = rings.current;
    if (!d || !r || shown.current.length !== count * 3) return;
    const frame = frameRef.current;

    // Frames arrive every 0.5 s. Each animation frame, move every dot part of
    // the way to its target color, which blends one frame into the next.
    // The exponential makes the speed the same at any screen refresh rate.
    const step = 1 - Math.exp(-delta * 6);
    const now = shown.current;
    const { low, high, noData } = colorsRef.current;
    for (let i = 0; i < count; i++) {
      if (frame) color.lerpColors(low, high, Math.min(1, Math.max(0, frame.power[i] / FULL_POWER)));
      else color.copy(noData);
      const o = i * 3;
      now[o] += (color.r - now[o]) * step;
      now[o + 1] += (color.g - now[o + 1]) * step;
      now[o + 2] += (color.b - now[o + 2]) * step;
      d.setColorAt(i, color.setRGB(now[o], now[o + 1], now[o + 2]));
    }
    if (d.instanceColor) d.instanceColor.needsUpdate = true;

    // The highlighted sensor (open popover or picked event) is drawn larger; no extra color.
    const want = highlightRef.current;
    if (want !== grown.current) {
      for (const [i, scale] of [[grown.current, 1], [want, HIGHLIGHT_SCALE]] as const) {
        if (i == null || i >= count) continue;
        scratch.position.copy(placed[i].position);
        scratch.quaternion.identity();
        scratch.scale.setScalar(scale);
        scratch.updateMatrix();
        d.setMatrixAt(i, scratch.matrix);
      }
      d.instanceMatrix.needsUpdate = true;
      grown.current = want;
    }

    // Rings only change when a new frame arrives, so lay them out once per frame from the server.
    if (frame && frame !== ringsDrawnFor.current) {
      ringsDrawnFor.current = frame;
      placed.forEach((sensor, i) => {
        scratch.position.copy(sensor.position);
        scratch.quaternion.setFromUnitVectors(Z_AXIS, sensor.outward); // a torus lies flat in x-y; turn it to face outward
        scratch.scale.setScalar(frame.quality[i] !== QUALITY_OK ? 1 : 0);
        scratch.updateMatrix();
        r.setMatrixAt(i, scratch.matrix);
      });
      r.instanceMatrix.needsUpdate = true;
    }
  });

  return (
    <>
      <instancedMesh
        key={`dots-${count}`} // an InstancedMesh cannot change its count, so make a new one
        ref={dots}
        args={[undefined, undefined, count]}
        onClick={(e: ThreeEvent<MouseEvent>) => {
          e.stopPropagation(); // do not let the click fall through to "clicked elsewhere"
          if (e.delta > CLICK_SLOP_PX || e.instanceId === undefined) return;
          onPick({ channel: e.instanceId, clientX: e.clientX, clientY: e.clientY });
        }}
      >
        <sphereGeometry args={[DOT_RADIUS, 20, 14]} />
        {/* unlit, so a dot's color is exactly the scale color; toneMapped off keeps it from being dulled */}
        <meshBasicMaterial toneMapped={false} />
      </instancedMesh>
      {/* raycast off: rings must not catch clicks meant for the dot inside them */}
      <instancedMesh key={`rings-${count}`} ref={rings} args={[undefined, undefined, count]} raycast={() => null}>
        <torusGeometry args={[DOT_RADIUS * 1.7, DOT_RADIUS * 0.34, 8, 32]} />
        <meshBasicMaterial color={ringColor} toneMapped={false} />
      </instancedMesh>
    </>
  );
}

// arcPoint writes the point at t (0..1) on the arc from p to q into out: the
// direction turns evenly from p to q, and the distance from the centre rises
// in the middle, so the line runs over the head instead of through the brain.
function arcPoint(p: THREE.Vector3, q: THREE.Vector3, t: number, out: THREE.Vector3, tmp: THREE.Vector3) {
  const lp = p.length();
  const lq = q.length();
  const angle = p.angleTo(q);
  const sin = Math.sin(angle);
  if (sin < 1e-4) out.copy(p).lerp(q, t).normalize();
  else {
    out.copy(p).multiplyScalar(Math.sin((1 - t) * angle) / (sin * lp))
      .add(tmp.copy(q).multiplyScalar(Math.sin(t * angle) / (sin * lq)))
      .normalize();
  }
  out.multiplyScalar((lp + (lq - lp) * t) * (1 + ARC_LIFT * Math.sin(Math.PI * t)));
}

// Links draws the correlation lines as one LineSegments geometry. All work
// happens in useFrame, reading linksRef and frameRef, so new data never
// re-renders React. A line's opacity eases toward its strength, which fades
// one update into the next. When a sensor's quality turns bad, the remembered
// lines touching it turn red (or hide, by BAD_LINK_MODE) and stay until the
// sensor recovers, even though the server no longer sends them.
function Links({ placed, frameRef, linksRef, lineColor, badColor }: {
  placed: Placed[];
  frameRef: RefObject<LiveFrame | null>;
  linksRef: RefObject<LiveLinks | null>;
  lineColor: string; // a trusted line
  badColor: string; // a line touching a bad sensor
}) {
  const lines = useRef(new Map<string, Line>()); // "a-b" -> line
  const remembered = useRef<[number, number, number][]>([]); // pairs of the most recent links message
  const bad = useRef(new Set<number>()); // sensors whose quality is not OK in the newest frame
  const seenLinks = useRef<LiveLinks | null>(null); // the links message last applied
  const seenFrame = useRef<LiveFrame | null>(null); // the frame last checked for bad sensors
  const segVertices = ARC_STEPS * 2;

  const { geometry, positionArray, colorArray } = useMemo(() => {
    const g = new THREE.BufferGeometry();
    const pos = new Float32Array(MAX_LINES * segVertices * 3);
    const col = new Float32Array(MAX_LINES * segVertices * 4); // r, g, b, opacity per vertex
    g.setAttribute("position", new THREE.BufferAttribute(pos, 3).setUsage(THREE.DynamicDrawUsage));
    g.setAttribute("color", new THREE.BufferAttribute(col, 4).setUsage(THREE.DynamicDrawUsage));
    g.setDrawRange(0, 0);
    return { geometry: g, positionArray: pos, colorArray: col };
  }, [segVertices]);
  useEffect(() => () => geometry.dispose(), [geometry]);

  // New sensor places (another session, or the brain model arriving) make every old line meaningless.
  useEffect(() => {
    lines.current.clear();
    remembered.current = [];
    bad.current = new Set();
    seenLinks.current = null;
    seenFrame.current = null;
  }, [placed]);

  const accent = useMemo(() => new THREE.Color(lineColor), [lineColor]);
  const red = useMemo(() => new THREE.Color(badColor), [badColor]);
  const from = useMemo(() => new THREE.Vector3(), []);
  const to = useMemo(() => new THREE.Vector3(), []);
  const tmp = useMemo(() => new THREE.Vector3(), []);

  useFrame((_, delta) => {
    const map = lines.current;
    const msg = linksRef.current;
    const frame = frameRef.current;

    // Links turned off: remove every line at once, held red ones included.
    if (!msg) {
      map.clear();
      remembered.current = [];
      seenLinks.current = null;
      geometry.setDrawRange(0, 0);
      return;
    }

    // A new frame: lines touching a sensor that is now bad turn red (or hide);
    // red lines whose sensors are all OK again are dropped.
    if (frame && frame !== seenFrame.current) {
      seenFrame.current = frame;
      const nowBad = new Set<number>();
      frame.quality.forEach((q, i) => { if (q !== QUALITY_OK) nowBad.add(i); });
      for (const [a, b] of remembered.current) {
        if (!nowBad.has(a) && !nowBad.has(b)) continue;
        const key = `${a}-${b}`;
        const line = map.get(key) ?? { a, b, target: 0, shown: 0, red: true };
        line.red = true;
        line.target = BAD_LINK_MODE === "red" ? BAD_LINK_OPACITY : 0;
        line.shown = line.target; // the switch to red (or hidden) is immediate, not faded
        map.set(key, line);
      }
      for (const [key, line] of map) {
        if (line.red && !nowBad.has(line.a) && !nowBad.has(line.b)) map.delete(key);
      }
      bad.current = nowBad;
    }

    // A new links message becomes the remembered set and sets the targets.
    if (msg !== seenLinks.current) {
      seenLinks.current = msg;
      const pairs = msg.pairs
        .slice(0, MAX_LINKS)
        .filter(([a, b]) => a < placed.length && b < placed.length);
      remembered.current = pairs;
      const fresh = new Set<string>();
      for (const [a, b, strength] of pairs) {
        if (bad.current.has(a) || bad.current.has(b)) continue; // never drawn as trusted
        const key = `${a}-${b}`;
        const line = map.get(key);
        if (line?.red) continue;
        fresh.add(key);
        if (line) line.target = strength;
        else map.set(key, { a, b, target: strength, shown: 0, red: false });
      }
      for (const [key, line] of map) if (!line.red && !fresh.has(key)) line.target = 0;
    }

    // Ease each line toward its target, drop faded-out ones, and fill the buffers.
    const step = 1 - Math.exp(-delta * LINK_FADE_PER_S);
    let n = 0;
    for (const [key, line] of map) {
      line.shown += (line.target - line.shown) * step;
      if (line.target === 0 && line.shown < 0.01) {
        map.delete(key);
        continue;
      }
      if (n >= MAX_LINES) continue;
      const c = line.red ? red : accent;
      const p = placed[line.a].position;
      const q = placed[line.b].position;
      for (let s = 0; s < ARC_STEPS; s++) {
        arcPoint(p, q, s / ARC_STEPS, from, tmp);
        arcPoint(p, q, (s + 1) / ARC_STEPS, to, tmp);
        const v = n * segVertices + s * 2;
        from.toArray(positionArray, v * 3);
        to.toArray(positionArray, (v + 1) * 3);
        for (let w = v; w <= v + 1; w++) {
          colorArray[w * 4] = c.r;
          colorArray[w * 4 + 1] = c.g;
          colorArray[w * 4 + 2] = c.b;
          colorArray[w * 4 + 3] = line.shown;
        }
      }
      n++;
    }
    geometry.setDrawRange(0, n * segVertices);
    geometry.attributes.position.needsUpdate = true;
    geometry.attributes.color.needsUpdate = true;
  });

  return (
    // raycast off: lines must not catch clicks meant for the dots
    <lineSegments geometry={geometry} frustumCulled={false} raycast={() => null} renderOrder={1}>
      <lineBasicMaterial vertexColors transparent depthWrite={false} toneMapped={false} />
    </lineSegments>
  );
}

// Brain draws the model's brain, matte and tinted, inside a glass head. On a
// dark page the glass edge glows; on a light page it is drawn dark instead.
function Brain({ model, tint, glass, glow }: { model: HeadModel; tint: string; glass: string; glow: boolean }) {
  const brain = useMemo(() => makeGeometry(model.cortex, brainBase(model.cortexShade)), [model]);
  const halo = useMemo(() => {
    const g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.BufferAttribute(model.scalp.positions, 3));
    g.setIndex(new THREE.BufferAttribute(model.scalp.indices, 1));
    g.computeVertexNormals();
    return { geometry: g, material: haloMaterial(glass, glow) };
  }, [model, glass, glow]);
  useEffect(() => () => {
    brain.dispose();
    halo.geometry.dispose();
    halo.material.dispose();
  }, [brain, halo]);

  return (
    <>
      <mesh geometry={brain} raycast={() => null}>
        <meshStandardMaterial vertexColors color={tint} roughness={0.85} metalness={0} />
      </mesh>
      <mesh geometry={halo.geometry} material={halo.material} renderOrder={2} raycast={() => null} />
    </>
  );
}

// FitToWidth shrinks its contents when the view is taller than it is wide (a
// phone held upright), so the head is not cut off at the sides. The camera's
// field of view is vertical, so a wide view needs no change.
function FitToWidth({ children }: { children: React.ReactNode }) {
  const aspect = useThree((state) => state.viewport.aspect); // width / height; changes only on resize
  return <group scale={Math.min(1, aspect)}>{children}</group>;
}

// HeadScene is the 3D scene: brain, sensors, links, lights and camera controls.
// It is wrapped in memo and takes only stable props, so the page's
// twice-a-second text updates never re-render it.
// Used by: the session page.
function HeadScene({ names, frameRef, linksRef, highlightRef, onPick, onMiss }: {
  names: string[]; // channel names, in the session's channel order
  frameRef: RefObject<LiveFrame | null>; // newest frame from useLiveSession
  linksRef: RefObject<LiveLinks | null>; // newest links message; null while links are off
  highlightRef: RefObject<number | null>; // the channel to draw larger, or null
  onPick: (pick: DotPick) => void; // a dot was clicked
  onMiss: () => void; // the canvas was clicked, but not on a dot
}) {
  const [autoRotate, setAutoRotate] = useState(true);
  // The brain model: loading, loaded, or failed (then the grey head stands in).
  const [model, setModel] = useState<HeadModel | "loading" | "failed">("loading");
  useEffect(() => {
    let alive = true;
    loadHeadModel()
      .then((m) => alive && setModel(m))
      .catch((e) => {
        console.error("brain model:", e);
        if (alive) setModel("failed");
      });
    return () => { alive = false; };
  }, []);
  const loaded = typeof model === "object" ? model : null;
  const placed = useMemo(() => placeSensors(names, loaded), [names, loaded]);
  // The theme's colors. They change only when the theme does.
  const palette = usePalette();
  const dotColors: DotColors = useMemo(() => ({
    low: new THREE.Color(palette.scaleLow),
    high: new THREE.Color(palette.scaleHigh),
    noData: new THREE.Color(palette.noData),
  }), [palette]);

  return (
    <Canvas camera={{ position: [0, 1.9, -4.4], fov: 38 }} dpr={[1, 2]} onPointerMissed={onMiss}>
      <hemisphereLight args={["#ffffff", "#3a3d45", 1.3]} />
      <directionalLight position={[2, 4, -3]} intensity={1.6} />
      <directionalLight position={[-3, 2, 4]} intensity={0.5} />
      {model !== "loading" && (
        <FitToWidth>
          {loaded ? (
            <Brain model={loaded} tint={palette.brain} glass={palette.glass} glow={!palette.light} />
          ) : (
            <mesh scale={HEAD_RADII}>
              <sphereGeometry args={[1, 64, 48]} />
              {/* roughness 1 and no metalness: matte, no highlight */}
              <meshStandardMaterial color={palette.head} roughness={1} metalness={0} />
            </mesh>
          )}
          <Sensors placed={placed} frameRef={frameRef} highlightRef={highlightRef} onPick={onPick}
            colors={dotColors} ringColor={palette.red} />
          <Links placed={placed} frameRef={frameRef} linksRef={linksRef}
            lineColor={palette.light ? palette.accent : palette.scaleHigh} badColor={palette.red} />
        </FitToWidth>
      )}
      <OrbitControls
        target={LOOK_AT}
        enablePan={false}
        minDistance={3}
        maxDistance={7}
        autoRotate={autoRotate}
        autoRotateSpeed={0.6}
        onStart={() => setAutoRotate(false)} // the first drag stops the auto-rotation for good
      />
    </Canvas>
  );
}

export default memo(HeadScene);
