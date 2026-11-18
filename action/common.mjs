import fs from "node:fs";
import path from "node:path";

export function input(name) {
  return (process.env[`INPUT_${name.replaceAll(" ", "_").toUpperCase()}`] ?? "").trim();
}

export function saveState(name, value) {
  fs.appendFileSync(process.env.GITHUB_STATE, `${name}=${value}\n`);
}

export function state(name) {
  return process.env[`STATE_${name}`] ?? "";
}

export function workDir() {
  return path.join(process.env.RUNNER_TEMP, "xilo");
}

export function fail(message) {
  console.log(`::error::${message}`);
  process.exitCode = 1;
}

export const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
