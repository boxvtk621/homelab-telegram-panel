// Run inside the existing NPM container as root.
// The password is read from a root-owned 0600 file and is never logged.
import fs from 'node:fs';
import { execFileSync } from 'node:child_process';
import crypto from 'node:crypto';
import Model from '/app/models/proxy_host.js';
import nginx from '/app/internal/nginx.js';

const active = '/data/nginx/proxy_host/2.conf';
const sharedAccess = '/data/access/1';
const panelAccess = '/data/access/panel';
const marker = /# BEGIN managed Panel HL-241[\s\S]*?# END managed Panel HL-241\n?/g;
const sourceDirective = `auth_basic_user_file ${sharedAccess};`;
const panelDirective = `auth_basic_user_file ${panelAccess};`;
const username = process.argv[2];
const passwordFile = process.argv[3];
const apply = process.argv.includes('--apply');
const check = (ok, message) => { if (!ok) throw new Error(message); };
const hash = value => crypto.createHash('sha256').update(value).digest('hex');
const runNginx = (...args) => execFileSync('/usr/sbin/nginx', args, { stdio: 'pipe', timeout: 15000 });
const readHost = () => Model.query().findById(2).withGraphFetched('certificate')
  .modifyGraph('certificate', query => query.select('id', 'provider'));

let backup;
let testConfig;
let original;
let before;
let candidateBytes;
let nextAdvanced;
let appliedSnapshot;
let configCommitted = false;
let configReplaced = false;
let accessReplaced = false;
let priorAccessExists = false;

