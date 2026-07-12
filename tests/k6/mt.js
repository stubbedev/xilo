// Multi-tenant conformance — the whole tenancy surface single-tenant ops.js
// never touches: self-registration + approval, plan CRUD, the three plan
// quotas (caches / storage / members), plan-gated org creation, and
// cross-account isolation of both tokens and dashboard views. Ends with the
// registration rate-limit probe (it poisons the per-IP login bucket, so it
// must run last).
//
//   XILO_E2E_CONFIG=server-mt.yaml \
//     docker compose -f tests/k6/compose.yaml run --rm k6 run /scripts/mt.js
//
// Runs 1 VU / 1 iteration with a single cookie jar, so identities are used
// sequentially (log in → act → log out) and auth operations are kept well
// under the login limiter's burst of 10 until the deliberate probe at the end.
//
// Threshold: every check must pass (rate==1).
import http from "k6/http";
import crypto from "k6/crypto";
import { check, fail } from "k6";
import { BASE, ADMIN_PASSWORD, chunkBytes, storePathFor, waitHealthy, adminLogin } from "./lib.js";

export const options = {
  scenarios: {
    mt: { executor: "shared-iterations", vus: 1, iterations: 1, maxDuration: "5m" },
  },
  thresholds: { checks: ["rate==1"] },
};

// Provoked 4xxs are asserted via checks, not counted as transport errors.
http.setResponseCallback(http.expectedStatuses({ min: 200, max: 499 }));

function must(res, name, cond) {
  const ok = check(res, { [name]: cond });
  if (!ok) console.error(`FAILED: ${name} status=${res.status} body=${String(res.body).slice(0, 200)}`);
  return ok;
}

function bearer(tok) {
  return { Authorization: `Bearer ${tok}` };
}

function logout() {
  http.post(`${BASE}/admin/logout`);
}

// register posts the signup form and returns the response.
function register(username, email, password, planID, org) {
  const body = { username, email, password };
  if (planID) body.plan = String(planID);
  if (org) body.org = org;
  return http.post(`${BASE}/register`, body);
}

// createPlan makes a plan via the admin form and returns its id (scraped from
// the settings page's edit/delete forms, matched by name).
function createPlan(fields) {
  const res = http.post(`${BASE}/admin/plans`, fields);
  must(res, `create plan ${fields.name}`, (r) => r.status === 200 && String(r.body).includes(fields.name));
  const settings = http.get(`${BASE}/admin/settings`);
  // Each plan row carries a /admin/plans/{id}/edit form near its name.
  const re = new RegExp(`plans/(\\d+)/(?:edit|delete)`, "g");
  const ids = [...String(settings.body).matchAll(re)].map((m) => parseInt(m[1], 10));
  if (!ids.length) fail(`no plan id found for ${fields.name}`);
  return Math.max(...ids); // newest plan = highest id
}

// mintToken creates a push+pull token for account/cache in the current
// session's context and returns the secret.
function mintToken(account, cache, name) {
  const res = http.post(`${BASE}/admin/tokens`, {
    name, cache: `${account}/${cache}`, push: "on", pull: "on",
  });
  const m = String(res.body).match(/[A-Za-z0-9_-]{40,}/);
  if (!m) fail(`no token secret for ${account}/${cache}: ${res.status}`);
  return m[0];
}

// pushOne uploads a 1 MiB path (2 × 512 KiB chunks) to account/cache and
// returns the put-path status. 1 MiB matches the smallest expressible plan
// storage cap (the plan form's minimum unit is MiB), so the first push lands
// and the second trips the quota.
function pushOne(account, cache, seed, tok) {
  const half = 512 * 1024;
  const c0 = chunkBytes(seed, 0, half);
  const c1 = chunkBytes(seed, 1, half);
  const h0 = crypto.sha256(c0, "hex");
  const h1 = crypto.sha256(c1, "hex");
  const cn = crypto.createHash("sha256");
  cn.update(c0);
  cn.update(c1);
  const narHex = cn.digest("hex");
  http.put(`${BASE}/c/${account}/${cache}/api/chunk/${h0}`, c0, { headers: bearer(tok) });
  http.put(`${BASE}/c/${account}/${cache}/api/chunk/${h1}`, c1, { headers: bearer(tok) });
  const res = http.put(`${BASE}/c/${account}/${cache}/api/path`,
    JSON.stringify({
      storePath: storePathFor(seed), narHash: `sha256:${narHex}`, narSize: 2 * half,
      deriver: "", references: [], chunks: [h0, h1],
    }),
    { headers: Object.assign({ "Content-Type": "application/json" }, bearer(tok)) });
  return res.status;
}

