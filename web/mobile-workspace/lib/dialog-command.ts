// Only non-secret command identity is persisted, never session/CSRF tokens.
// An uncertain outcome must reuse this exact command even after WebView reload.
export type PendingDialogCommand = {
  commandId: string;
  expectedCollectionVersion: number;
};

export function pendingDialogCommand(
  storage: Storage,
  clientID: string,
  version: number,
): PendingDialogCommand {
  const key = `fixik:dialog-create:${clientID}`;
  const previous = storage.getItem(key);
  if (previous !== null) {
    const value = JSON.parse(previous) as PendingDialogCommand;
    if (
      !value ||
      Object.keys(value).sort().join(',') !==
        'commandId,expectedCollectionVersion' ||
      !/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(
        value.commandId,
      ) ||
      !Number.isSafeInteger(value.expectedCollectionVersion) ||
      value.expectedCollectionVersion < 0
    ) {
      throw new Error('pending command identity is invalid');
    }
    return value;
  }
  if (
    !Number.isSafeInteger(version) ||
    version < 0 ||
    version === Number.MAX_SAFE_INTEGER
  ) {
    throw new Error('collection version is invalid');
  }
  const command = {
    commandId: crypto.randomUUID(),
    expectedCollectionVersion: version,
  };
  const encoded = JSON.stringify(command);
  storage.setItem(key, encoded);
  if (storage.getItem(key) !== encoded) {
    throw new Error('command identity could not be saved');
  }
  return command;
}

export function clearDialogCommand(
  storage: Storage,
  clientID: string,
  command: PendingDialogCommand,
): void {
  const key = `fixik:dialog-create:${clientID}`;
  const raw = storage.getItem(key);
  if (raw === null) {
    return;
  }
  const stored = JSON.parse(raw) as PendingDialogCommand;
  if (
    stored.commandId === command.commandId &&
    stored.expectedCollectionVersion === command.expectedCollectionVersion
  ) {
    storage.removeItem(key);
  }
}
