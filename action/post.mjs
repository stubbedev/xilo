import fs from "node:fs";

import { fail, input, sleep, state } from "./common.mjs";

async function main() {
  const files = { log: state("log"), pid: state("pid"), status: state("status") };
  if (files.pid === "") {
    return;
  }
  const timeoutMinutes = Number(input("push-timeout-minutes") || "60");
  if (!Number.isFinite(timeoutMinutes) || timeoutMinutes <= 0) {
    throw new Error(`push-timeout-minutes must be a positive number, got ${JSON.stringify(input("push-timeout-minutes"))}`);
  }
  if (!fs.existsSync(files.status)) {
    try {
      process.kill(Number(fs.readFileSync(files.pid, "utf8").trim()), "SIGTERM");
    } catch (err) {
      if (err.code !== "ESRCH") {
        throw err;
      }
    }
  }
  const deadline = Date.now() + timeoutMinutes * 60_000;
  let announced = false;
  while (!fs.existsSync(files.status)) {
    if (Date.now() > deadline) {
      throw new Error(`xilo watch still pushing after ${timeoutMinutes} minutes`);
    }
    if (!announced) {
      console.log("Waiting for xilo to push what this job built");
      announced = true;
    }
    await sleep(1000);
  }
  console.log("::group::xilo watch");
  console.log(fs.readFileSync(files.log, "utf8"));
  console.log("::endgroup::");
  const status = fs.readFileSync(files.status, "utf8").trim();
  if (status !== "0") {
    throw new Error(`xilo watch exited with status ${status}; some paths were not pushed (see the log above)`);
  }
}

main().catch((err) => fail(err.message));
