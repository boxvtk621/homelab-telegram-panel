import { api, APIError, type Session } from './panel-api';

export type EnrollmentHost = {
  hostId: string;
  hostVersion: number;
  displayName: string;
  transport: 'local' | 'ssh';
  availability: string;
};

type EnrollmentHostPage = {
  schemaId: 'external-harness-enrollment-host-page-v1';
  items: EnrollmentHost[];
  nextCursor: string | null;
};

export type EnrollmentInput = {
  schemaId: 'external-harness-enrollment-v1';
  operationId: string;
  hostId: string;
  expectedHostVersion: number;
  nodeId: string;
  name: string;
  adapter: 'cursor' | 'codex';
  endpointUri: string;
  certificateSHA256: string;
};

export type EnrollmentResult = {
  schemaId: 'external-harness-enrollment-result-v1';
  operationId: string;
  nodeId: string;
  status: 'ready';
  registrationRevision: number;
  identityEpoch: number;
};

const object = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value);
const exactKeys = (value: Record<string, unknown>, keys: string[]) =>
  Object.keys(value).length === keys.length &&
  keys.every((key) => Object.prototype.hasOwnProperty.call(value, key));
const uuid = (value: unknown): value is string =>
  typeof value === 'string' &&
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(
    value,
  );
const safeInteger = (value: unknown): value is number =>
  Number.isSafeInteger(value) && Number(value) > 0;

function validHost(value: unknown): value is EnrollmentHost {
  return (
    object(value) &&
    exactKeys(value, [
      'hostId',
      'hostVersion',
      'displayName',
      'transport',
      'availability',
    ]) &&
    uuid(value.hostId) &&
    safeInteger(value.hostVersion) &&
    typeof value.displayName === 'string' &&
    value.displayName.length > 0 &&
    value.displayName.length <= 200 &&
    (value.transport === 'local' || value.transport === 'ssh') &&
    typeof value.availability === 'string' &&
    value.availability.length > 0 &&
    value.availability.length <= 64
  );
}

function validHostPage(value: unknown): value is EnrollmentHostPage {
  return (
    object(value) &&
    exactKeys(value, ['schemaId', 'items', 'nextCursor']) &&
    value.schemaId === 'external-harness-enrollment-host-page-v1' &&
    Array.isArray(value.items) &&
    value.items.length <= 100 &&
    value.items.every(validHost) &&
    (value.nextCursor === null ||
      (typeof value.nextCursor === 'string' &&
        value.nextCursor.length > 0 &&
        value.nextCursor.length <= 512))
  );
}

function validResult(
  value: unknown,
  operationId: string,
  nodeId: string,
): value is EnrollmentResult {
  return (
    object(value) &&
    exactKeys(value, [
      'schemaId',
      'operationId',
      'nodeId',
      'status',
      'registrationRevision',
      'identityEpoch',
    ]) &&
    value.schemaId === 'external-harness-enrollment-result-v1' &&
    value.operationId === operationId &&
    value.nodeId === nodeId &&
    value.status === 'ready' &&
    safeInteger(value.registrationRevision) &&
    safeInteger(value.identityEpoch)
  );
}

export const externalEnrollmentAPI = {
  async hosts(signal?: AbortSignal): Promise<EnrollmentHost[]> {
    const items: EnrollmentHost[] = [];
    let cursor: string | null = null;
    for (let pageNumber = 0; pageNumber < 10; pageNumber += 1) {
      const query = new URLSearchParams({ limit: '100' });
      if (cursor) query.set('cursor', cursor);
      const page = await api<unknown>(
        `external-enrollment-hosts?${query.toString()}`,
        { signal },
      );
      if (!validHostPage(page)) throw new APIError(200, 'invalid_response');
      items.push(...page.items);
      cursor = page.nextCursor;
      if (cursor === null) return items;
    }
    throw new APIError(200, 'invalid_response');
  },

  async enroll(
    session: Session,
    input: EnrollmentInput,
    signal?: AbortSignal,
  ): Promise<EnrollmentResult> {
    const result = await api<unknown>('external-enrollments', {
      body: input,
      csrf: session.csrf,
      signal,
    });
    if (!validResult(result, input.operationId, input.nodeId))
      throw new APIError(200, 'invalid_response');
    return result;
  },
};

export function enrollmentMessage(cause: unknown): string {
  if (!(cause instanceof APIError))
    return 'Не удалось проверить подключение Harness.';
  switch (cause.code) {
    case 'admission_profile_missing':
      return 'Harness не публикует обязательный admission profile.';
    case 'admission_schema_mismatch':
      return 'Harness использует несовместимую версию admission profile.';
    case 'admission_owner_mismatch':
      return 'Harness принадлежит другому владельцу.';
    case 'admission_identity_mismatch':
      return 'Identity Harness не совпала с зарегистрированным узлом.';
    case 'admission_capability_missing':
      return 'Harness не поддерживает все обязательные возможности переноса и владения.';
    case 'admission_unready':
    case 'node_not_ready':
      return 'Harness доступен, но ещё не готов к активации.';
    case 'endpoint_rejected':
      return 'URI или защищённый transport не прошёл проверку.';
    case 'host_version_conflict':
      return 'Параметры выбранного хоста изменились. Обновите список и начните новую операцию.';
    case 'registry_projection_required':
      return 'Сначала подтвердите текущую Router-проекцию реестра Harness.';
    case 'tunnel_not_configured':
      return 'Для SSH-хоста не настроен закрытый transport Panel.';
    case 'enrollment_conflict':
    case 'registry_operation_conflict':
    case 'tunnel_binding_conflict':
      return 'Параметры подключения изменились. Закройте форму и начните новую операцию.';
    case 'enrollment_reconciliation_required':
      return 'Результат операции пока не подтверждён. Повторите с теми же данными.';
    case 'authentication_required':
      return 'Сессия Panel завершена.';
    default:
      return 'Не удалось проверить подключение Harness.';
  }
}
