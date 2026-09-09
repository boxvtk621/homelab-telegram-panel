// The watchdog lives outside test workers: a synchronous loop can starve
// node:test's own timeout. On POSIX, stop the entire isolated process group.
import { spawn } from 'node:child_process';
import { readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const cwd = fileURLToPath(new URL('../', import.meta.url));
const limit = Number(process.env.NODE_TEST_TIMEOUT_MS ?? '30000');
if (!Number.isInteger(limit) || limit < 250 || limit > 30000) {
  throw new Error('NODE_TEST_TIMEOUT_MS must be between 250 and 30000');
}
const files = process.argv.length > 2
  ? process.argv.slice(2)
  : readdirSync(new URL('../tests/', import.meta.url))
    .filter(name => name.endsWith('.test.mjs')).sort().map(name => `tests/${name}`);
if (!files.length) throw new Error('No Node tests selected');
const group = process.platform !== 'win32';
const child = spawn(process.execPath, ['--experimental-strip-types', '--test', '--test-concurrency=2', ...files], {
  cwd, stdio: 'inherit', detached: group,
});
let outcome;
let grace;
let stopping = false;
function signalChild(signal) {
  if (!child.pid) return;
  try {
    if (group) process.kill(-child.pid, signal);
    else child.kill(signal);
  } catch (error) {
    if (error.code !== 'ESRCH') throw error;
  }
}
function stop(code, reason) {
  if (stopping) return;
  stopping = true;
  outcome = code;
  console.error(reason);
  signalChild('SIGTERM');
  grace = setTimeout(() => signalChild('SIGKILL'), 1000);
}
const watchdog = setTimeout(() => stop(124, `Node tests exceeded ${limit}ms; stopping their process group.`), limit);
const interrupted = () => stop(130, 'Node tests interrupted.');
const terminated = () => stop(143, 'Node tests terminated.');
process.on('SIGINT', interrupted);
process.on('SIGTERM', terminated);
child.on('error', () => { outcome = 1; console.error('Node test runner could not start.'); });
child.on('close', (code) => {
  clearTimeout(watchdog);
  // The runner may exit before a worker which ignores SIGTERM. Keep the
  // group-wide hard-stop timer alive even after the runner has closed.
  if (!stopping) clearTimeout(grace);
  process.off('SIGINT', interrupted);
  process.off('SIGTERM', terminated);
  process.exitCode = outcome ?? code ?? 1;
});
