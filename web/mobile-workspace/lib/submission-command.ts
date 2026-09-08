// Persist only identity and a digest. The message remains in the existing
// tab-scoped draft; another tab cannot silently retry content it does not own.
export type PendingSubmission = {
  commandId: string;
  dialogId: string;
  expectedVersion: number;
  payloadDigest: string;
};

// Match the public Gateway limits before persisting an unresolved command.
export function validSubmissionIntent(
  issueId: string,
  text: string,
  version: number,
): boolean {
  return (
    /^(HL-[1-9][0-9]*)?$/.test(issueId) &&
    issueId.length <= 64 &&
    !/^[\s\u0085]*$/.test(text) &&
    !text.includes('\u0000') &&
    new TextDecoder().decode(new TextEncoder().encode(text)) === text &&
    new TextEncoder().encode(text).length <= 2048 &&
    Number.isSafeInteger(version) &&
    version > 0 &&
    version < Number.MAX_SAFE_INTEGER &&
    new TextEncoder().encode(
      JSON.stringify({
        command_id: '00000000-0000-4000-8000-000000000000',
        expected_object_version: version,
        requested_issue_id: issueId,
        text,
      }),
    ).length <= 4096
  );
}

export function readSubmissionIssue(dialogId: string): string {
  try {
    return (
      sessionStorage.getItem(`fixik:task-submit-issue:${dialogId}`) ?? ''
    ).slice(0, 64);
  } catch {
    return '';
  }
}

export function writeSubmissionIssue(dialogId: string, issueId: string): void {
  if (issueId === '')
    sessionStorage.removeItem(`fixik:task-submit-issue:${dialogId}`);
  else sessionStorage.setItem(`fixik:task-submit-issue:${dialogId}`, issueId);
}

export function pendingSubmissionVersion(
  storage: Storage,
  clientId: string,
  dialogId: string,
): number | null {
  const raw = storage.getItem(`fixik:task-submit:${clientId}:${dialogId}`);
  if (raw === null) return null;
  const value = JSON.parse(raw) as PendingSubmission;
  if (
    value.dialogId !== dialogId ||
    !Number.isSafeInteger(value.expectedVersion) ||
    value.expectedVersion < 1 ||
    value.expectedVersion >= Number.MAX_SAFE_INTEGER
  )
    throw new Error('Invalid pending target');
  return value.expectedVersion;
}

export async function pendingSubmission(
  storage: Storage,
  clientId: string,
  dialogId: string,
  expectedVersion: number,
  issueId: string,
  text: string,
): Promise<PendingSubmission> {
  if (!validSubmissionIntent(issueId, text, expectedVersion))
    throw new Error('Invalid submission intent');
  const digest = await crypto.subtle.digest(
    'SHA-256',
    new TextEncoder().encode(JSON.stringify({ issueId, text })),
  );
  const payloadDigest = Array.from(new Uint8Array(digest), (byte) =>
    byte.toString(16).padStart(2, '0'),
  ).join('');
  const key = `fixik:task-submit:${clientId}:${dialogId}`;
  const raw = storage.getItem(key);
  if (raw !== null) {
    const command = JSON.parse(raw) as PendingSubmission;
    if (
      !command ||
      Object.keys(command).sort().join(',') !==
        'commandId,dialogId,expectedVersion,payloadDigest' ||
      !/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(
        command.commandId,
      ) ||
      command.dialogId !== dialogId ||
      !Number.isSafeInteger(command.expectedVersion) ||
      command.expectedVersion < 1 ||
      command.expectedVersion >= Number.MAX_SAFE_INTEGER ||
      command.payloadDigest !== payloadDigest
    )
      throw new Error('The unresolved command belongs to a different draft');
    return command;
  }
  if (
    !Number.isSafeInteger(expectedVersion) ||
    expectedVersion < 1 ||
    expectedVersion === Number.MAX_SAFE_INTEGER
  )
    throw new Error('Invalid target version');
  const command = {
    commandId: crypto.randomUUID(),
    dialogId,
    expectedVersion,
    payloadDigest,
  };
  const encoded = JSON.stringify(command);
  storage.setItem(key, encoded);
  if (storage.getItem(key) !== encoded)
    throw new Error('Command identity could not be saved');
  return command;
}

export function clearSubmission(
  storage: Storage,
  clientId: string,
  command: PendingSubmission,
): void {
  const key = `fixik:task-submit:${clientId}:${command.dialogId}`;
  const raw = storage.getItem(key);
  if (raw === null) return;
  const current = JSON.parse(raw) as PendingSubmission;
  if (
    current.commandId === command.commandId &&
    current.payloadDigest === command.payloadDigest &&
    current.expectedVersion === command.expectedVersion
  )
    storage.removeItem(key);
}
