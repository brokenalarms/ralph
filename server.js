#!/usr/bin/env node

// Ralph Loop - Thin HTTP Server
// Wraps ralph.sh for remote monitoring/control via Tailscale or any network.
// Zero external dependencies - uses only Node.js builtins.

const http = require("http");
const { spawn, execSync } = require("child_process");
const fs = require("fs");
const path = require("path");

const PORT = parseInt(process.env.RALPH_PORT || "3411", 10);
const HOST = process.env.RALPH_HOST || "127.0.0.1";
const RALPH_SCRIPT = path.join(__dirname, "ralph.sh");

// Active loop state
let activeProcess = null;
let activeProjectDir = null;
let activePlanFile = null;

// --- Helpers ---

function ralphDir(projectDir) {
  return path.join(projectDir, ".ralph");
}

function readJSON(filePath) {
  try {
    return JSON.parse(fs.readFileSync(filePath, "utf-8"));
  } catch {
    return null;
  }
}

function readFile(filePath) {
  try {
    return fs.readFileSync(filePath, "utf-8");
  } catch {
    return null;
  }
}

function tailFile(filePath, lines = 50) {
  try {
    const content = fs.readFileSync(filePath, "utf-8");
    const allLines = content.split("\n");
    return allLines.slice(-lines).join("\n");
  } catch {
    return null;
  }
}

function json(res, statusCode, data) {
  res.writeHead(statusCode, {
    "Content-Type": "application/json",
    "Access-Control-Allow-Origin": "*",
  });
  res.end(JSON.stringify(data, null, 2));
}

function parseBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on("data", (chunk) => chunks.push(chunk));
    req.on("end", () => {
      try {
        resolve(JSON.parse(Buffer.concat(chunks).toString()));
      } catch {
        resolve({});
      }
    });
    req.on("error", reject);
  });
}

// --- Routes ---

async function handleStart(req, res) {
  if (activeProcess) {
    return json(res, 409, {
      error: "Loop already running",
      project: activeProjectDir,
    });
  }

  const body = await parseBody(req);
  const projectDir = body.dir || process.cwd();
  const maxIterations = body.max || 50;
  const prompt = body.prompt || "";
  const resume = body.resume || false;
  const planFile = body.plan_file || null;
  const callsPerHour = body.calls_per_hour || null;
  const tmux = body.tmux || false;

  if (!fs.existsSync(projectDir)) {
    return json(res, 400, { error: `Directory not found: ${projectDir}` });
  }

  const args = ["--dir", projectDir, "--max", String(maxIterations)];
  if (prompt) args.push("--prompt", prompt);
  if (planFile) args.push("--plan-file", planFile);
  if (resume) args.push("--resume");
  if (callsPerHour) args.push("--calls-per-hour", String(callsPerHour));
  if (tmux) args.push("--tmux");

  activeProjectDir = projectDir;
  activePlanFile = planFile;
  activeProcess = spawn("bash", [RALPH_SCRIPT, ...args], {
    cwd: projectDir,
    stdio: ["ignore", "pipe", "pipe"],
    detached: false,
  });

  activeProcess.stdout.on("data", (data) => {
    process.stdout.write(data);
  });

  activeProcess.stderr.on("data", (data) => {
    process.stderr.write(data);
  });

  activeProcess.on("close", (code) => {
    console.log(`[ralph-server] Loop exited with code ${code}`);
    activeProcess = null;
    activePlanFile = null;
  });

  json(res, 200, {
    status: "started",
    pid: activeProcess.pid,
    project: projectDir,
    max_iterations: maxIterations,
  });
}

function handleStatus(req, res) {
  const dir = activeProjectDir || req.headers["x-project-dir"];
  if (!dir) {
    return json(res, 200, { running: false, message: "No active loop" });
  }

  const rd = ralphDir(dir);
  const state = readJSON(path.join(rd, "state.json"));
  const planPath = activePlanFile
    ? (path.isAbsolute(activePlanFile) ? activePlanFile : path.join(dir, activePlanFile))
    : path.join(rd, "plan.md");
  const plan = readFile(planPath);
  const logTail = tailFile(path.join(rd, "loop.log"), 30);

  json(res, 200, {
    running: activeProcess !== null,
    pid: activeProcess?.pid || null,
    project: dir,
    plan_file: activePlanFile || null,
    state,
    log_tail: logTail,
    plan_preview: plan ? plan.substring(0, 2000) : null,
    stop_file_exists: fs.existsSync(path.join(rd, "stop")),
  });
}