try {
  check(process.getuid() === 0, 'ROOT_REQUIRED');
  check(typeof username === 'string' && /^[a-z][a-z0-9-]{2,31}$/.test(username), 'INVALID_TEST_USERNAME');
  check(typeof passwordFile === 'string' && passwordFile.startsWith('/run/') &&
    passwordFile === fs.realpathSync(passwordFile), 'INVALID_PASSWORD_FILE');
  const passwordStat = fs.lstatSync(passwordFile);
  check(passwordStat.isFile() && !passwordStat.isSymbolicLink() && passwordStat.uid === 0 &&
    (passwordStat.mode & 0o077) === 0 && passwordStat.size >= 32 && passwordStat.size <= 256,
    'UNSAFE_PASSWORD_FILE');
  const password = fs.readFileSync(passwordFile, 'utf8');
  check(password.length >= 32 && password.length <= 128 && /^[\x21-\x7e]+$/.test(password), 'INVALID_TEST_PASSWORD');

  const sharedStat = fs.lstatSync(sharedAccess);
  check(sharedStat.isFile() && !sharedStat.isSymbolicLink() && sharedStat.uid === 0 &&
    (sharedStat.mode & 0o022) === 0, 'UNSAFE_SHARED_ACCESS_FILE');
  const shared = fs.readFileSync(sharedAccess, 'utf8');
  const sharedLines = shared.split('\n').filter(Boolean);
  check(sharedLines.length > 0 && sharedLines.every(line => /^[^:\s]+:[^\r\n]+$/.test(line)),
    'INVALID_SHARED_ACCESS_FILE');
  const sharedUsers = sharedLines.map(line => line.slice(0, line.indexOf(':')));
  check(new Set(sharedUsers).size === sharedUsers.length && !sharedUsers.includes(username),
    'TEST_USERNAME_COLLISION');
  const passwordHash = execFileSync('openssl', ['passwd', '-apr1', '-stdin'], {
    input: password + '\n', encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'], timeout: 15000,
  }).trim();
  check(/^\$apr1\$[^$]+\$[^\r\n]+$/.test(passwordHash), 'PASSWORD_HASH_FAILED');
  const nextAccess = Buffer.from(sharedLines.join('\n') + `\n${username}:${passwordHash}\n`);

  original = await readHost();
  check(original?.enabled && !original.is_deleted && original.access_list_id === 0 &&
    JSON.stringify(original.domain_names) === '["h1-cloud.ru"]' &&
    original.certificate_id === 7 && original.certificate?.provider === 'letsencrypt' &&
    typeof original.advanced_config === 'string', 'TARGET_CHANGED');
  const matches = original.advanced_config.match(marker) || [];
  check(matches.length === 1, 'PANEL_ROUTE_MISSING_OR_AMBIGUOUS');
  const panelBlock = matches[0];
  const sharedCount = panelBlock.split(sourceDirective).length - 1;
  const panelCount = panelBlock.split(panelDirective).length - 1;
  check(sharedCount + panelCount === 1, 'PANEL_ACCESS_DIRECTIVE_CHANGED');
  check(!original.advanced_config.replace(panelBlock, '').includes(panelDirective), 'PANEL_ACCESS_FILE_REUSED');
  const nextBlock = panelBlock.replace(sharedCount === 1 ? sourceDirective : panelDirective, panelDirective);
  nextAdvanced = original.advanced_config.replace(panelBlock, nextBlock);

  before = fs.readFileSync(active);
  const snapshot = JSON.stringify(original);
  backup = fs.mkdtempSync('/data/nginx/panel-account-');
  fs.chmodSync(backup, 0o700);
  fs.writeFileSync(`${backup}/host2.before.conf`, before, { mode: 0o600 });
  fs.writeFileSync(`${backup}/host2.before.json`, snapshot, { mode: 0o600 });
  fs.writeFileSync(`${backup}/panel.candidate.htpasswd`, nextAccess, { mode: 0o600, flag: 'wx' });
  const candidate = `${backup}/host2.candidate.conf`;
  const next = { ...original, advanced_config: nextAdvanced };
  const getName = nginx.getConfigName;
  try {
    nginx.getConfigName = () => `${backup}/host2.rendered-original.conf`;
    await nginx.generateConfig('proxy_host', original);
    check(hash(fs.readFileSync(`${backup}/host2.rendered-original.conf`)) === hash(before),
      'EXISTING_CONFIG_MODEL_DRIFT');
    nginx.getConfigName = () => candidate;
    await nginx.generateConfig('proxy_host', next);
  } finally { nginx.getConfigName = getName; }
  candidateBytes = fs.readFileSync(candidate);

  const main = fs.readFileSync('/etc/nginx/nginx.conf', 'utf8');
  const include = /include\s+\/data\/nginx\/proxy_host\/\*\.conf;/g;
  check([...main.matchAll(include)].length === 1, 'NGINX_INCLUDE_LAYOUT_CHANGED');
  const siblings = fs.readdirSync('/data/nginx/proxy_host').filter(name => name.endsWith('.conf') && name !== '2.conf');
  check(siblings.every(name => /^[0-9]+\.conf$/.test(name)), 'UNEXPECTED_HOST_FILENAME');
  let candidateInclude = fs.readFileSync(candidate, 'utf8');
  check(candidateInclude.split(panelDirective).length - 1 === 1, 'PANEL_ACCESS_DIRECTIVE_CHANGED');
  candidateInclude = candidateInclude.replace(panelDirective,
    `auth_basic_user_file ${backup}/panel.candidate.htpasswd;`);
  fs.writeFileSync(`${backup}/host2.preflight.conf`, candidateInclude, { mode: 0o600 });
  const testMain = main.replace(include,
    [...siblings.map(name => `include /data/nginx/proxy_host/${name};`),
      `include ${backup}/host2.preflight.conf;`].join('\n'));
  fs.writeFileSync(`${backup}/nginx.test.conf`, testMain, { mode: 0o600 });
  testConfig = `/etc/nginx/panel-account-preflight-${crypto.randomUUID()}.conf`;
  fs.writeFileSync(testConfig, testMain, { mode: 0o600, flag: 'wx' });
  runNginx('-t', '-c', testConfig, '-g', 'error_log off;');
  fs.unlinkSync(testConfig);
  testConfig = undefined;

  check(JSON.stringify(await readHost()) === snapshot && hash(fs.readFileSync(active)) === hash(before),
    'CONCURRENT_NPM_CHANGE');
  if (!apply) {
    console.log('PANEL_TEST_ACCOUNT_CANDIDATE_VERIFIED', `username=${username}`, `backup=${backup}`);
    process.exit(0);
  }

  priorAccessExists = fs.existsSync(panelAccess);
  if (priorAccessExists) {
    const priorStat = fs.lstatSync(panelAccess);
    check(priorStat.isFile() && !priorStat.isSymbolicLink() && priorStat.uid === 0 &&
      (priorStat.mode & 0o022) === 0, 'UNSAFE_PANEL_ACCESS_FILE');
    fs.copyFileSync(panelAccess, `${backup}/panel.before.htpasswd`);
    fs.chmodSync(`${backup}/panel.before.htpasswd`, 0o600);
  }
  const stagedAccess = `/data/access/.panel-${crypto.randomUUID()}.tmp`;
  fs.writeFileSync(stagedAccess, nextAccess, { mode: sharedStat.mode & 0o777, flag: 'wx' });
  fs.renameSync(stagedAccess, panelAccess);
  accessReplaced = true;

  if (original.advanced_config !== nextAdvanced) {
    const changed = await Model.query().where('id', 2).where('modified_on', original.modified_on)
      .where('advanced_config', original.advanced_config).patch({ advanced_config: nextAdvanced });
    check(changed === 1, 'CONCURRENT_NPM_CHANGE');
    configCommitted = true;
    const latest = await readHost();
    check(latest.advanced_config === nextAdvanced &&
      JSON.stringify({ ...latest, modified_on: original.modified_on, advanced_config: original.advanced_config }) === snapshot,
      'CONCURRENT_NPM_CHANGE');
    appliedSnapshot = JSON.stringify(latest);
    check(hash(fs.readFileSync(active)) === hash(before), 'CONCURRENT_NPM_CONFIG_CHANGE');
    fs.renameSync(candidate, active);
    configReplaced = true;
  } else {
    check(hash(before) === hash(candidateBytes), 'EXISTING_CONFIG_MODEL_DRIFT');
    appliedSnapshot = snapshot;
  }
  runNginx('-t', '-g', 'error_log off;');
  runNginx('-s', 'reload');
  check(JSON.stringify(await readHost()) === appliedSnapshot &&
    hash(fs.readFileSync(active)) === hash(candidateBytes) &&
    hash(fs.readFileSync(panelAccess)) === hash(nextAccess), 'POST_RELOAD_CONCURRENT_CHANGE');
  console.log('PANEL_TEST_ACCOUNT_APPLIED', `username=${username}`, `backup=${backup}`,
    `access_sha256=${hash(nextAccess)}`);
  process.exit(0);
} catch (error) {
  if (testConfig && fs.existsSync(testConfig)) fs.unlinkSync(testConfig);
  try {
    if (configCommitted) {
      const latest = await readHost();
      check(appliedSnapshot && JSON.stringify(latest) === appliedSnapshot, 'PANEL_ACCOUNT_CONCURRENT_CHANGE_RECOVERY_REQUIRED');
      const restored = await Model.query().where('id', 2).where('modified_on', latest.modified_on)
        .where('advanced_config', nextAdvanced).patch({ advanced_config: original.advanced_config });
      check(restored === 1, 'PANEL_ACCOUNT_ROLLBACK_CONFLICT_RECOVERY_REQUIRED');
    }
    if (configReplaced) {
      fs.copyFileSync(`${backup}/host2.before.conf`, `${backup}/host2.rollback.conf`);
      fs.renameSync(`${backup}/host2.rollback.conf`, active);
    }
    if (accessReplaced) {
      if (priorAccessExists) {
        fs.copyFileSync(`${backup}/panel.before.htpasswd`, `${backup}/panel.rollback.htpasswd`);
        fs.renameSync(`${backup}/panel.rollback.htpasswd`, panelAccess);
      } else if (fs.existsSync(panelAccess)) {
        fs.unlinkSync(panelAccess);
      }
    }
    if (configCommitted || configReplaced || accessReplaced) {
      runNginx('-t', '-g', 'error_log off;');
      runNginx('-s', 'reload');
    }
  } catch {
    console.error('PANEL_ACCOUNT_RECOVERY_REQUIRED', `backup=${backup || 'none'}`);
    process.exit(1);
  }
  console.error('PANEL_TEST_ACCOUNT_FAILED', /^[A-Z_]+$/.test(error.message || '') ? error.message : 'CHECK_PRIVATE_CANDIDATE',
    `backup=${backup || 'none'}`);
  process.exit(1);
}
