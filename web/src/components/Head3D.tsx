"use client";

// The 3D view: a real brain inside a see-through head, or the head's skin on
// its own. Band strength is painted over the surface as a smooth heat map,
// spread out from the sensors. Small dots mark the sensors; a red ring marks
// a bad one.

import { Canvas, useFrame, useThree, type ThreeEvent } from "@react-three/fiber";
import { OrbitControls } from "@react-three/drei";
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import * as THREE from "three";
import { NO_DATA, RAMP_LINEAR, rampColor, toLinear } from "@/lib/color";
import { usePalette } from "@/lib/theme";
import { percent } from "@/lib/format";
import { buildWeights, loadHeadModel, type HeadModel, type Surface, type Weights } from "@/lib/model";

const LOOK_AT = new THREE.Vector3(0, -0.2, 0);
const Z_AXIS = new THREE.Vector3(0, 0, 1);

export type View = "angle" | "top" | "front" | "back" | "left" | "right";
export const VIEWS: View[] = ["angle", "top", "front", "back", "left", "right"];
// Camera positions. The nose points to -z, the subject's right is +x. The top
// view sits a hair behind the crown so the nose is at the top of the screen.
const CAMERA: Record<View, [number, number, number]> = {
  angle: [-3.4, 1.9, -3.6],
  top: [0, 5.4, 0.03],
  front: [0, 0.4, -5.6],
  back: [0, 0.4, 5.6],
  left: [-5.6, 0.4, 0],
  right: [5.6, 0.4, 0],
};

export type SurfaceKind = "brain" | "scalp";

// One sensor's place in the scene.
// Built by: layoutSensors. Used by: Scene, SensorNames, Head3D's tooltip.
type Placed = { pos: THREE.Vector3; normal: THREE.Vector3; known: boolean };

// Which sensor the pointer is over, and where the pointer is inside the viewer.
// Set by: Scene (through Head3D's onHover). Used by: Head3D's tooltip.
type Hover = { index: number; x: number; y: number };

// layoutSensors places each channel: known names on the skin where the
// standard layout puts them, unknown ones in rows below the chin.
function layoutSensors(names: string[], model: HeadModel): Placed[] {
  const unknownTotal = names.filter((n) => !model.electrodes[n]).length;
  const perRow = 16;
  let unknown = 0;
  return names.map((name) => {
    const e = model.electrodes[name];
    if (e) return { pos: new THREE.Vector3(...e.p), normal: new THREE.Vector3(...e.n).normalize(), known: true };
    const row = Math.floor(unknown / perRow);
    const col = unknown % perRow;
    const inRow = Math.min(perRow, unknownTotal - row * perRow);
    unknown++;
    return {
      pos: new THREE.Vector3((col - (inRow - 1) / 2) * 0.12, -1.75 - row * 0.12, -0.2),
      normal: new THREE.Vector3(0, 0, -1),
      known: false,
    };
  });
}

// makeGeometry turns a model surface into a three.js geometry with smooth
// normals and a color per vertex, starting at the base (unpainted) color.
export function makeGeometry(surface: Surface, base: Float32Array): THREE.BufferGeometry {
  const g = new THREE.BufferGeometry();
  g.setAttribute("position", new THREE.BufferAttribute(surface.positions, 3));
  g.setIndex(new THREE.BufferAttribute(surface.indices, 1));
  g.setAttribute("color", new THREE.BufferAttribute(base.slice(), 3));
  g.computeVertexNormals();
  return g;
}

// brainBase is the brain's own color per vertex: light on the ridges, darker
// in the folds, which is what makes it read as a brain.
export function brainBase(shade: Uint8Array): Float32Array {
  const ridge = [0.80, 0.76, 0.72].map(toLinear);
  const fold = [0.36, 0.33, 0.32].map(toLinear);
  const out = new Float32Array(shade.length * 3);
  for (let v = 0; v < shade.length; v++) {
    const t = shade[v] / 255;
    for (let c = 0; c < 3; c++) out[v * 3 + c] = fold[c] + (ridge[c] - fold[c]) * t;
  }
  return out;
}

// brainRelief is how much paint each brain vertex keeps: all of it on a
// ridge, about a third deep in a fold.
function brainRelief(shade: Uint8Array): Float32Array {
  const out = new Float32Array(shade.length);
  for (let v = 0; v < shade.length; v++) out[v] = 0.32 + 0.68 * (shade[v] / 255);
  return out;
}

// flatBase is one color for every vertex, used for the skin.
function flatBase(vertices: number, rgb: [number, number, number]): Float32Array {
  const lin = rgb.map(toLinear);
  const out = new Float32Array(vertices * 3);
  for (let v = 0; v < vertices; v++) out.set(lin, v * 3);
  return out;
}

