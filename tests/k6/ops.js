// Operations conformance suite — every operation the server exposes, exercised
// end to end with correctness assertions (not just status codes). Run against
// the perf config (open bootstrap OFF is covered by the auth matrix, which
// creates private caches + scoped tokens through the admin API).
//
//   docker compose -f tests/k6/compose.yaml run --rm k6 run /scripts/ops.js
//
// Threshold: every check must pass (rate==1). Overlap with perf.js/churn.js
// is deliberate — this suite proves semantics, those prove load behavior.
import http from "k6/http";
import crypto from "k6/crypto";
import { check, fail } from "k6";
import { BASE, chunkBytes, storePathFor, waitHealthy } from "./lib.js";

export const options = {
  scenarios: {
    ops: { executor: "shared-iterations", vus: 1, iterations: 1, maxDuration: "5m" },
  },
  thresholds: {
    checks: ["rate==1"],
  },
};

const ADMIN = __ENV.ADMIN_PASSWORD || "k6-admin";

// This suite deliberately provokes 4xxs; correctness is asserted via checks.
http.setResponseCallback(http.expectedStatuses({ min: 200, max: 499 }));

// ---- helpers ----

function must(res, name, cond) {
  const ok = check(res, { [name]: cond });
  if (!ok) {
    console.error(`FAILED: ${name} status=${res.status} body=${String(res.body).slice(0, 200)}`);
  }
  return ok;
}

function putChunk(cache, data, headers, expect) {
  const hex = crypto.sha256(data, "hex");
  const res = http.put(`${BASE}/${cache}/api/chunk/${hex}`, data, { headers });
  must(res, `put-chunk ${expect}`, (r) => r.status === expect);
  return hex;
}

function putPath(cache, seed, chunkHexes, narHex, narSize, headers, expect, label) {
  const res = http.put(
    `${BASE}/${cache}/api/path`,
    JSON.stringify({
      storePath: storePathFor(seed),
      narHash: `sha256:${narHex}`,
      narSize: narSize,
      deriver: "",
      references: [],
      chunks: chunkHexes,
    }),
    { headers: Object.assign({ "Content-Type": "application/json" }, headers) },
  );
  must(res, label || `put-path ${expect}`, (r) => r.status === expect);
  return storePathFor(seed).slice(11, 43);
}

// adminLogin returns a cookie jar holding an authenticated admin session.
function adminLogin() {
  const jar = http.cookieJar();
  const res = http.post(`${BASE}/admin/login`, { password: ADMIN });
  must(res, "admin login 200", (r) => r.status === 200);
  return jar;
}

// createToken makes a token via the admin form and scrapes the secret out of
// the returned HTML (shown exactly once).
function createToken(name, perms, cacheScope) {
  const body = { name: name, cache: cacheScope || "*" };
  for (const p of perms) body[p] = "on";
  const res = http.post(`${BASE}/admin/tokens`, body);
  must(res, `token ${name} created`, (r) => r.status === 200);
  const m = String(res.body).match(/[A-Za-z0-9_-]{40,}/);
  if (!m) fail(`no secret in token response for ${name}`);
  return m[0];
}

function bearer(tok) {
  return { Authorization: `Bearer ${tok}` };
}

// ---- TOTP (RFC 6238, SHA1/30s/6 digits) in k6 JS, to drive the 2FA flow ----

function b32decode(s) {
  const A = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
  s = s.replace(/[=\s]/g, "").toUpperCase();
  const out = [];
  let bits = 0, val = 0;
  for (const ch of s) {
    const idx = A.indexOf(ch);
    if (idx < 0) fail(`bad base32 char ${ch}`);
    val = (val << 5) | idx;
    bits += 5;
    if (bits >= 8) {
      out.push((val >>> (bits - 8)) & 0xff);
      bits -= 8;
    }
  }
  return new Uint8Array(out);
}

function totpCode(secretB32) {
  const counter = Math.floor(Date.now() / 1000 / 30);
  const buf = new Uint8Array(8);
  let c = counter;
  for (let i = 7; i >= 0; i--) {
    buf[i] = c & 0xff;
    c = Math.floor(c / 256);
  }
  const mac = crypto.hmac("sha1", b32decode(secretB32).buffer, buf.buffer, "hex");
  const off = parseInt(mac.slice(-1), 16);
  const dbc = parseInt(mac.slice(off * 2, off * 2 + 8), 16) & 0x7fffffff;
  return String(dbc % 1000000).padStart(6, "0");
}

