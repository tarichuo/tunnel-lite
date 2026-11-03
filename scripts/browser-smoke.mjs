// Optional UI smoke test: PLAYWRIGHT_MODULE=/tmp/.../playwright-core/index.mjs node scripts/browser-smoke.mjs
import { spawn } from "node:child_process";
import { mkdtemp, readFile, writeFile, mkdir } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import net from "node:net";
import assert from "node:assert/strict";
import { pathToFileURL } from "node:url";
const { chromium } = await import(
  process.env.PLAYWRIGHT_MODULE
    ? pathToFileURL(process.env.PLAYWRIGHT_MODULE).href
    : "playwright-core"
);
const root = process.cwd(),
  work = await mkdtemp(path.join(tmpdir(), "tunnel-browser-"));
const children = [];
let browser, local;
async function port() {
  const server = net.createServer();
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  const p = server.address().port;
  await new Promise((r) => server.close(r));
  return p;
}
function child(file, args) {
  const p = spawn(file, args, {
    cwd: work,
    stdio: ["ignore", "ignore", "pipe"],
  });
  p.output = "";
  p.stderr.on("data", (b) => (p.output += b));
  children.push(p);
  return p;
}
async function wait(fn) {
  const until = Date.now() + 15000;
  while (Date.now() < until) {
    try {
      if (await fn()) return;
    } catch {}
    await new Promise((r) => setTimeout(r, 100));
  }
  throw Error("Timed out waiting for service");
}
async function exit(p) {
  if (p.exitCode !== null || p.signalCode !== null) return;
  p.kill("SIGTERM");
  await Promise.race([
    new Promise((r) => p.once("exit", r)),
    new Promise((_, reject) =>
      setTimeout(() => {
        p.kill("SIGKILL");
        reject(Error("Process shutdown timed out"));
      }, 10000),
    ),
  ]);
}
try {
  const init = child(path.join(root, "bin/tunnel-server"), [
    "-init",
    "-dir",
    work,
    "-public-host",
    "127.0.0.1",
  ]);
  assert.equal(await new Promise((r) => init.once("exit", r)), 0);
  const apiPort = await port(),
    controlPort = await port(),
    poolStart = await port();
  let yaml = await readFile(path.join(work, "server.yaml"), "utf8");
  yaml = yaml
    .replace("api_port: 7000", `api_port: ${apiPort}`)
    .replace("control_port: 7001", `control_port: ${controlPort}`)
    .replace("start: 10000", `start: ${poolStart}`)
    .replace("end: 20000", `end: ${Math.min(poolStart + 10, 65535)}`);
  await writeFile(path.join(work, "server.yaml"), yaml, { mode: 0o600 });
  const token = yaml.match(/admin_token: (\S+)/)[1];
  const server = child(path.join(root, "bin/tunnel-server"), [
    "-c",
    path.join(work, "server.yaml"),
  ]);
  await wait(async () => {
    const r = await fetch(`http://127.0.0.1:${apiPort}/`);
    return r.status === 200;
  });
  local = net.createServer({ allowHalfOpen: true }, (socket) => {
    const data = [];
    socket.on("data", (b) => data.push(b));
    socket.on("end", () => socket.end(Buffer.concat(data)));
    socket.on("error", () => {});
  });
  await new Promise((r) => local.listen(0, "127.0.0.1", r));
  browser = await chromium.launch({
    executablePath: process.env.CHROME_PATH || "/usr/bin/google-chrome",
    headless: true,
    args: ["--no-sandbox"],
  });
  const context = await browser.newContext({
    viewport: { width: 1440, height: 1000 },
    permissions: ["clipboard-read", "clipboard-write"],
  });
  const page = await context.newPage();
  const errors = [];
  page.setDefaultTimeout(15000);
  page.on("pageerror", (err) => errors.push(err.message));
  page.on("dialog", (d) => d.accept());
  await page.goto(`http://127.0.0.1:${apiPort}`);
  await page.locator("#login-token").fill(token);
  await page.getByRole("button", { name: "登录管理台" }).click();
  await page.getByRole("button", { name: "＋ 创建映射" }).waitFor();
  await page.getByRole("button", { name: "添加 Agent", exact: true }).click();
  await page.locator("#agent-form input[name=name]").fill("browser-agent");
  await page.getByRole("button", { name: "生成接入配置" }).click();
  const configDownload = page.waitForEvent("download");
  await page.getByRole("button", { name: "下载 agent.yaml" }).click();
  await (await configDownload).saveAs(path.join(work, "agent.yaml"));
  const caDownload = page.waitForEvent("download");
  await page.getByRole("button", { name: "下载 ca.crt" }).click();
  await (await caDownload).saveAs(path.join(work, "agent-ca.crt"));
  // The generated init CA and downloaded CA must be identical.
  assert.equal(
    await readFile(path.join(work, "agent-ca.crt"), "utf8"),
    await readFile(path.join(work, "ca.crt"), "utf8"),
  );
  child(path.join(root, "bin/tunnel-agent"), [
    "-c",
    path.join(work, "agent.yaml"),
  ]);
  await page.locator("#first-tunnel").waitFor();
  await wait(() => page.locator("#first-tunnel").isEnabled());
  await page.locator("#first-tunnel").click();
  await page.locator("#create-form input[name=name]").fill("browser-web");
  await page
    .locator("#create-form input[name=local_port]")
    .fill(String(local.address().port));
  await page
    .locator("#create-form")
    .getByRole("button", { name: "创建映射", exact: true })
    .click();
  await page.locator("tr .badge.ACTIVE").waitFor({ timeout: 15000 });
  await page.getByRole("button", { name: "复制公网地址" }).click();
  const publicAddress = await page.evaluate(() =>
    navigator.clipboard.readText(),
  );
  assert.match(publicAddress, /127\.0\.0\.1:\d+/);
  const echoed = await new Promise((resolve, reject) => {
    const c = net.connect({
      host: "127.0.0.1",
      port: Number(publicAddress.split(":").pop()),
    });
    const data = [];
    c.setTimeout(5000, () => c.destroy(Error("TCP timeout")));
    c.on("connect", () => c.end("browser-to-TCP"));
    c.on("data", (b) => data.push(b));
    c.on("end", () => resolve(Buffer.concat(data).toString()));
    c.on("error", reject);
  });
  assert.equal(echoed, "browser-to-TCP");
  const artifacts = path.join(work, "artifacts");
  await mkdir(artifacts);
  await page.screenshot({
    path: path.join(artifacts, "desktop.png"),
    fullPage: true,
  });
  await page.getByRole("button", { name: "下线", exact: true }).click();
  await page.getByRole("button", { name: "下线映射", exact: true }).click();
  await page.locator("#confirm").waitFor({ state: "hidden" });
  await page.locator("#status-filter").selectOption("STOPPED");
  await page.getByRole("button", { name: "启用", exact: true }).waitFor();
  await page.getByRole("button", { name: "启用", exact: true }).click();
  await page.getByRole("button", { name: "启用映射", exact: true }).click();
  await page.locator("#confirm").waitFor({ state: "hidden" });
  await page.locator("#status-filter").selectOption({ value: "ALL" });
  await page.locator("tr .badge.ACTIVE").waitFor({ timeout: 15000 });
  await page.getByRole("button", { name: "详情", exact: true }).click();
  await page.getByRole("heading", { name: "映射详情" }).waitFor();
  await page.getByRole("button", { name: "关闭", exact: true }).click();
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: path.join(artifacts, "mobile.png"),
    fullPage: true,
  });
  assert.ok(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
    "Mobile horizontal overflow",
  );
  await page.getByRole("link", { name: /Agent/ }).click();
  await page.getByRole("button", { name: "详情", exact: true }).click();
  await page.getByRole("heading", { name: "Agent 详情" }).waitFor();
  await page.getByRole("button", { name: "查看此 Agent 的全部映射" }).click();
  await page.locator("#status-filter").waitFor();
  assert.equal(
    await page.locator("#agent-filter").inputValue(),
    (await readFile(path.join(work, "agent.yaml"), "utf8")).match(
      /agent_id: "([^"]+)"/,
    )[1],
  );
  await page.getByRole("button", { name: "退出", exact: true }).click();
  await page.locator("#login-token").waitFor();
  assert.equal(errors.length, 0, errors.join("\n"));
  console.log(
    "PASS: login, Agent provisioning/downloads, TCP mapping, copy, stop/start, details, mobile layout, Agent filter, logout; no page errors.",
  );
  console.log("Screenshots: " + artifacts);
} finally {
  if (browser) await browser.close();
  for (const p of children.reverse()) await exit(p);
  if (local) await new Promise((r) => local.close(r));
}
