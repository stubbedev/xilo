// Shared helpers: deterministic chunk generation and the xilo push protocol
// (chunk PUT + put-path), so scenarios can exercise the real pipeline without
// a Nix store. Content is seeded per path index — the same index always
// produces the same bytes, which is what makes dedup/GC-churn scenarios work.
import http from "k6/http";
import crypto from "k6/crypto";
import { check, sleep } from "k6";

export const BASE = __ENV.BASE_URL || "http://localhost:8080";
export const CACHE = __ENV.CACHE || "k6";

// nix-base32 alphabet (no e, o, u, t).
const NIX32 = "0123456789abcdfghijklmnpqrsvwxyz";

// xorshift32 — fast deterministic PRNG, good enough for content generation.
function xorshift(seed) {
  let s = seed >>> 0 || 1;
  return () => {
    s ^= s << 13; s >>>= 0;
    s ^= s >>> 17;
    s ^= s << 5; s >>>= 0;
    return s;
  };
}

// chunkBytes returns `size` deterministic bytes for (pathSeed, chunkIndex).
export function chunkBytes(pathSeed, chunkIndex, size) {
  const rnd = xorshift(pathSeed * 2654435761 + chunkIndex + 1);
  const buf = new Uint32Array(Math.ceil(size / 4));
  for (let i = 0; i < buf.length; i++) buf[i] = rnd();
  return buf.buffer.slice(0, size);
}

// storePathFor derives a valid-looking /nix/store path for a seed.
export function storePathFor(seed) {
  const rnd = xorshift(seed + 0x9e3779b9);
  let h = "";
  for (let i = 0; i < 32; i++) h += NIX32[rnd() % 32];
  return `/nix/store/${h}-k6-${seed}`;
}

export function authHeaders(extra) {
  const h = Object.assign({}, extra);
  if (__ENV.XILO_TOKEN) h["Authorization"] = `Bearer ${__ENV.XILO_TOKEN}`;
  return h;
}

// pushPath uploads a path's chunks then registers it. Returns
// {storeHash, narHex, narSize, chunkHexes, ok} — narHex is the sha256 of the
// reassembled NAR, which the server independently verifies before accepting.
export function pushPath(seed, chunkCount, chunkSize, tags) {
  const chunkHexes = [];
  const narHasher = crypto.createHash("sha256");
  let narSize = 0;
  let ok = true;

  for (let i = 0; i < chunkCount; i++) {
    const data = chunkBytes(seed, i, chunkSize);
    const hex = crypto.sha256(data, "hex");
    chunkHexes.push(hex);
    narHasher.update(data);
    narSize += data.byteLength;
    const res = http.put(`${BASE}/${CACHE}/api/chunk/${hex}`, data, {
      headers: authHeaders(),
      tags: Object.assign({ name: "put-chunk" }, tags),
    });
    ok = check(res, { "chunk 200": (r) => r.status === 200 }) && ok;
  }

  const narHex = narHasher.digest("hex");
  const storePath = storePathFor(seed);
  const res = http.put(
    `${BASE}/${CACHE}/api/path`,
    JSON.stringify({
      storePath: storePath,
      narHash: `sha256:${narHex}`,
      narSize: narSize,
      deriver: "",
      references: [],
      chunks: chunkHexes,
    }),
    {
      headers: authHeaders({ "Content-Type": "application/json" }),
      tags: Object.assign({ name: "put-path" }, tags),
    },
  );
  ok = check(res, { "path 200": (r) => r.status === 200 }) && ok;

  return { storeHash: storePath.slice(11, 43), narHex, narSize, ok };
}

// waitHealthy polls /healthz so scenarios don't need a compose healthcheck
// (the distroless image has nothing to run one with).
export function waitHealthy(timeoutSec) {
  for (let i = 0; i < (timeoutSec || 30); i++) {
    const res = http.get(`${BASE}/healthz`);
    if (res.status === 200) return;
    sleep(1);
  }
  throw new Error(`server at ${BASE} not healthy after ${timeoutSec}s`);
}

// ensureCache logs into the admin dashboard and creates the test cache —
// idempotent (an existing cache is fine), verified via nix-cache-info.
export function ensureCache() {
  http.post(`${BASE}/admin/login`, {
    password: __ENV.ADMIN_PASSWORD || "k6-admin",
  });
  http.post(`${BASE}/admin/caches`, { name: CACHE, priority: "40" });
  const res = http.get(`${BASE}/${CACHE}/nix-cache-info`);
  if (res.status !== 200) {
    throw new Error(`cache ${CACHE} unavailable after create: ${res.status}`);
  }
}
