import { api, APIError, type Session } from './panel-api';

export type ConfigurationDiagnostic = {
  code: string;
  pointer: string;
  line: number;
  column: number;
  message: string;
};
export type ConfigurationValidation = {
  valid: boolean;
  diagnostics: ConfigurationDiagnostic[];
  effectStatus: 'none';
};
export type ConfigurationDraft = {
  schemaId: 'agent-configuration-draft-v2';
  nodeId: string;
  draftVersion: number;
  rawJsonText: string;
  rawDockerfileText: string;
  buildContextManifest?: BuildContextManifest;
  validation: ConfigurationValidation;
};
export type ConfigurationValidationResponse = {
  schemaId: 'agent-configuration-validate-v2';
  buildContextManifest?: BuildContextManifest;
  validation: ConfigurationValidation;
};
export type BuildContextManifest = {
  revision: string;
  assets: { path: string; assetId: string; sha256: string }[];
};

const uuid =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const sha256 = /^[0-9a-f]{64}$/;
const safePath =
  /^(?!\/)(?!.*\/\/)(?!.*(?:^|\/)\.{1,2}(?:\/|$))[A-Za-z0-9._-]+(?:\/[A-Za-z0-9._-]+)*$/;

export function validBuildContextManifest(
  value: unknown,
): value is BuildContextManifest {
  if (typeof value !== 'object' || value === null) return false;
  const manifest = value as BuildContextManifest;
  if (
    typeof manifest.revision !== 'string' ||
    !uuid.test(manifest.revision) ||
    !Array.isArray(manifest.assets) ||
    manifest.assets.length > 1000 ||
    Object.keys(value).sort().join(',') !== 'assets,revision'
  )
    return false;
  const paths = new Set<string>();
  return manifest.assets.every((asset) => {
    if (
      typeof asset !== 'object' ||
      asset === null ||
      Object.keys(asset).sort().join(',') !== 'assetId,path,sha256' ||
      typeof asset.path !== 'string' ||
      typeof asset.assetId !== 'string' ||
      typeof asset.sha256 !== 'string' ||
      !safePath.test(asset.path) ||
      new TextEncoder().encode(asset.path).length > 256 ||
      !uuid.test(asset.assetId) ||
      !sha256.test(asset.sha256) ||
      paths.has(asset.path) ||
      asset.path.split('/').some((segment) => segment === '.' || segment === '..')
    )
      return false;
    paths.add(asset.path);
    return true;
  });
}

const validValidation = (value: ConfigurationValidation) =>
  value.effectStatus === 'none' &&
  Array.isArray(value.diagnostics) &&
  value.valid === (value.diagnostics.length === 0) &&
  value.diagnostics.every(
    (item) =>
      typeof item.code === 'string' &&
      typeof item.pointer === 'string' &&
      Number.isInteger(item.line) &&
      item.line >= 1 &&
      Number.isInteger(item.column) &&
      item.column >= 1 &&
      typeof item.message === 'string',
  );
function checkedDraft(
  value: ConfigurationDraft,
  nodeId?: string,
): ConfigurationDraft {
  if (
    value.schemaId !== 'agent-configuration-draft-v2' ||
    !uuid.test(value.nodeId) ||
    (nodeId !== undefined && value.nodeId !== nodeId) ||
    !Number.isSafeInteger(value.draftVersion) ||
    value.draftVersion < 1 ||
    typeof value.rawJsonText !== 'string' ||
    new TextEncoder().encode(value.rawJsonText).length > 256 * 1024 ||
    typeof value.rawDockerfileText !== 'string' ||
    new TextEncoder().encode(value.rawDockerfileText).length > 128 * 1024 ||
    (value.buildContextManifest !== undefined &&
      !validBuildContextManifest(value.buildContextManifest)) ||
    !validValidation(value.validation)
  )
    throw new APIError(200, 'invalid_configuration_response');
  return value;
}

export const configurationAPI = {
  async read(nodeId: string, signal?: AbortSignal) {
    return checkedDraft(
      await api<ConfigurationDraft>(`configuration-drafts/${nodeId}`, {
        signal,
      }),
      nodeId,
    );
  },
  async validate(
    session: Session,
    rawJsonText: string,
    rawDockerfileText: string,
    buildContextManifest?: BuildContextManifest,
    signal?: AbortSignal,
  ) {
    const value = await api<ConfigurationValidationResponse>(
      'configuration-drafts/validate',
      {
        csrf: session.csrf,
        body: {
          schemaId: 'agent-configuration-validate-v2',
          rawJsonText,
          rawDockerfileText,
          buildContextManifest,
        },
        signal,
      },
    );
    if (
      value.schemaId !== 'agent-configuration-validate-v2' ||
      (value.buildContextManifest !== undefined &&
        !validBuildContextManifest(value.buildContextManifest)) ||
      !validValidation(value.validation)
    )
      throw new APIError(200, 'invalid_configuration_response');
    return value;
  },
  async save(
    session: Session,
    nodeId: string,
    expectedDraftVersion: number,
    rawJsonText: string,
    rawDockerfileText: string,
    buildContextManifest?: BuildContextManifest,
    signal?: AbortSignal,
  ) {
    return checkedDraft(
      await api<ConfigurationDraft>(`configuration-drafts/${nodeId}`, {
        csrf: session.csrf,
        body: {
          schemaId: 'agent-configuration-save-v2',
          expectedDraftVersion,
          rawJsonText,
          rawDockerfileText,
          buildContextManifest,
        },
        signal,
      }),
      nodeId,
    );
  },
  conflict(cause: unknown, nodeId: string): ConfigurationDraft | undefined {
    if (
      !(cause instanceof APIError) ||
      cause.status !== 409 ||
      typeof cause.payload !== 'object' ||
      cause.payload === null
    )
      return;
    const current = (cause.payload as { current?: ConfigurationDraft }).current;
    try {
      return current ? checkedDraft(current, nodeId) : undefined;
    } catch {
      return;
    }
  },
};
