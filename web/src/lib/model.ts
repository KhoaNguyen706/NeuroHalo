// The anatomical model: a real scalp and brain surface plus where each sensor
// sits on that head. The files come from replayer/make_head_model.py.

// Where one sensor sits (p) and which way is outward there (n).
// Loaded by: loadHeadModel. Used by: Head3D's layoutSensors.
export type Electrode = { p: [number, number, number]; n: [number, number, number] };

// A triangle mesh. Loaded by: loadHeadModel. Used by: Head3D (drawing) and
// buildWeights (spreading sensor values over it).
export type Surface = { positions: Float32Array; indices: Uint32Array };

// Everything the 3D view needs. Returned by: loadHeadModel. Used by: Head3D.
export type HeadModel = {
  scalp: Surface;
  cortex: Surface;
  cortexShade: Uint8Array; // per brain vertex: 0 deep in a fold, 255 on a ridge
  electrodes: Record<string, Electrode>;
};

let cached: Promise<HeadModel> | null = null;

// fetchBytes downloads one model file.
async function fetchBytes(name: string): Promise<ArrayBuffer> {
  const res = await fetch(`/model/${name}`);
  if (!res.ok) throw new Error(`cannot load /model/${name}: ${res.status}`);
  return res.arrayBuffer();
}

// loadHeadModel downloads the model once and shares it between views.
export function loadHeadModel(): Promise<HeadModel> {
  cached ??= (async () => {
    const [sp, si, cp, ci, cs, el] = await Promise.all([
      fetchBytes("scalp.pos.bin"),
      fetchBytes("scalp.idx.bin"),
      fetchBytes("cortex.pos.bin"),
      fetchBytes("cortex.idx.bin"),
      fetchBytes("cortex.shade.bin"),
      fetch("/model/electrodes.json").then((r) => r.json() as Promise<Record<string, Electrode>>),
    ]);
    return {
      scalp: { positions: new Float32Array(sp), indices: new Uint32Array(si) },
      cortex: { positions: new Float32Array(cp), indices: new Uint32Array(ci) },
      cortexShade: new Uint8Array(cs),
      electrodes: el,
    };
  })();
  cached.catch(() => (cached = null)); // let a later view try again
  return cached;
}

// For every vertex of a surface: its k nearest sensors, how much each one
// counts, and how strongly the vertex is covered at all (0 far from any sensor).
// Built by: buildWeights. Used by: Head3D's paint.
export type Weights = { k: number; index: Uint16Array; weight: Float32Array; cover: Float32Array };

// buildWeights prepares the spreading of sensor values over a surface. Each
// vertex takes a weighted average of its k nearest sensors, closer ones
// counting much more (1 / distance cubed). Coverage fades from 1 at `near`
// to 0 at `far` (scene units, 1 = 10 cm), so the face, the neck and the
// underside of the brain stay unpainted.
//
// sensors holds x,y,z per sensor; channel[i] is the channel number of sensor i.
export function buildWeights(positions: Float32Array, sensors: Float32Array, channel: number[],
  near: number, far: number, wantK = 6): Weights {
  const n = positions.length / 3;
  const m = sensors.length / 3;
  const k = Math.min(wantK, m);
  const index = new Uint16Array(n * k);
  const weight = new Float32Array(n * k);
  const cover = new Float32Array(n);
  const bestD = new Float64Array(k);
  const bestI = new Int32Array(k);

  for (let v = 0; v < n; v++) {
    const x = positions[v * 3], y = positions[v * 3 + 1], z = positions[v * 3 + 2];
    bestD.fill(Infinity);
    for (let s = 0; s < m; s++) {
      const dx = sensors[s * 3] - x, dy = sensors[s * 3 + 1] - y, dz = sensors[s * 3 + 2] - z;
      const d2 = dx * dx + dy * dy + dz * dz;
      if (d2 >= bestD[k - 1]) continue;
      let j = k - 1; // insert into the sorted list of the k nearest
      while (j > 0 && bestD[j - 1] > d2) {
        bestD[j] = bestD[j - 1];
        bestI[j] = bestI[j - 1];
        j--;
      }
      bestD[j] = d2;
      bestI[j] = s;
    }
    if (k === 0) continue;
    let total = 0;
    for (let j = 0; j < k; j++) {
      const w = 1 / (Math.pow(bestD[j], 1.5) + 1e-6);
      weight[v * k + j] = w;
      total += w;
    }
    for (let j = 0; j < k; j++) {
      weight[v * k + j] /= total;
      index[v * k + j] = channel[bestI[j]];
    }
    const t = Math.min(1, Math.max(0, (Math.sqrt(bestD[0]) - near) / (far - near)));
    cover[v] = 1 - t * t * (3 - 2 * t);
  }
  return { k, index, weight, cover };
}
