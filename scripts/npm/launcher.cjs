#!/usr/bin/env node
'use strict';

const { spawn } = require('node:child_process');
const path = require('node:path');
const checkPlatform = require('./check-platform.cjs');

try {
  checkPlatform();
} catch (error) {
  console.error(error.message);
  process.exit(1);
}

const packageName = `@agentfeedback/cli-${process.platform}-${process.arch}`;
let binary;
try {
  const manifest = require.resolve(`${packageName}/package.json`);
  binary = path.join(path.dirname(manifest), process.platform === 'win32' ? 'agentfeedback.exe' : 'agentfeedback');
} catch (error) {
  console.error(`Missing ${packageName}. Reinstall @agentfeedback/cli with optional dependencies enabled (do not use --omit=optional or --no-optional).`);
  process.exit(1);
}

const child = spawn(binary, process.argv.slice(2), {
  stdio: 'inherit',
  env: process.env,
  shell: false,
});
const handlers = new Map();
for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
  const handler = () => child.kill(signal);
  handlers.set(signal, handler);
  process.on(signal, handler);
}
child.on('error', error => {
  console.error(`Could not run ${packageName}: ${error.message}`);
  process.exit(1);
});
child.on('exit', (status, signal) => {
  for (const [name, handler] of handlers) process.removeListener(name, handler);
  if (signal) process.kill(process.pid, signal);
  else process.exit(status === null ? 1 : status);
});