// paint writes the heat map into a surface's vertex colors: each vertex takes
// the weighted average of its nearest sensors, and that value is blended over
// the base color. Weak values and vertices far from any sensor stay unpainted.
// `relief` (optional, one factor per vertex) darkens the paint inside folds,
// so the brain's shape stays visible under strong colors.
function paint(colors: Float32Array, base: Float32Array, w: Weights, values: Float32Array, scaleMax: number,
  relief: Float32Array | null) {
  const { k, index, weight, cover } = w;
  if (k === 0) {
    colors.set(base);
    return;
  }
  const inv = 1 / scaleMax;
  for (let v = 0, n = cover.length; v < n; v++) {
    let a = 0;
    const o = v * k;
    for (let j = 0; j < k; j++) a += weight[o + j] * values[index[o + j]];
    let t = a * inv;
    if (t > 1) t = 1;
    let s = (t - 0.08) / 0.37;
    s = s < 0 ? 0 : s > 1 ? 1 : s;
    const alpha = cover[v] * s * s * (3 - 2 * s) * 0.95;
    const li = ((t * 255) | 0) * 3;
    const b = v * 3;
    const r = relief ? relief[v] : 1;
    colors[b] = base[b] + (RAMP_LINEAR[li] * r - base[b]) * alpha;
    colors[b + 1] = base[b + 1] + (RAMP_LINEAR[li + 1] * r - base[b + 1]) * alpha;
    colors[b + 2] = base[b + 2] + (RAMP_LINEAR[li + 2] * r - base[b + 2]) * alpha;
  }
}

// haloMaterial draws the head as glass: nearly invisible face-on, glowing at
// the edges, so the brain inside stays in full view. Also used by HeadScene,
// with a neutral tint. With `glow` the edge adds light, which shows on a dark
// page; without it the edge is drawn in the tint itself, for a light page.
export function haloMaterial(tint = "#9db4cf", glow = true): THREE.ShaderMaterial {
  return new THREE.ShaderMaterial({
    transparent: true,
    depthWrite: false,
    blending: glow ? THREE.AdditiveBlending : THREE.NormalBlending,
    uniforms: { tint: { value: new THREE.Color(tint) } },
    vertexShader: `
      varying vec3 vNormal;
      varying vec3 vToEye;
      varying float vHeight;
      void main() {
        vec4 mv = modelViewMatrix * vec4(position, 1.0);
        vNormal = normalize(normalMatrix * normal);
        vToEye = normalize(-mv.xyz);
        vHeight = position.y;
        gl_Position = projectionMatrix * mv;
      }`,
    fragmentShader: `
      uniform vec3 tint;
      varying vec3 vNormal;
      varying vec3 vToEye;
      varying float vHeight;
      void main() {
        float edge = pow(1.0 - abs(dot(normalize(vNormal), normalize(vToEye))), 2.4);
        // The scan ends at the neck with a ragged cut; fade the glass out before it.
        float fade = smoothstep(-1.38, -0.78, vHeight);
        gl_FragColor = vec4(tint, (edge * 0.5 + 0.025) * fade);
      }`,
  });
}

// TextSprite draws a short text that always faces the camera. `lift` moves it
// up on screen from its anchor point, in units of its own height. With onTop
// it is drawn over everything, and the caller hides it when it faces away.
function TextSprite({ text, position, height = 0.07, color = "#e8e6e1", lift = 0, onTop = false, spriteRef }: {
  text: string;
  position: THREE.Vector3 | [number, number, number];
  height?: number;
  color?: string;
  lift?: number;
  onTop?: boolean;
  spriteRef?: (s: THREE.Sprite | null) => void;
}) {
  const { texture, aspect } = useMemo(() => {
    const fontPx = 44;
    const pad = 10;
    const font = `500 ${fontPx}px "IBM Plex Mono", ui-monospace, Consolas, monospace`;
    const canvas = document.createElement("canvas");
    let ctx = canvas.getContext("2d")!;
    ctx.font = font;
    canvas.width = Math.ceil(ctx.measureText(text).width) + pad * 2;
    canvas.height = fontPx + pad * 2;
    ctx = canvas.getContext("2d")!; // resizing the canvas resets its state
    ctx.font = font;
    ctx.fillStyle = "rgba(10, 11, 13, 0.78)";
    ctx.fillRect(0, 0, canvas.width, canvas.height);
    ctx.fillStyle = color;
    ctx.textAlign = "center";
    ctx.textBaseline = "middle";
    ctx.fillText(text, canvas.width / 2, canvas.height / 2 + 2);
    const texture = new THREE.CanvasTexture(canvas);
    texture.colorSpace = THREE.SRGBColorSpace;
    texture.anisotropy = 4;
    return { texture, aspect: canvas.width / canvas.height };
  }, [text, color]);
  useEffect(() => () => texture.dispose(), [texture]);

  return (
    <sprite
      ref={spriteRef}
      position={position}
      scale={[height * aspect, height, 1]}
      center={[0.5, 0.5 - lift]}
      renderOrder={onTop ? 10 : 0}
      raycast={() => null}
    >
      <spriteMaterial map={texture} transparent depthWrite={false} depthTest={!onTop} toneMapped={false} />
    </sprite>
  );
}

