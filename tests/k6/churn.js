// Integrity soak — the "no dropped NARs, no corruption" regression test.
//
// Runs against a server configured with aggressive GC (short interval,
// retention and grace — see server-churn.yaml): paths are constantly evicted
// and their chunks swept while pushers re-register the same deterministic
// content, so every push dedups against chunks the sweeper may be deleting.
// This is exactly the GC-vs-push race window; after every registration the
// NAR is pulled back and hash-verified byte for byte.
//
//   docker compose -f tests/k6/compose.yaml --profile churn up --abort-on-container-exit
//
// Any dropped or corrupt NAR fails the nar_broken threshold (must be 0).
import http from "k6/http";
import crypto from "k6/crypto";
import { check } from "k6";
import exec from "k6/execution";
import { Counter } from "k6/metrics";
import { cachePrefix, pushPath, chunkBytes, authHeaders, waitHealthy, provisionTenants } from "./lib.js";

const RECURRING = 50; // fixed content set — re-pushed forever, dedup city
const CHUNKS = 3;
const CHUNK_SIZE = 128 * 1024;
// TENANTS>0 runs the churn across that many accounts at once.
const TENANTS = parseInt(__ENV.TENANTS || "0", 10);

const narBroken = new Counter("nar_broken");

export const options = {
  setupTimeout: "60s",
  scenarios: {
    dedup_churn: {
      executor: "constant-vus",
      vus: 8,
      duration: __ENV.DURATION || "3m",
      exec: "churn",
    },
    orphan_flood: {
      // Chunks uploaded but never registered: steady GC fodder that keeps the
      // sweeper genuinely busy deleting alongside the churn.
      executor: "constant-vus",
      vus: 2,
      duration: __ENV.DURATION || "3m",
      exec: "orphans",
    },
  },
  thresholds: {
    nar_broken: ["count==0"],
    checks: ["rate>0.99"],
  },
};

export function setup() {
  waitHealthy(60);
  return { targets: provisionTenants(TENANTS) };
}

function tn(data) {
  return exec.scenario.iterationInTest % data.targets.length;
}

export function churn(data) {
  const t = data.targets[tn(data)];
  const it = exec.scenario.iterationInTest;
  // 3 of 4 iterations: recurring set — stays referenced, pure dedup load.
  // 1 of 4: a "lapsing" seed shared per 45s window. Once its window passes
  // nobody touches it, so retention evicts the path and the sweeper kills its
  // chunks; when its window comes around again (3 windows later) the re-push
  // dedups against exactly-dying chunks — the GC-vs-push race path.
  let seed;
  if (it % 4 === 3) {
    seed = 6000 + (Math.floor(Date.now() / 45_000) % 3);
  } else {
    seed = it % RECURRING;
  }
  const r = pushPath(seed, CHUNKS, CHUNK_SIZE, { phase: "churn" }, t);
  if (!r.ok) return;

  // Read-back: narinfo present and the NAR reassembles to the exact bytes we
  // pushed. A 404/500 here or a hash mismatch is a dropped/corrupt NAR.
  const ni = http.get(`${cachePrefix(t)}/${r.storeHash}.narinfo`, {
    tags: { name: "narinfo" },
  });
  const nar = http.get(`${cachePrefix(t)}/nar/${r.storeHash}.nar`, {
    headers: { "Accept-Encoding": "identity" },
    responseType: "binary",
    tags: { name: "nar-verify" },
  });
  const intact =
    ni.status === 200 &&
    nar.status === 200 &&
    nar.body.byteLength === r.narSize &&
    crypto.sha256(nar.body, "hex") === r.narHex;
  if (!intact) {
    narBroken.add(1);
    console.error(
      `BROKEN NAR seed=${seed} narinfo=${ni.status} nar=${nar.status} ` +
        `len=${nar.body ? nar.body.byteLength : 0}/${r.narSize}`,
    );
  }
  check(nar, { "nar intact": () => intact });
}

// Note on timing: with server-churn.yaml (retention 30s, grace 15s, sweep 5s)
// runs shorter than ~2m never see mid-run eviction — keep DURATION >= 3m for
// the test to mean anything.

export function orphans(data) {
  const t = data.targets[tn(data)];
  const bytes = chunkBytes(0xdead0000 + exec.scenario.iterationInTest, 0, CHUNK_SIZE);
  const hex = crypto.sha256(bytes, "hex");
  const res = http.put(`${cachePrefix(t)}/api/chunk/${hex}`, bytes, {
    headers: authHeaders({}, t),
    tags: { name: "orphan-chunk" },
  });
  check(res, { "orphan chunk 200": (r) => r.status === 200 });
}
