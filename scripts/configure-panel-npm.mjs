// Run inside the existing NPM container as root, with node --input-type=module.
// Read-only preview unless the final argument is --apply. No credential access.
import fs from 'node:fs';
import { execFileSync } from 'node:child_process';
import crypto from 'node:crypto';
import Model from '/app/models/proxy_host.js';
import nginx from '/app/internal/nginx.js';

const active = '/data/nginx/proxy_host/2.conf';
const cert = '/data/nginx/custom/panel-backend.crt';
const access = '/data/access/1';
const marker = '# BEGIN managed Panel HL-241';
const markerEnd = '# END managed Panel HL-241';
const addition = `${marker}
location = /panel { return 308 https://h1-cloud.ru/panel/; }
location ^~ /panel/ {
    auth_basic "HomeLab Agent Panel";
    auth_basic_user_file ${access};
    proxy_pass https://10.202.2.52:18443;
    proxy_ssl_verify on;
    proxy_ssl_trusted_certificate ${cert};
    proxy_ssl_name panel-backend.homelab.internal;
    proxy_ssl_server_name on;
    proxy_set_header Host $http_host;
    proxy_set_header X-Forwarded-Proto https;
    proxy_set_header X-Panel-Authenticated-User $remote_user;
    proxy_connect_timeout 5s;
    proxy_read_timeout 65s;
    client_max_body_size 72k;
    proxy_redirect off;
    proxy_buffering off;
    access_log off;
}
${markerEnd}
`;
const readHost = () => Model.query().findById(2).withGraphFetched('certificate')
  .modifyGraph('certificate', qb => qb.select('id', 'provider'));