// SensorNames shows every sensor's name above its dot, hiding the ones on the
// far side of the head each frame.
function SensorNames({ names, placed }: { names: string[]; placed: Placed[] }) {
  const sprites = useRef<(THREE.Sprite | null)[]>([]);
  const toCamera = useMemo(() => new THREE.Vector3(), []);
  useFrame(({ camera }) => {
    placed.forEach((s, i) => {
      const sprite = sprites.current[i];
      if (!sprite) return;
      toCamera.copy(camera.position).sub(s.pos).normalize();
      sprite.visible = !s.known || s.normal.dot(toCamera) > 0.12;
    });
  });
  return (
    <>
      {placed.map((s, i) => (
        <TextSprite key={names[i]} text={names[i]} position={s.pos} height={0.062} lift={1.25} onTop
          spriteRef={(el) => { sprites.current[i] = el; }} />
      ))}
    </>
  );
}

// Scene draws the model and keeps its colors in step with the data. Sensor
// values ease toward their targets every animation frame, because frames
// arrive only twice a second; the surface and the dots are repainted from the
// eased values.
function Scene({
  model, placed, values, scaleMax, bad, selected, surface, showSensors, showSides, onHover, onSelect,
}: {
  model: HeadModel;
  showSides: boolean;
  placed: Placed[];
  values: (number | null)[] | null;
  scaleMax: number;
  bad: boolean[];
  selected: number | null;
  surface: SurfaceKind;
  showSensors: boolean;
  onHover: (index: number | null, clientX: number, clientY: number) => void;
  onSelect: (i: number) => void;
}) {
  const n = placed.length;
  const dots = useRef<THREE.InstancedMesh>(null);
  const hits = useRef<THREE.InstancedMesh>(null);
  const rings = useRef<THREE.InstancedMesh>(null);
  const eased = useRef(new Float32Array(0));
  const dirty = useRef(true);
  const latest = useRef({ values, scaleMax });
  latest.current = { values, scaleMax };
  const tmp = useMemo(() => new THREE.Object3D(), []);
  const color = useMemo(() => new THREE.Color(), []);
  const palette = usePalette();

  // The painted surface: the brain, or the skin.
  const painted = useMemo(() => {
    const src = surface === "brain" ? model.cortex : model.scalp;
    const base = surface === "brain"
      ? brainBase(model.cortexShade)
      : flatBase(src.positions.length / 3, [0.56, 0.53, 0.5]);
    const relief = surface === "brain" ? brainRelief(model.cortexShade) : null;
    return { surface: src, base, relief, geometry: makeGeometry(src, base) };
  }, [model, surface]);
  useEffect(() => () => painted.geometry.dispose(), [painted]);

  // How sensor values spread over that surface, for this session's sensors.
  const weights = useMemo(() => {
    const channel: number[] = [];
    placed.forEach((s, i) => { if (s.known) channel.push(i); });
    const xyz = new Float32Array(channel.length * 3);
    channel.forEach((c, i) => placed[c].pos.toArray(xyz, i * 3));
    // The brain lies 1 to 3 cm under the sensors; the skin lies right under them.
    return surface === "brain"
      ? buildWeights(painted.surface.positions, xyz, channel, 0.42, 0.85)
      : buildWeights(painted.surface.positions, xyz, channel, 0.28, 0.62);
  }, [painted, placed, surface]);

  // The glass head shown around the brain.
  const halo = useMemo(() => {
    const g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.BufferAttribute(model.scalp.positions, 3));
    g.setIndex(new THREE.BufferAttribute(model.scalp.indices, 1));
    g.computeVertexNormals();
    // Dark theme: a blue glow. Light theme: the theme's dark glass edge.
    return { geometry: g, material: palette.light ? haloMaterial(palette.glass, false) : haloMaterial() };
  }, [model, palette]);
  useEffect(() => () => {
    halo.geometry.dispose();
    halo.material.dispose();
  }, [halo]);

  useEffect(() => { dirty.current = true; }, [painted, weights, scaleMax, values == null, showSensors]);

  // Sensor positions, sizes and rings.
  useLayoutEffect(() => {
    if (eased.current.length !== n) eased.current = new Float32Array(n);
    const d = dots.current;
    const h = hits.current;
    const r = rings.current;
    if (!d || !h || !r) return;
    placed.forEach((s, i) => {
      tmp.position.copy(s.pos);
      tmp.quaternion.identity();
      tmp.scale.setScalar(i === selected ? 1.7 : 1);
      tmp.updateMatrix();
      d.setMatrixAt(i, tmp.matrix);
      tmp.scale.setScalar(1);
      tmp.updateMatrix();
      h.setMatrixAt(i, tmp.matrix);
      tmp.quaternion.setFromUnitVectors(Z_AXIS, s.normal);
      tmp.scale.setScalar(bad[i] ? 1 : 0);
      tmp.updateMatrix();
      r.setMatrixAt(i, tmp.matrix);
    });
    d.instanceMatrix.needsUpdate = true;
    h.instanceMatrix.needsUpdate = true;
    r.instanceMatrix.needsUpdate = true;
    h.computeBoundingSphere();
    dirty.current = true;
  }, [placed, bad, selected, n, tmp, showSensors]);

  useFrame((_, dt) => {
    const cur = eased.current;
    if (cur.length !== n) return;
    const { values: vals, scaleMax: max } = latest.current;
    const k = 1 - Math.exp(-dt * 6);
    let moved = false;
    for (let i = 0; i < n; i++) {
      const delta = (vals?.[i] ?? 0) - cur[i];
      if (Math.abs(delta) > 1e-4) {
        cur[i] += delta * k;
        moved = true;
      }
    }
    if (!moved && !dirty.current) return;
    dirty.current = false;

    const attr = painted.geometry.getAttribute("color") as THREE.BufferAttribute;
    paint(attr.array as Float32Array, painted.base, weights, cur, max, painted.relief);
    attr.needsUpdate = true;

    const d = dots.current;
    if (d) {
      for (let i = 0; i < n; i++) {
        const rgb = vals == null || !placed[i].known ? NO_DATA : rampColor(cur[i] / max);
        d.setColorAt(i, color.setRGB(rgb[0], rgb[1], rgb[2], THREE.SRGBColorSpace));
      }
      if (d.instanceColor) d.instanceColor.needsUpdate = true;
    }
  });

  return (
    <>
      <mesh geometry={painted.geometry}>
        <meshStandardMaterial vertexColors roughness={0.78} metalness={0} />
      </mesh>
      {surface === "brain" && <mesh geometry={halo.geometry} material={halo.material} renderOrder={2} raycast={() => null} />}

      {/* dots are small; a larger unseen copy catches the pointer */}
      <group visible={showSensors}>
        <instancedMesh key={`dots-${n}`} ref={dots} args={[undefined, undefined, n]} raycast={() => null}>
          <sphereGeometry args={[0.026, 16, 12]} />
          <meshBasicMaterial toneMapped={false} />
        </instancedMesh>
        <instancedMesh key={`rings-${n}`} ref={rings} args={[undefined, undefined, n]} raycast={() => null}>
          <torusGeometry args={[0.058, 0.012, 8, 32]} />
          <meshBasicMaterial color="#e5484d" toneMapped={false} />
        </instancedMesh>
        {selected != null && placed[selected] && (
          <mesh
            position={placed[selected].pos}
            quaternion={new THREE.Quaternion().setFromUnitVectors(Z_AXIS, placed[selected].normal)}
            raycast={() => null}
          >
            <torusGeometry args={[0.085, 0.008, 8, 40]} />
            <meshBasicMaterial color="#e8e6e1" toneMapped={false} />
          </mesh>
        )}
      </group>
      {showSensors && n > 0 && (
        <instancedMesh
          key={`hits-${n}`}
          ref={hits}
          args={[undefined, undefined, n]}
          onPointerMove={(e: ThreeEvent<PointerEvent>) => {
            e.stopPropagation();
            onHover(e.instanceId ?? null, e.clientX, e.clientY);
          }}
          onPointerOut={() => onHover(null, 0, 0)}
          onClick={(e: ThreeEvent<MouseEvent>) => {
            e.stopPropagation();
            if (e.instanceId !== undefined) onSelect(e.instanceId);
          }}
        >
          <sphereGeometry args={[0.055, 10, 8]} />
          <meshBasicMaterial transparent opacity={0} depthWrite={false} />
        </instancedMesh>
      )}
      {/* left and right, beside the ears, because a head seen from the front is mirrored */}
      {showSides && (
        <>
          <TextSprite text="L" position={[-1.08, -0.35, 0.1]} height={0.1} color="#a9a7a0" />
          <TextSprite text="R" position={[1.08, -0.35, 0.1]} height={0.1} color="#a9a7a0" />
        </>
      )}
    </>
  );
}

