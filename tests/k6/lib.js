// Shared helpers: deterministic chunk generation and the xilo push protocol
// (chunk PUT + put-path), so scenarios can exercise the real pipeline without
// a Nix store. Content is seeded per path index — the same index always
// produces the same bytes, which is what makes dedup/GC-churn scenarios work.
//
// Every cache lives under /c/{account}/{cache}/… (there is no flat /{cache}
// route). Single-tenant deployments use the "default" account; multi-tenant
// runs spread load across several accounts (see TENANTS in perf/pressure/
// churn). Helpers take an optional `target` {account, cache, token} so one VU
// can address whichever tenant it was assigned; when omitted they fall back to
// the module-level ACCOUNT/CACHE/XILO_TOKEN.
import http from "k6/http";
import crypto from "k6/crypto";
import { check, sleep } from "k6";

export const BASE = __ENV.BASE_URL || "http://localhost:8080";
export const CACHE = __ENV.CACHE || "k6";
export const ACCOUNT = __ENV.ACCOUNT || "default";
export const ADMIN_PASSWORD = __ENV.ADMIN_PASSWORD || "k6-admin";
// The bootstrap dashboard user (cli/serve.go creates "admin" on first run).
export const ADMIN_USER = __ENV.ADMIN_USER || "admin";

// nix-base32 alphabet (no e, o, u, t).
const NIX32 = "0123456789abcdfghijklmnpqrsvwxyz";

// defaultTarget is the single-tenant cache addressed when a helper is called
// without an explicit target.
export function defaultTarget() {
  return { account: ACCOUNT, cache: CACHE, token: __ENV.XILO_TOKEN || "" };
}

// cachePrefix builds the /c/{account}/{cache} path a target is mounted at.
export function cachePrefix(target) {
  const t = target || defaultTarget();
  return `${BASE}/c/${t.account}/${t.cache}`;
}

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

export function authHeaders(extra, target) {
  const h = Object.assign({}, extra);
  const tok = target ? target.token : __ENV.XILO_TOKEN;
  if (tok) h["Authorization"] = `Bearer ${tok}`;
  return h;
}

// pushPath uploads a path's chunks then registers it against `target` (or the
// default single-tenant cache). Returns {storeHash, narHex, narSize, ok} —
// narHex is the sha256 of the reassembled NAR, which the server independently
// verifies before accepting.
export function pushPath(seed, chunkCount, chunkSize, tags, target) {
  const prefix = cachePrefix(target);
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
    const res = http.put(`${prefix}/api/chunk/${hex}`, data, {
      headers: authHeaders({}, target),
      tags: Object.assign({ name: "put-chunk" }, tags),
    });
    ok = check(res, { "chunk 200": (r) => r.status === 200 }) && ok;
  }

  const narHex = narHasher.digest("hex");
  const storePath = storePathFor(seed);
  const res = http.put(
    `${prefix}/api/path`,
    JSON.stringify({
      storePath: storePath,
      narHash: `sha256:${narHex}`,
      narSize: narSize,
      deriver: "",
      references: [],
      chunks: chunkHexes,
    }),
    {
      headers: authHeaders({ "Content-Type": "application/json" }, target),
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

// adminJar logs into the dashboard and returns a cookie jar with the session.
// Login is rate-limited (per-IP burst 10, refill 1/10s) so callers must reuse
// one jar rather than logging in per operation.
export function adminLogin() {
  const res = http.post(`${BASE}/admin/login`, { username: ADMIN_USER, password: ADMIN_PASSWORD });
  if (res.status !== 200) throw new Error(`admin login failed: ${res.status}`);
}

// setContext switches the dashboard's active account context so that
// subsequent cache creation (namespace preset) and token scoping resolve
// against `account` rather than the owner's personal namespace.
export function setContext(account) {
  http.post(`${BASE}/admin/context`, { ctx: account });
}

// ensureCache creates `target`'s account+cache through the admin dashboard —
// idempotent (an existing cache is fine), verified via nix-cache-info. Must be
// called with an active admin session (adminLogin first).
export function ensureCache(target) {
  const t = target || defaultTarget();
  const form = { name: t.cache, namespace: t.account, priority: "40" };
  if (t.private) form.private = "on";
  http.post(`${BASE}/admin/caches`, form);
  const res = http.get(`${cachePrefix(t)}/nix-cache-info`);
  if (res.status !== 200 && res.status !== 401) {
    // 401 is expected for a private cache read without a token — the create
    // still succeeded; only an outright failure (404/5xx) is fatal.
    throw new Error(`cache ${t.account}/${t.cache} unavailable: ${res.status}`);
  }
}

// mintToken creates a push+pull token scoped to `target` via the admin form
// and returns its secret (shown exactly once). Requires an active admin
// session whose context is set to target.account (see setContext).
export function mintToken(target, name) {
  const res = http.post(`${BASE}/admin/tokens`, {
    name: name || `k6-${target.account}-${target.cache}`,
    cache: `${target.account}/${target.cache}`,
    push: "on",
    pull: "on",
  });
  const m = String(res.body).match(/[A-Za-z0-9_-]{40,}/);
  if (!m) throw new Error(`no token secret for ${target.account}/${target.cache}: ${res.status}`);
  return m[0];
}

// provisionTenants builds the per-VU target list. TENANTS<=0 (single-tenant)
// yields one entry for the default account, pushing anonymously (open
// bootstrap) or with XILO_TOKEN. TENANTS>0 (multi-tenant) creates that many
// org accounts, each with its own cache and a scoped push token, so load can
// be sharded across accounts. Returns an array of {account, cache, token}.
export function provisionTenants(nTenants, cacheName) {
  const cache = cacheName || CACHE;
  if (!nTenants || nTenants <= 0) {
    const t = { account: ACCOUNT, cache, token: __ENV.XILO_TOKEN || "" };
    adminLogin();
    ensureCache(t);
    return [t];
  }
  adminLogin();
  const targets = [];
  for (let i = 0; i < nTenants; i++) {
    const account = `k6t${i}`;
    // Create the org account by creating a cache in it (owner mints accounts
    // on the fly), then switch context to mint a scoped token.
    http.post(`${BASE}/admin/orgs`, { name: account });
    const t = { account, cache, token: "" };
    ensureCache(t);
    setContext(account);
    t.token = mintToken(t);
    targets.push(t);
  }
  setContext(ACCOUNT);
  return targets;
}