export default function () {
  waitHealthy(60);

  // ---------- health & observability ----------
  let res = http.get(`${BASE}/healthz`);
  must(res, "healthz 200 ok", (r) => r.status === 200 && r.body.includes("ok"));
  res = http.get(`${BASE}/healthz?format=json`);
  must(res, "healthz json", (r) => r.status === 200 && JSON.parse(r.body).status === "ok");
  res = http.get(`${BASE}/metrics`);
  must(res, "metrics exposes counters", (r) => r.status === 200 && r.body.includes("xilo_"));
  res = http.get(`${BASE}/`, { redirects: 0 });
  must(res, "index redirects to /admin", (r) => r.status === 302);
  res = http.get(`${BASE}/definitely/not/a/route.narinfo`);
  must(res, "unknown route 404", (r) => r.status === 404);

  // ---------- admin: session + cache lifecycle ----------
  res = http.post(`${BASE}/admin/login`, { password: "wrong-password" });
  must(res, "bad password rejected", (r) => r.status === 200 && r.body.includes("Invalid password"));
  adminLogin();

  // Idempotent across reruns of a persisted stack: "already exists" is fine.
  const created = (r) => r.status === 200 || String(r.body).includes("UNIQUE");
  res = http.post(`${BASE}/admin/caches`, { name: "pub", priority: "40" });
  must(res, "create cache pub", created);
  res = http.post(`${BASE}/admin/caches`, { name: "priv", priority: "40", private: "on" });
  must(res, "create cache priv", created);

  res = http.get(`${BASE}/admin/cache/pub`);
  must(res, "cache detail shows pubkey", (r) => r.status === 200 && r.body.includes("pub:"));
  res = http.get(`${BASE}/admin/status`);
  must(res, "status page", (r) => r.status === 200);
  res = http.get(`${BASE}/admin/status/data?range=1h`);
  must(res, "status data json", (r) => r.status === 200);
  res = http.get(`${BASE}/admin/settings`);
  must(res, "settings page", (r) => r.status === 200);

  // Tokens: full set of scopes used by the auth matrix below. Creating the
  // FIRST token also ends open bootstrap — push now requires auth everywhere.
  const pushTok = createToken("k6-push", ["push"], "*");
  const pullTok = createToken("k6-pull", ["pull"], "*");
  const scopedTok = createToken("k6-scoped", ["push", "pull"], "pub");
  const deadTok = createToken("k6-dead", ["push", "pull"], "*");

  // Revoke via admin. k6-dead is the newest token, so its id is the highest
  // revoke-form id on the dashboard (row layout independent).
  res = http.get(`${BASE}/admin`);
  const ids = [...String(res.body).matchAll(/tokens\/(\d+)\/revoke/g)].map((m) => parseInt(m[1], 10));
  if (!ids.length) fail("no token revoke forms found in dashboard");
  res = http.post(`${BASE}/admin/tokens/${Math.max(...ids)}/revoke`);
  must(res, "revoke token", (r) => r.status === 200);

  // ---------- pull protocol: public cache ----------
  res = http.get(`${BASE}/pub/nix-cache-info`);
  must(res, "nix-cache-info", (r) =>
    r.status === 200 && r.body.includes("StoreDir: /nix/store") && r.body.includes("WantMassQuery: 1"));

  // ---------- push pipeline on the public cache ----------
  const seed = 42;
  const c0 = chunkBytes(seed, 0, 65536);
  const c1 = chunkBytes(seed, 1, 65536);
  const narHasher = crypto.createHash("sha256");
  narHasher.update(c0);
  narHasher.update(c1);
  const narHex = narHasher.digest("hex");
  const h0 = crypto.sha256(c0, "hex");
  const h1 = crypto.sha256(c1, "hex");

  // push without token now 401 (bootstrap ended above)
  res = http.put(`${BASE}/pub/api/chunk/${h0}`, c0);
  must(res, "chunk push unauthenticated 401", (r) => r.status === 401);
  // pull-only token cannot push
  res = http.put(`${BASE}/pub/api/chunk/${h0}`, c0, { headers: bearer(pullTok) });
  must(res, "chunk push with pull token 401", (r) => r.status === 401);
  // revoked token cannot push
  res = http.put(`${BASE}/pub/api/chunk/${h0}`, c0, { headers: bearer(deadTok) });
  must(res, "chunk push with revoked token 401", (r) => r.status === 401);

  // config endpoint (advertises chunking params to pushers)
  res = http.get(`${BASE}/pub/api/config`, { headers: bearer(pushTok) });
  must(res, "api config", (r) => {
    const c = JSON.parse(r.body);
    return r.status === 200 && c.minSize > 0 && c.avgSize >= c.minSize && c.maxSize >= c.avgSize &&
      c.publicKey.startsWith("pub:") && c.public === true;
  });

  // get-missing-chunks before upload: both missing
  res = http.post(`${BASE}/pub/api/get-missing-chunks`, JSON.stringify({ hashes: [h0, h1] }),
    { headers: Object.assign({ "Content-Type": "application/json" }, bearer(pushTok)) });
  must(res, "missing-chunks before push", (r) =>
    r.status === 200 && JSON.parse(r.body).missing.length === 2);

  // wrong-hash upload rejected, nothing stored under the claimed hash
  res = http.put(`${BASE}/pub/api/chunk/${h0}`, c1, { headers: bearer(pushTok) });
  must(res, "chunk hash mismatch 400", (r) => r.status === 400);

  putChunk("pub", c0, bearer(pushTok), 200);
  putChunk("pub", c1, bearer(pushTok), 200);
  putChunk("pub", c0, bearer(pushTok), 200); // idempotent re-put (dedup hit)

  res = http.post(`${BASE}/pub/api/get-missing-chunks`, JSON.stringify({ hashes: [h0, h1] }),
    { headers: Object.assign({ "Content-Type": "application/json" }, bearer(pushTok)) });
  must(res, "missing-chunks after push empty", (r) =>
    r.status === 200 && (JSON.parse(r.body).missing || []).length === 0);

  // put-path rejections: unknown chunk / wrong hash / wrong size / bad hash format
  putPath("pub", seed, [h0, h1, crypto.sha256(chunkBytes(9, 9, 10), "hex")], narHex, 131072,
    bearer(pushTok), 400, "put-path unknown chunk 400");
  putPath("pub", seed, [h0, h1], crypto.sha256(c0, "hex"), 131072,
    bearer(pushTok), 400, "put-path wrong narHash 400");
  putPath("pub", seed, [h0, h1], narHex, 999,
    bearer(pushTok), 400, "put-path wrong narSize 400");
  res = http.put(`${BASE}/pub/api/path`,
    JSON.stringify({ storePath: storePathFor(seed), narHash: "sha256:zzz", narSize: 1, chunks: [h0] }),
    { headers: Object.assign({ "Content-Type": "application/json" }, bearer(pushTok)) });
  must(res, "put-path malformed narHash 400", (r) => r.status === 400);
  res = http.put(`${BASE}/pub/api/path`, "{not json",
    { headers: Object.assign({ "Content-Type": "application/json" }, bearer(pushTok)) });
  must(res, "put-path bad json 400", (r) => r.status === 400);

  // chunk order matters: reversed list reassembles to a different hash → 400
  putPath("pub", seed, [h1, h0], narHex, 131072,
    bearer(pushTok), 400, "put-path wrong chunk order 400");

  // the real registration
  const storeHash = putPath("pub", seed, [h0, h1], narHex, 131072, bearer(pushTok), 200, "put-path 200");

  // get-missing-paths semantics
  res = http.post(`${BASE}/pub/api/get-missing-paths`,
    JSON.stringify({ hashes: [storeHash, "0000000000000000000000000000000a"] }),
    { headers: Object.assign({ "Content-Type": "application/json" }, bearer(pushTok)) });
  must(res, "missing-paths semantics", (r) => {
    const miss = JSON.parse(r.body).missing || [];
    return r.status === 200 && miss.length === 1 && miss[0] === "0000000000000000000000000000000a";
  });

  // ---------- narinfo correctness ----------
  res = http.get(`${BASE}/pub/${storeHash}.narinfo`);
  must(res, "narinfo full contract", (r) =>
    r.status === 200 &&
    r.body.includes(`StorePath: ${storePathFor(seed)}`) &&
    r.body.includes(`URL: nar/${storeHash}.nar`) &&
    r.body.includes("NarSize: 131072") &&
    r.body.includes("Compression: none") &&
    r.body.includes("Sig: pub:") &&
    r.headers["Cache-Control"].includes("immutable") &&
    r.headers["Etag"] === `"${storeHash}"`);
  res = http.get(`${BASE}/pub/ffffffffffffffffffffffffffffffff.narinfo`);
  must(res, "narinfo miss 404 + negative cache", (r) =>
    r.status === 404 && r.headers["Cache-Control"].includes("max-age=30"));

  // ---------- NAR serving: byte-exact on all encodings ----------
  res = http.get(`${BASE}/pub/nar/${storeHash}.nar`, {
    headers: { "Accept-Encoding": "identity" }, responseType: "binary",
  });
  must(res, "nar identity byte-exact", (r) =>
    r.status === 200 && r.body.byteLength === 131072 && crypto.sha256(r.body, "hex") === narHex);

  // gzip: k6 decodes transparently — hash proves the encoder round-trips
  res = http.get(`${BASE}/pub/nar/${storeHash}.nar`, {
    headers: { "Accept-Encoding": "gzip" }, responseType: "binary",
  });
  must(res, "nar gzip byte-exact", (r) =>
    r.status === 200 && crypto.sha256(r.body, "hex") === narHex);

  // zstd: k6 decodes transparently (multi-frame concat) — byte-exact hash
  // proves the stored-frame passthrough reassembles the true NAR.
  res = http.get(`${BASE}/pub/nar/${storeHash}.nar`, {
    headers: { "Accept-Encoding": "zstd" }, responseType: "binary",
  });
  must(res, "nar zstd byte-exact", (r) =>
    r.status === 200 && r.headers["Content-Encoding"] === "zstd" &&
    r.body.byteLength === 131072 && crypto.sha256(r.body, "hex") === narHex);

  res = http.get(`${BASE}/pub/nar/ffffffffffffffffffffffffffffffff.nar`);
  must(res, "nar miss 404", (r) => r.status === 404);

  // re-push same path (upsert) then confirm still intact
  putPath("pub", seed, [h0, h1], narHex, 131072, bearer(pushTok), 200, "put-path upsert 200");
  res = http.get(`${BASE}/pub/nar/${storeHash}.nar`, {
    headers: { "Accept-Encoding": "identity" }, responseType: "binary",
  });
  must(res, "nar intact after upsert", (r) => crypto.sha256(r.body, "hex") === narHex);

  // ---------- private cache auth matrix ----------
  const pseed = 77;
  const pc = chunkBytes(pseed, 0, 4096);
  const ph = crypto.sha256(pc, "hex");
  const pnar = crypto.sha256(pc, "hex");
  putChunk("priv", pc, bearer(pushTok), 200);
  const privHash = putPath("priv", pseed, [ph], pnar, 4096, bearer(pushTok), 200, "priv put-path 200");

  const privReads = [
    ["nix-cache-info", `${BASE}/priv/nix-cache-info`],
    ["narinfo", `${BASE}/priv/${privHash}.narinfo`],
    ["nar", `${BASE}/priv/nar/${privHash}.nar`],
    ["api config", `${BASE}/priv/api/config`],
  ];
  for (const [name, url] of privReads) {
    res = http.get(url);
    must(res, `priv ${name} anonymous 401`, (r) => r.status === 401);
    res = http.get(url, { headers: bearer(pullTok) });
    must(res, `priv ${name} with pull token 200`, (r) => r.status === 200);
    // scoped to "pub" only — must not open "priv"
    res = http.get(url, { headers: bearer(scopedTok) });
    must(res, `priv ${name} wrong-scope token 401`, (r) => r.status === 401);
  }
  // public cache reads stay open to anonymous
  res = http.get(`${BASE}/pub/${storeHash}.narinfo`);
  must(res, "pub narinfo anonymous still 200", (r) => r.status === 200);
  // scoped token pushes fine to its own cache
  putChunk("pub", chunkBytes(5, 5, 2048), bearer(scopedTok), 200);
  // ...but not to priv
  res = http.put(`${BASE}/priv/api/chunk/${ph}`, pc, { headers: bearer(scopedTok) });
  must(res, "priv push wrong-scope 401", (r) => r.status === 401);

  // ---------- admin: configure / rotate / GC / delete ----------
  res = http.post(`${BASE}/admin/cache/pub/configure`,
    { priority: "30", retention_value: "", cap_value: "" });
  must(res, "configure cache", (r) => r.status === 200);

  res = http.get(`${BASE}/pub/nix-cache-info`);
  must(res, "priority applied", (r) => r.body.includes("Priority: 30"));

  // key rotation: pubkey must change, old narinfo re-signed with new key name
  const keyBefore = String(http.get(`${BASE}/pub/api/config`, { headers: bearer(pushTok) }).body);
  res = http.post(`${BASE}/admin/cache/pub/rotate`);
  must(res, "rotate key", (r) => r.status === 200);
  const keyAfter = String(http.get(`${BASE}/pub/api/config`, { headers: bearer(pushTok) }).body);
  must({ body: keyAfter }, "pubkey changed after rotate", () =>
    JSON.parse(keyBefore).publicKey !== JSON.parse(keyAfter).publicKey);
  res = http.get(`${BASE}/pub/${storeHash}.narinfo`);
  must(res, "narinfo re-signed after rotate", (r) =>
    r.status === 200 && r.body.includes(`Sig: pub:`));

  // GC via dashboard button: nothing referenced should vanish
  res = http.post(`${BASE}/admin/gc`);
  must(res, "admin gc runs", (r) => r.status === 200);
  res = http.get(`${BASE}/pub/nar/${storeHash}.nar`, {
    headers: { "Accept-Encoding": "identity" }, responseType: "binary",
  });
  must(res, "nar survives gc", (r) => crypto.sha256(r.body, "hex") === narHex);

  // delete cache: its endpoints go 404, other caches untouched
  res = http.post(`${BASE}/admin/caches`, { name: "doomed", priority: "40" });
  must(res, "create doomed cache", (r) => r.status === 200);
  res = http.post(`${BASE}/admin/cache/doomed/delete`);
  must(res, "delete cache", (r) => r.status === 200);
  res = http.get(`${BASE}/doomed/nix-cache-info`);
  must(res, "deleted cache 404", (r) => r.status === 404);
  res = http.get(`${BASE}/pub/nix-cache-info`);
  must(res, "sibling cache survives delete", (r) => r.status === 200);

  // ---------- dashboard browsing: search / sort / paging / detail ----------
  res = http.get(`${BASE}/admin?caches%5Bq%5D=pub&tokens%5Bq%5D=k6-push`);
  must(res, "dashboard search filters", (r) =>
    r.status === 200 && r.body.includes("pub") && r.body.includes("k6-push"));
  res = http.get(`${BASE}/admin/cache/pub?paths%5Bnumber%5D=2&paths%5Bsize%5D=1&paths%5Bq%5D=k6`);
  must(res, "cache detail paged path search", (r) => r.status === 200);
  res = http.get(`${BASE}/admin/cache/nonexistent`);
  must(res, "cache detail 404", (r) => r.status === 404);
  for (const w of ["10", "60", "1440", "43200"]) {
    res = http.get(`${BASE}/admin/status/data?window=${w}`);
    must(res, `status data window=${w}`, (r) => r.status === 200 && r.body.includes("minT"));
  }
  res = http.get(`${BASE}/admin/status/data?from=2026-01-01&to=2026-01-02`);
  must(res, "status data custom range", (r) => r.status === 200);
  res = http.get(`${BASE}/static/pico.min.css`);
  must(res, "static asset served", (r) => r.status === 200 && r.body.length > 1000);

  // ---------- token edit ----------
  res = http.get(`${BASE}/admin`);
  const editIds = [...String(res.body).matchAll(/tokens\/(\d+)\/(?:edit|revoke)/g)].map((m) => parseInt(m[1], 10));
  const editId = Math.min(...editIds); // k6-push, the first created
  res = http.post(`${BASE}/admin/tokens/${editId}/edit`,
    { name: "k6-push-renamed", cache: "*", push: "on", pull: "on", permanent: "on" },
    { redirects: 0 });
  must(res, "token edit redirects", (r) => r.status === 303);
  res = http.get(`${BASE}/admin`);
  must(res, "token rename visible", (r) => r.body.includes("k6-push-renamed"));
  // token keeps working after edit (same secret, new perms)
  putChunk("pub", chunkBytes(6, 6, 1024), bearer(pushTok), 200);

  // ---------- password change (change → verify → revert) ----------
  res = http.post(`${BASE}/admin/settings/password/check`, { new: "swordfish-9", confirm: "swordfish-9" });
  must(res, "password strength check", (r) => r.status === 200);
  res = http.post(`${BASE}/admin/settings/password`,
    { current: ADMIN, new: "swordfish-9", confirm: "swordfish-9" });
  must(res, "password change", (r) => r.status === 200);
  res = http.post(`${BASE}/admin/settings/password`,
    { current: "swordfish-9", new: ADMIN, confirm: ADMIN });
  must(res, "password revert", (r) => r.status === 200);
  res = http.post(`${BASE}/admin/settings/password`,
    { current: "wrong", new: "x-1234567", confirm: "x-1234567" });
  must(res, "password change wrong current rejected", (r) =>
    String(r.body).includes("incorrect") || String(r.body).includes("wrong") || r.status !== 200 ||
    String(r.body).includes("Current"));

  // ---------- TOTP 2FA full cycle ----------
  res = http.post(`${BASE}/admin/settings/totp/enroll`);
  must(res, "totp enroll shows secret", (r) => r.status === 200 && /<code>[A-Z2-7 ]+<\/code>/.test(r.body));
  const secret = String(res.body).match(/<code>([A-Z2-7 ]+)<\/code>/)[1];
  res = http.post(`${BASE}/admin/settings/totp/enable`, { code: "000000" });
  must(res, "totp enable wrong code rejected", (r) => String(r.body).includes("match")); // "didn't match" (apostrophe HTML-escaped)
  res = http.post(`${BASE}/admin/settings/totp/enable`, { code: totpCode(secret) });
  must(res, "totp enable", (r) => r.status === 200 && r.body.includes("enabled"));

  // login now demands the second factor
  http.post(`${BASE}/admin/logout`);
  res = http.post(`${BASE}/admin/login`, { password: ADMIN });
  must(res, "login with 2fa asks for code", (r) => r.status === 200 && r.body.includes("pending"));
  const pending = String(res.body).match(/name="pending"[^>]*value="([^"]+)"/)[1];
  res = http.post(`${BASE}/admin/login/code`, { pending: pending, code: "111111" });
  must(res, "2fa wrong code rejected", (r) => !String(r.body).includes("Signed in") &&
    (String(r.body).includes("code") || String(r.body).includes("expired")));
  res = http.post(`${BASE}/admin/login`, { password: ADMIN });
  const pending2 = String(res.body).match(/name="pending"[^>]*value="([^"]+)"/)[1];
  res = http.post(`${BASE}/admin/login/code`, { pending: pending2, code: totpCode(secret) });
  must(res, "2fa login completes", (r) => r.status === 200);
  res = http.post(`${BASE}/admin/settings/totp/disable`);
  must(res, "totp disable", (r) => r.status === 200 && r.body.includes("disabled"));
  http.post(`${BASE}/admin/logout`);
  res = http.post(`${BASE}/admin/login`, { password: ADMIN });
  must(res, "plain login after totp disable", (r) => r.status === 200 && !r.body.includes("pending"));

  // ---------- session security ----------
  res = http.post(`${BASE}/admin/logout`);
  must(res, "logout", (r) => r.status === 200 || r.status === 302);
  res = http.post(`${BASE}/admin/caches`, { name: "nope", priority: "40" });
  must(res, "admin mutation after logout rejected", (r) => r.status !== 200 || String(r.body).includes("password"));
}
