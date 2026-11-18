import { execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";

import { fail, input, saveState, sleep, workDir } from "./common.mjs";

const readyTimeoutMs = 60_000;
const platforms = {
  "linux-x64": "linux-amd64",
  "linux-arm64": "linux-arm64",
  "linux-riscv64": "linux-riscv64",
  "darwin-arm64": "darwin-arm64",
};

function pushEnabled(mode, token) {
  switch (mode) {
    case "auto":
      return token !== "";
    case "true":
      if (token === "") {
        throw new Error("push: true needs a token");
      }
      return true;
    case "false":
      return false;
  }
  throw new Error(`push must be auto, true or false, got ${JSON.stringify(mode)}`);
}

function install(dir, version) {
  const platform = platforms[`${process.platform}-${process.arch}`];
  if (!platform) {
    throw new Error(`no xilo release for ${process.platform}-${process.arch}`);
  }
  const release = version === "latest" ? "latest/download" : `download/${version}`;
  const archive = path.join(dir, "xilo.tar.gz");
  const url = `https://github.com/stubbedev/xilo/releases/${release}/xilo-${platform}.tar.gz`;
  execFileSync("curl", ["-fsSL", "--retry", "3", "-o", archive, url], { stdio: "inherit" });
  execFileSync("tar", ["-xzf", archive, "-C", dir], { stdio: "inherit" });
  fs.appendFileSync(process.env.GITHUB_PATH, `${dir}\n`);
  return path.join(dir, "xilo");
}

async function startWatcher(xilo, cache, dir, env) {
  const files = {
    log: path.join(dir, "watch.log"),
    pid: path.join(dir, "watch.pid"),
    status: path.join(dir, "watch.status"),
  };
  const out = fs.openSync(files.log, "w");
  const child = spawn(
    "sh",
    ["-c", '"$1" watch "$2" & echo $! > "$3"; wait $!; echo $? > "$4"', "sh", xilo, cache, files.pid, files.status],
    { detached: true, stdio: ["ignore", out, out], env },
  );
  child.unref();
  fs.closeSync(out);
  for (const [name, file] of Object.entries(files)) {
    saveState(name, file);
  }
  const deadline = Date.now() + readyTimeoutMs;
  while (Date.now() < deadline) {
    if (fs.existsSync(files.status)) {
      throw new Error(`xilo watch exited before it was ready:\n${fs.readFileSync(files.log, "utf8")}`);
    }
    if (fs.readFileSync(files.log, "utf8").includes("watching ")) {
      console.log(`Pushing every path this job builds to ${cache}`);
      return;
    }
    await sleep(100);
  }
  throw new Error(`xilo watch was not ready after ${readyTimeoutMs / 1000}s:\n${fs.readFileSync(files.log, "utf8")}`);
}

async function main() {
  const url = input("url");
  const cache = input("cache");
  const token = input("token");
  if (url === "" || cache === "") {
    throw new Error("url and cache are required");
  }
  if (token !== "") {
    console.log(`::add-mask::${token}`);
  }
  const pushing = pushEnabled(input("push") || "auto", token);
  const dir = workDir();
  fs.mkdirSync(dir, { recursive: true });
  const xilo = install(dir, input("version") || "latest");
  const env = { ...process.env, XILO_URL: url, XILO_TOKEN: token };
  execFileSync(xilo, ["login", url], { stdio: "inherit", env });
  execFileSync(xilo, ["use", cache], { stdio: "inherit", env });
  if (pushing) {
    await startWatcher(xilo, cache, dir, env);
  }
}

main().catch((err) => fail(err.message));