// CameraRig moves the camera when the view buttons are pressed.
function CameraRig({ view, nonce }: { view: View; nonce: number }) {
  const camera = useThree((s) => s.camera);
  const controls = useThree((s) => s.controls) as unknown as { target: THREE.Vector3; update: () => void } | null;
  useLayoutEffect(() => {
    camera.position.set(...CAMERA[view]);
    camera.lookAt(LOOK_AT);
    controls?.target.copy(LOOK_AT);
    controls?.update();
  }, [view, nonce, camera, controls]);
  return null;
}

// Head3D is the whole viewer: it loads the model, then shows the scene, the
// sensor names and a tooltip for the sensor under the pointer. With `ambient`
// it is a slowly turning display piece with no controls.
// Used by: the session page and the home page.
export default function Head3D({
  names, values, quality, scaleMax, bad, selected, onSelect, showLabels, showSensors = true,
  surface = "brain", view = "angle", viewNonce = 0, ambient = false,
}: {
  names: string[];
  values: (number | null)[] | null;
  quality?: number[] | null;
  scaleMax: number;
  bad: boolean[];
  selected: number | null;
  onSelect?: (i: number | null) => void;
  showLabels?: boolean;
  showSensors?: boolean;
  surface?: SurfaceKind;
  view?: View;
  viewNonce?: number;
  ambient?: boolean;
}) {
  const [model, setModel] = useState<HeadModel | null>(null);
  const [failed, setFailed] = useState<string | null>(null);
  useEffect(() => {
    let alive = true;
    loadHeadModel().then((m) => alive && setModel(m)).catch((e) => alive && setFailed(String(e.message ?? e)));
    return () => { alive = false; };
  }, []);

  const placed = useMemo(() => (model ? layoutSensors(names, model) : []), [names, model]);
  const wrap = useRef<HTMLDivElement>(null);
  const [hover, setHover] = useState<Hover | null>(null);
  const qualityName = (q: number | undefined) => (q === 2 ? "flat" : q === 3 ? "noisy" : q === 1 ? "ok" : "");

  const onHover = (index: number | null, clientX: number, clientY: number) => {
    const rect = wrap.current?.getBoundingClientRect();
    setHover(index == null || !rect ? null : { index, x: clientX - rect.left, y: clientY - rect.top });
  };

  return (
    <div ref={wrap} className="viewer" style={{ cursor: ambient ? "default" : hover ? "pointer" : "grab" }}>
      <Canvas camera={{ position: CAMERA[view], fov: 34 }} dpr={[1, 2]}>
        <hemisphereLight args={["#ffffff", "#3a3d45", 1.5]} />
        <directionalLight position={[-3, 5, -4]} intensity={1.7} />
        <directionalLight position={[4, 2, 5]} intensity={0.6} />
        {model && (
          <Scene
            model={model}
            placed={placed}
            values={values}
            scaleMax={scaleMax}
            bad={bad}
            selected={selected}
            surface={surface}
            showSensors={showSensors && !ambient}
            showSides={!ambient}
            onHover={onHover}
            onSelect={(i) => onSelect?.(i === selected ? null : i)}
          />
        )}
        {model && showLabels && showSensors && !ambient && <SensorNames names={names} placed={placed} />}
        <OrbitControls
          makeDefault
          enablePan={false}
          enableZoom={!ambient}
          autoRotate={ambient}
          autoRotateSpeed={0.7}
          minDistance={2.6}
          maxDistance={10}
          target={LOOK_AT}
        />
        <CameraRig view={view} nonce={viewNonce} />
      </Canvas>
      {!model && <div className="viewer-note">{failed ? `Cannot load the head model: ${failed}` : "Loading head model…"}</div>}
      {hover && placed[hover.index] && (
        <div className="tip viewer-tip" style={{ left: hover.x, top: hover.y }}>
          <strong>{names[hover.index]}</strong>
          <span>{percent(values?.[hover.index])}</span>
          {qualityName(quality?.[hover.index]) && <span className="dim">{qualityName(quality?.[hover.index])}</span>}
          {bad[hover.index] && <span className="bad">✕ bad</span>}
          {!placed[hover.index].known && <span className="dim">no standard position</span>}
        </div>
      )}
    </div>
  );
}
