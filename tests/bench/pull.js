// Protocol-generic binary-cache pull load: GET narinfo → parse URL → GET NAR.
// Works unmodified against xilo AND attic (both speak the standard protocol),
// so the numbers are directly comparable.
//
//   k6 run -e BASE_URL=http://host:port/cache -e HASHES=/bench/hashes.txt pull.js
import http from "k6/http";
import { check } from "k6";
import exec from "k6/execution";
import { SharedArray } from "k6/data";

const BASE = __ENV.BASE_URL;
const hashes = new SharedArray("hashes", () =>
  open(__ENV.HASHES).split("\n").filter((l) => l.length > 0),
);

export const options = {
  scenarios: {
    narinfo: {
      executor: "constant-vus", vus: 16, duration: "20s",
      exec: "narinfo",
    },
    nar: {
      executor: "constant-vus", vus: 8, duration: "20s", startTime: "22s",
      exec: "nar",
    },
  },
  thresholds: {
    http_req_failed: ["rate<0.01"],
    // Loose bounds; their real purpose is forcing these submetrics into the
    // summary export so bench.sh can report them.
    "http_req_duration{name:narinfo}": ["p(95)<5000"],
    "http_req_duration{name:nar}": ["p(95)<60000"],
  },
};

function pick() {
  return hashes[exec.scenario.iterationInTest % hashes.length];
}

export function narinfo() {
  const res = http.get(`${BASE}/${pick()}.narinfo`, { tags: { name: "narinfo" } });
  check(res, { "narinfo 200": (r) => r.status === 200 });
}

export function nar() {
  const ni = http.get(`${BASE}/${pick()}.narinfo`, { tags: { name: "narinfo-for-nar" } });
  if (ni.status !== 200) return;
  const url = String(ni.body).match(/^URL: (.+)$/m)[1];
  const res = http.get(`${BASE}/${url}`, {
    headers: { "Accept-Encoding": "identity" },
    responseType: "none",
    tags: { name: "nar" },
  });
  check(res, { "nar 200": (r) => r.status === 200 });
}