export default function () {
  waitHealthy(60);
  const ADMIN = ADMIN_PASSWORD;

  // ---------- registration is closed until an owner opens it ----------
  let res = http.get(`${BASE}/register`);
  must(res, "register 404 before enabling", (r) => r.status === 404);

  // ---------- owner: open registration (approval on) + define plans ----------
  adminLogin();
  res = http.post(`${BASE}/admin/settings/instance`, { allow_registrations: "on", require_approval: "on" });
  must(res, "enable registrations (approval on)", (r) => r.status === 200);

  // planSmall: tight caps, no orgs — drives the cache/storage/org gates. The
  // plan form's storage field is {value, unit} with a MiB minimum, so 1 MiB is
  // the smallest cap we can set.
  const planSmall = createPlan({
    name: "k6-small", max_caches: "1", max_members: "0",
    plan_storage_value: "1", plan_storage_unit: "MiB", public: "on",
  });
  // planOrg: orgs allowed, one member cap — drives org creation + member gate.
  const planOrg = createPlan({
    name: "k6-org", max_caches: "0", max_members: "1",
    orgs_allowed: "on", public: "on",
  });

  res = http.get(`${BASE}/register`);
  must(res, "register form open (200) with plans", (r) =>
    r.status === 200 && r.body.includes("k6-small") && r.body.includes("k6-org"));
  // signup must pick an offered plan when public plans exist
  res = register("noplan", "noplan@x.test", "password1");
  must(res, "register without a plan rejected", (r) => r.body.includes("offered plan"));

  // ---------- alice: pending registration → approval → login ----------
  res = register("alice", "alice@x.test", "password1", planSmall);
  must(res, "alice registers (pending)", (r) => r.status === 200 && r.body.includes("approve"));

  res = http.post(`${BASE}/admin/login`, { username: "alice", password: "password1" });
  must(res, "pending user cannot sign in", (r) => r.body.includes("awaiting approval"));

  // owner approves alice (session is still the owner's — pending login granted none)
  const settings = http.get(`${BASE}/admin/settings`);
  const am = String(settings.body).match(/users\/(\d+)\/approve/);
  if (!am) fail("no pending approve form for alice");
  const aliceId = am[1]; // reused later for the member-quota check
  res = http.post(`${BASE}/admin/users/${aliceId}/approve`);
  must(res, "owner approves alice", (r) => r.status === 200);
  logout();

  res = http.post(`${BASE}/admin/login`, { username: "alice", password: "password1" });
  must(res, "approved user signs in", (r) => r.status === 200 && !r.body.includes("awaiting"));

  // ---------- alice: cache quota (plan allows 1) ----------
  // ac1 is private so the cross-account isolation read below is meaningful
  // (a public nix-cache-info is open to everyone).
  res = http.post(`${BASE}/admin/caches`, { name: "ac1", namespace: "alice", priority: "40", private: "on" });
  must(res, "alice cache #1 created", (r) => r.status === 200);
  http.post(`${BASE}/admin/caches`, { name: "ac2", namespace: "alice", priority: "40" });
  res = http.get(`${BASE}/c/alice/ac2/nix-cache-info`);
  must(res, "alice cache #2 blocked by plan quota", (r) => r.status === 404);

  // ---------- alice: storage quota (plan caps logical bytes at 8192) ----------
  const aliceTok = mintToken("alice", "ac1", "alice-tok");
  const s1 = pushOne("alice", "ac1", 5101, aliceTok);
  must({ status: s1 }, "first push under quota accepted", () => s1 === 200);
  const s2 = pushOne("alice", "ac1", 5102, aliceTok);
  must({ status: s2 }, "push over storage quota rejected 403", () => s2 === 403);

  // ---------- alice: org creation denied (plan disallows orgs) ----------
  res = http.post(`${BASE}/admin/neworg`, { name: "aliceorg" });
  must(res, "alice org creation forbidden by plan", (r) => r.status === 403);
  logout();

  // ---------- owner: switch to instant registration ----------
  adminLogin();
  res = http.post(`${BASE}/admin/settings/instance`, { allow_registrations: "on" }); // require_approval off
  must(res, "disable approval requirement", (r) => r.status === 200);
  logout();

  // ---------- bob: instant registration (session granted immediately) ----------
  res = register("bob", "bob@x.test", "password1", planOrg, "");
  must(res, "bob registers instantly (session granted)", (r) =>
    r.status === 200 && !r.body.toLowerCase().includes("approval"));

  // bob's registration granted a session in the jar; act as bob.
  res = http.post(`${BASE}/admin/caches`, { name: "bc1", namespace: "bob", priority: "40" });
  must(res, "bob cache created", (r) => r.status === 200);
  const bobTok = mintToken("bob", "bc1", "bob-tok");

  // ---------- bob: plan-allowed org creation + member quota ----------
  res = http.post(`${BASE}/admin/neworg`, { name: "borg" });
  must(res, "bob creates org (plan allows)", (r) => r.status === 200);
  // borg starts with bob as its only member; plan caps members at 1, so
  // adding alice (by numeric user id) is refused with the quota message.
  res = http.post(`${BASE}/admin/org/borg/members`, { user_id: aliceId, role: "user" });
  must(res, "member add blocked by plan quota", (r) => String(r.body).includes("at most"));
  logout();

  // ---------- cross-account isolation (token scope) ----------
  res = http.get(`${BASE}/c/alice/ac1/nix-cache-info`, { headers: bearer(aliceTok) });
  must(res, "alice token reads her own cache", (r) => r.status === 200);
  res = http.put(`${BASE}/c/bob/bc1/api/chunk/${"00".repeat(32)}`, "x", { headers: bearer(aliceTok) });
  must(res, "alice token cannot push to bob's cache", (r) => r.status === 401);
  res = http.get(`${BASE}/c/alice/ac1/nix-cache-info`, { headers: bearer(bobTok) });
  must(res, "bob token cannot read alice's cache", (r) => r.status === 401);

  // ---------- cross-account isolation (dashboard view) ----------
  res = http.post(`${BASE}/admin/login`, { username: "alice", password: "password1" });
  must(res, "alice re-login", (r) => r.status === 200);
  res = http.get(`${BASE}/admin/cache/bob/bc1`);
  must(res, "alice cannot view bob's cache page (404)", (r) => r.status === 404);
  logout();

  // ---------- registration rate limit (LAST — poisons the per-IP bucket) ----------
  let got429 = false;
  for (let i = 0; i < 20; i++) {
    const rr = http.post(`${BASE}/register`,
      { username: `flood${i}`, email: `flood${i}@x.test`, password: "password1", plan: String(planOrg) },
      { responseCallback: http.expectedStatuses({ min: 200, max: 499 }) });
    if (rr.status === 429) { got429 = true; break; }
  }
  must({ status: got429 ? 429 : 0 }, "registration rate-limited after burst", () => got429);
}