const check = (ok, message) => { if (!ok) throw new Error(message); };
const hash = value => crypto.createHash('sha256').update(value).digest('hex');
const run = (...args) => execFileSync('/usr/sbin/nginx', args, { stdio: 'pipe', timeout: 15000 });
let backup;
let replaced = false;
let committed = false;
let original;
let before;
let appliedSnapshot;
let candidateBytes;
let testConfig;
let nextAdvanced;
try {
  check(process.getuid() === 0, 'ROOT_REQUIRED');
  original = await readHost();
  check(original?.enabled && !original.is_deleted && original.access_list_id === 0 &&
    JSON.stringify(original.domain_names) === '["h1-cloud.ru"]' &&
    original.certificate_id === 7 && original.certificate?.provider === 'letsencrypt', 'TARGET_CHANGED');
  check(typeof original.advanced_config === 'string', 'TARGET_CHANGED');
  const matches = original.advanced_config.match(/# BEGIN managed Panel HL-241[\s\S]*?# END managed Panel HL-241\n?/g) || [];
  check(matches.length <= 1, 'PANEL_ROUTE_AMBIGUOUS');
  const base = original.advanced_config.replace(/# BEGIN managed Panel HL-241[\s\S]*?# END managed Panel HL-241\n?/g, '');
  check(!/location\s+[^\n{]*\/panel/.test(base), 'PANEL_ROUTE_ALREADY_EXISTS');
  nextAdvanced = base + (base && !base.endsWith('\n') ? '\n' : '') + addition;
  check(fs.statSync(cert).isFile() && !fs.lstatSync(cert).isSymbolicLink(), 'CERTIFICATE_NOT_PROVISIONED');
  check(fs.statSync(access).isFile() && !fs.lstatSync(access).isSymbolicLink(), 'ACCESS_LIST_NOT_PROVISIONED');
  before = fs.readFileSync(active);
  const snapshot = JSON.stringify(original);
  backup = fs.mkdtempSync('/data/nginx/panel-change-');
  fs.chmodSync(backup, 0o700);
  fs.writeFileSync(`${backup}/host2.before.conf`, before, { mode: 0o600 });
  fs.writeFileSync(`${backup}/host2.before.json`, snapshot, { mode: 0o600 });
  const candidate = `${backup}/host2.candidate.conf`;
  const next = { ...original, advanced_config: nextAdvanced };
  const getName = nginx.getConfigName;
  try {
    nginx.getConfigName = () => `${backup}/host2.rendered-original.conf`;
    await nginx.generateConfig('proxy_host', original);
    check(hash(fs.readFileSync(`${backup}/host2.rendered-original.conf`)) === hash(before), 'EXISTING_CONFIG_MODEL_DRIFT');
    nginx.getConfigName = () => candidate;
    await nginx.generateConfig('proxy_host', next);
  } finally { nginx.getConfigName = getName; }
  candidateBytes = fs.readFileSync(candidate);
  const main = fs.readFileSync('/etc/nginx/nginx.conf', 'utf8');
  const include = /include\s+\/data\/nginx\/proxy_host\/\*\.conf;/g;
  check([...main.matchAll(include)].length === 1, 'NGINX_INCLUDE_LAYOUT_CHANGED');
  const siblings = fs.readdirSync('/data/nginx/proxy_host').filter(n => n.endsWith('.conf') && n !== '2.conf');
  check(siblings.every(n => /^[0-9]+\.conf$/.test(n)), 'UNEXPECTED_HOST_FILENAME');
  const testMain = main.replace(include, [...siblings.map(n => `include /data/nginx/proxy_host/${n};`), `include ${candidate};`].join('\n'));
  fs.writeFileSync(`${backup}/nginx.test.conf`, testMain, { mode: 0o600 });
  // Keep the same config prefix: NPM uses relative conf.d/include directives.
  testConfig = `/etc/nginx/panel-preflight-${crypto.randomUUID()}.conf`;
  fs.writeFileSync(testConfig, testMain, { mode: 0o600, flag: 'wx' });
  run('-t', '-c', testConfig, '-g', 'error_log off;');
  fs.unlinkSync(testConfig);
  testConfig = undefined;
  check(JSON.stringify(await readHost()) === snapshot && hash(fs.readFileSync(active)) === hash(before), 'CONCURRENT_NPM_CHANGE');
  if (process.argv.includes('--apply')) {
    if (original.advanced_config === nextAdvanced) {
      check(hash(before) === hash(candidateBytes), 'EXISTING_CONFIG_MODEL_DRIFT');
      console.log('PANEL_NPM_ROUTE_CURRENT', 'backup=' + backup, 'sha256=' + hash(before));
    } else {
      // Compare all model inputs before the CAS; never replace locations/certificates.
      const changed = await Model.query().where('id', 2).where('modified_on', original.modified_on)
        .where('advanced_config', original.advanced_config).patch({ advanced_config: next.advanced_config });
      check(changed === 1, 'CONCURRENT_NPM_CHANGE');
      committed = true;
      const latest = await readHost();
      check(latest.advanced_config === next.advanced_config && JSON.stringify({ ...latest, modified_on: original.modified_on, advanced_config: original.advanced_config }) === snapshot,
        'CONCURRENT_NPM_CHANGE');
      appliedSnapshot = JSON.stringify(latest);
      check(hash(fs.readFileSync(active)) === hash(before), 'CONCURRENT_NPM_CONFIG_CHANGE');
      fs.renameSync(candidate, active);
      replaced = true;
      run('-t', '-g', 'error_log off;');
      run('-s', 'reload');
      check(JSON.stringify(await readHost()) === appliedSnapshot && hash(fs.readFileSync(active)) === hash(candidateBytes), 'POST_RELOAD_CONCURRENT_CHANGE');
      console.log('PANEL_NPM_ROUTE_APPLIED', 'backup=' + backup, 'original_sha256=' + hash(before));
    }
  } else {
    console.log('PANEL_NPM_CANDIDATE_VERIFIED', 'backup=' + backup, 'original_sha256=' + hash(before));
  }
  process.exit(0);
} catch (error) {
  if (testConfig) fs.unlinkSync(testConfig);
  if (committed) {
    const latest = await readHost();
    if (!appliedSnapshot || JSON.stringify(latest) !== appliedSnapshot ||
      hash(fs.readFileSync(active)) !== hash(replaced ? candidateBytes : before)) {
      console.error('PANEL_NPM_CONCURRENT_CHANGE_RECOVERY_REQUIRED', 'backup=' + backup);
      process.exit(1);
    }
    const restored = await Model.query().where('id', 2).where('modified_on', latest.modified_on)
      .where('advanced_config', nextAdvanced).patch({ advanced_config: original.advanced_config });
    if (restored !== 1) {
      console.error('PANEL_NPM_ROLLBACK_CONFLICT_RECOVERY_REQUIRED', 'backup=' + backup);
      process.exit(1);
    }
  }
  if (replaced) {
    const latest = await readHost();
    if (latest.advanced_config !== original.advanced_config ||
      JSON.stringify({ ...latest, modified_on: original.modified_on }) !== JSON.stringify(original) ||
      hash(fs.readFileSync(active)) !== hash(candidateBytes)) {
      console.error('PANEL_NPM_ROLLBACK_CONFIG_CONFLICT_RECOVERY_REQUIRED', 'backup=' + backup);
      process.exit(1);
    }
    fs.copyFileSync(`${backup}/host2.before.conf`, `${backup}/host2.rollback.conf`);
    fs.renameSync(`${backup}/host2.rollback.conf`, active);
    run('-t', '-g', 'error_log off;');
    run('-s', 'reload');
  }
  console.error('PANEL_NPM_UPDATE_FAILED', /^[A-Z_]+$/.test(error.message || '') ? error.message : 'CHECK_PRIVATE_CANDIDATE', 'backup=' + (backup || 'none'));
  // Do not log raw ORM/runtime objects or config. Retain private backup for inspection.
  process.exit(1);
}
