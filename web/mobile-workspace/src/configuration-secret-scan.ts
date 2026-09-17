import type { ConfigurationDiagnostic } from './configuration-api';

const keyPattern =
  /(?:^|[,{\s])["']?(token|access[_-]?token|api[_-]?key|password|passphrase|private[_-]?key|credential|credentials|authorization|proxy[_-]?authorization|x[_-]?api[_-]?key)["']?\s*:/gim;
const contentPatterns = [
  /-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----/g,
  /\b(?:ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[baprs]-[A-Za-z0-9-]{16,}|sk-[A-Za-z0-9]{20,})\b/g,
];
const dockerPatterns = [
  /^\s*(?:ARG|ENV)\s+(?:TOKEN|ACCESS_TOKEN|API_KEY|PRIVATE_KEY|PASSWORD|PASSPHRASE|AUTHORIZATION|X_API_KEY)(?:\s|=|$)/gim,
  /(?:Authorization|Proxy-Authorization|X-Api-Key|Api-Key)\s*:/gi,
  ...contentPatterns,
];
const forbiddenKeys = new Set([
  'authorization',
  'proxyauthorization',
  'xapikey',
  'apikey',
  'token',
  'accesstoken',
  'privatekey',
  'password',
  'passphrase',
  'credential',
  'credentials',
]);

const normalizeKey = (value: string) =>
  value.toLocaleLowerCase('en-US').replace(/[_\- ]/g, '');
const pointerPart = (value: string) => value.replaceAll('~', '~0').replaceAll('/', '~1');

function location(text: string, offset: number) {
  const before = text.slice(0, offset).split('\n');
  return { line: before.length, column: before.at(-1)!.length + 1 };
}

function scan(
  text: string,
  pointer: string,
  patterns: RegExp[],
): ConfigurationDiagnostic[] {
  const diagnostics: ConfigurationDiagnostic[] = [];
  for (const source of patterns) {
    const pattern = new RegExp(source.source, source.flags);
    for (const match of text.matchAll(pattern)) {
      const at = location(text, match.index ?? 0);
      diagnostics.push({
        code: 'secret_content',
        pointer,
        ...at,
        message: 'Secret-like content is not allowed; use credentialRef.',
      });
    }
  }
  return diagnostics;
}

function scanDecodedJSON(rawJsonText: string): ConfigurationDiagnostic[] {
  const diagnostics: ConfigurationDiagnostic[] = [];
  const visit = (value: unknown, pointer: string) => {
    if (Array.isArray(value)) {
      value.forEach((child, index) => visit(child, `${pointer}/${index}`));
      return;
    }
    if (typeof value === 'object' && value !== null) {
      for (const [key, child] of Object.entries(value)) {
        const childPointer = `${pointer}/${pointerPart(key)}`;
        if (forbiddenKeys.has(normalizeKey(key)))
          diagnostics.push({
            code: 'secret_field',
            pointer: childPointer,
            line: 1,
            column: 1,
            message: 'Inline secret field is not allowed; use credentialRef.',
          });
        visit(child, childPointer);
      }
      return;
    }
    if (
      typeof value === 'string' &&
      contentPatterns.some((pattern) =>
        new RegExp(pattern.source, pattern.flags).test(value),
      )
    )
      diagnostics.push({
        code: 'secret_content',
        pointer,
        line: 1,
        column: 1,
        message: 'Secret-like content is not allowed; use credentialRef.',
      });
  };
  try {
    visit(JSON.parse(rawJsonText), '');
  } catch {
    // Raw scanning below remains the bounded best-effort path for malformed JSON.
  }
  return diagnostics;
}

// Decode only individually completed JSON string tokens. This intentionally does
// not attempt to repair or parse the surrounding malformed document.
function scanDecodedStringTokens(rawJsonText: string): ConfigurationDiagnostic[] {
  const diagnostics: ConfigurationDiagnostic[] = [];
  for (let start = 0; start < rawJsonText.length; start++) {
    if (rawJsonText[start] !== '"') continue;
    let escaped = false;
    let end = start + 1;
    for (; end < rawJsonText.length; end++) {
      const character = rawJsonText[end];
      if (escaped) {
        escaped = false;
      } else if (character === '\\') {
        escaped = true;
      } else if (character === '"') {
        break;
      }
    }
    if (end >= rawJsonText.length) break;
    let decoded: unknown;
    try {
      decoded = JSON.parse(rawJsonText.slice(start, end + 1));
    } catch {
      start = end;
      continue;
    }
    if (typeof decoded !== 'string') {
      start = end;
      continue;
    }
    let next = end + 1;
    while (next < rawJsonText.length && /\s/.test(rawJsonText[next])) next++;
    if (
      rawJsonText[next] === ':' &&
      forbiddenKeys.has(normalizeKey(decoded))
    ) {
      diagnostics.push({
        code: 'secret_field',
        pointer: `/${pointerPart(normalizeKey(decoded))}`,
        ...location(rawJsonText, start),
        message: 'Inline secret field is not allowed; use credentialRef.',
      });
    } else if (
      contentPatterns.some((pattern) =>
        new RegExp(pattern.source, pattern.flags).test(decoded),
      )
    ) {
      diagnostics.push({
        code: 'secret_content',
        pointer: '',
        ...location(rawJsonText, start),
        message: 'Secret-like content is not allowed; use credentialRef.',
      });
    }
    start = end;
  }
  return diagnostics;
}

export function scanConfigurationSecrets(
  rawJsonText: string,
  rawDockerfileText: string,
): ConfigurationDiagnostic[] {
  return [
    ...scan(rawJsonText, '', [keyPattern, ...contentPatterns]),
    ...scanDecodedStringTokens(rawJsonText),
    ...scanDecodedJSON(rawJsonText),
    ...scan(rawDockerfileText, '/dockerfile', dockerPatterns),
  ];
}