function handleStop(req, res) {
  if (!activeProjectDir) {
    return json(res, 404, { error: "No active loop" });
  }

  const rd = ralphDir(activeProjectDir);
  fs.mkdirSync(rd, { recursive: true });
  fs.writeFileSync(path.join(rd, "stop"), `stopped at ${new Date().toISOString()}\n`);

  json(res, 200, {
    status: "stop_requested",
    message: "Stop file created. Loop will halt after current iteration.",
  });
}

function handleKill(req, res) {
  if (!activeProcess) {
    return json(res, 404, { error: "No active process" });
  }

  activeProcess.kill("SIGTERM");
  json(res, 200, { status: "killed", pid: activeProcess.pid });
}

function handleLog(req, res) {
  const dir = activeProjectDir || req.headers["x-project-dir"];
  if (!dir) {
    return json(res, 404, { error: "No project directory" });
  }

  const logPath = path.join(ralphDir(dir), "loop.log");
  const url = new URL(req.url, `http://${req.headers.host}`);
  const lines = parseInt(url.searchParams.get("lines") || "100", 10);
  const content = tailFile(logPath, lines);

  if (content === null) {
    return json(res, 404, { error: "No log file found" });
  }

  res.writeHead(200, {
    "Content-Type": "text/plain",
    "Access-Control-Allow-Origin": "*",
  });
  res.end(content);
}

function handleReset(req, res) {
  if (activeProcess) {
    return json(res, 409, {
      error: "Cannot reset while loop is running. Stop or kill first.",
    });
  }

  const dir = activeProjectDir || req.headers["x-project-dir"];
  if (!dir) {
    return json(res, 400, { error: "No project directory specified" });
  }

  const rd = ralphDir(dir);
  if (fs.existsSync(rd)) {
    fs.rmSync(rd, { recursive: true, force: true });
  }

  activeProjectDir = null;
  activePlanFile = null;
  json(res, 200, { status: "reset", message: ".ralph directory removed" });
}

function handlePlan(req, res) {
  const dir = activeProjectDir || req.headers["x-project-dir"];
  if (!dir) {
    return json(res, 404, { error: "No project directory" });
  }

  const planPath = activePlanFile
    ? (path.isAbsolute(activePlanFile) ? activePlanFile : path.join(dir, activePlanFile))
    : path.join(ralphDir(dir), "plan.md");
  const content = readFile(planPath);

  if (content === null) {
    return json(res, 404, { error: "No plan file found" });
  }

  res.writeHead(200, {
    "Content-Type": "text/markdown",
    "Access-Control-Allow-Origin": "*",
  });
  res.end(content);
}

// --- Server ---

const server = http.createServer(async (req, res) => {
  // CORS preflight
  if (req.method === "OPTIONS") {
    res.writeHead(204, {
      "Access-Control-Allow-Origin": "*",
      "Access-Control-Allow-Methods": "GET, POST, DELETE, OPTIONS",
      "Access-Control-Allow-Headers": "Content-Type, X-Project-Dir",
    });
    return res.end();
  }

  const url = new URL(req.url, `http://${req.headers.host}`);
  const route = `${req.method} ${url.pathname}`;

  try {
    switch (route) {
      case "POST /start":
        return await handleStart(req, res);
      case "GET /status":
        return handleStatus(req, res);
      case "POST /stop":
        return handleStop(req, res);
      case "POST /kill":
        return handleKill(req, res);
      case "GET /log":
        return handleLog(req, res);
      case "GET /plan":
        return handlePlan(req, res);
      case "DELETE /reset":
        return handleReset(req, res);
      case "GET /":
        return json(res, 200, {
          name: "ralph-server",
          version: "0.1.0",
          routes: [
            "POST /start   - Start a ralph loop",
            "GET  /status   - Get loop status",
            "POST /stop     - Request graceful stop",
            "POST /kill     - Kill the running process",
            "GET  /log      - Tail the loop log",
            "GET  /plan     - View the plan file",
            "DELETE /reset  - Clean .ralph state",
          ],
        });
      default:
        return json(res, 404, { error: "Not found" });
    }
  } catch (err) {
    console.error("[ralph-server] Error:", err);
    json(res, 500, { error: err.message });
  }
});

server.listen(PORT, HOST, () => {
  console.log(`[ralph-server] Listening on http://${HOST}:${PORT}`);
  console.log(`[ralph-server] Ralph script: ${RALPH_SCRIPT}`);
});

// Graceful shutdown
process.on("SIGINT", () => {
  console.log("\n[ralph-server] Shutting down...");
  if (activeProcess) {
    activeProcess.kill("SIGTERM");
  }
  server.close(() => process.exit(0));
});

process.on("SIGTERM", () => {
  if (activeProcess) {
    activeProcess.kill("SIGTERM");
  }
  server.close(() => process.exit(0));
});
