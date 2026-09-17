import { useEffect, useMemo, useState, type SyntheticEvent } from 'react';
import {
  enrollmentMessage,
  externalEnrollmentAPI,
  type EnrollmentHost,
  type EnrollmentInput,
} from './external-enrollment-api';
import { APIError, type Session } from './panel-api';

type Props = {
  open: boolean;
  session: Session;
  onClose: () => void;
  onSuccess: () => void;
  onExpired: () => void;
};

const newOperationID = () => crypto.randomUUID().toLowerCase();

export function ExternalEnrollment({
  open,
  session,
  onClose,
  onSuccess,
  onExpired,
}: Props) {
  if (!open) return null;
  return (
    <ExternalEnrollmentDialog
      session={session}
      onClose={onClose}
      onSuccess={onSuccess}
      onExpired={onExpired}
    />
  );
}

function ExternalEnrollmentDialog({
  session,
  onClose,
  onSuccess,
  onExpired,
}: Omit<Props, 'open'>) {
  const [hosts, setHosts] = useState<EnrollmentHost[]>([]);
  const [hostID, setHostID] = useState('');
  const [nodeID, setNodeID] = useState('');
  const [name, setName] = useState('');
  const [adapter, setAdapter] = useState<'cursor' | 'codex'>('codex');
  const [endpointURI, setEndpointURI] = useState('');
  const [certificateSHA256, setCertificateSHA256] = useState('');
  const [operationID] = useState(newOperationID);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const selectedHost = useMemo(
    () => hosts.find((host) => host.hostId === hostID),
    [hostID, hosts],
  );

  useEffect(() => {
    const abort = new AbortController();
    externalEnrollmentAPI
      .hosts(abort.signal)
      .then((items) => {
        if (abort.signal.aborted) return;
        setHosts(items);
        setHostID(
          items.find((host) => host.availability === 'ready')?.hostId ??
            items[0]?.hostId ??
            '',
        );
      })
      .catch((cause) => {
        if (abort.signal.aborted) return;
        if (cause instanceof APIError && cause.status === 401) onExpired();
        else setError(enrollmentMessage(cause));
      })
      .finally(() => {
        if (!abort.signal.aborted) setLoading(false);
      });
    return () => abort.abort();
  }, [onExpired]);

  const submit = async (event: SyntheticEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!selectedHost || !operationID) return;
    const input: EnrollmentInput = {
      schemaId: 'external-harness-enrollment-v1',
      operationId: operationID,
      hostId: selectedHost.hostId,
      expectedHostVersion: selectedHost.hostVersion,
      nodeId: nodeID.trim(),
      name: name.trim(),
      adapter,
      endpointUri: endpointURI.trim(),
      certificateSHA256: certificateSHA256.trim().toLowerCase(),
    };
    setBusy(true);
    setError('');
    try {
      await externalEnrollmentAPI.enroll(session, input);
      onSuccess();
    } catch (cause) {
      if (cause instanceof APIError && cause.status === 401) onExpired();
      else setError(enrollmentMessage(cause));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="enrollment-backdrop" role="presentation">
      <dialog
        open
        className="enrollment-dialog"
        aria-modal="true"
        aria-labelledby="external-enrollment-title"
        onCancel={(event) => {
          event.preventDefault();
          if (!busy) onClose();
        }}
      >
        <header>
          <div>
            <span className="eyebrow">Существующий Harness</span>
            <h2 id="external-enrollment-title">
              Подключить по защищённому URI
            </h2>
          </div>
          <button
            type="button"
            className="secondary"
            onClick={onClose}
            disabled={busy}
          >
            Закрыть
          </button>
        </header>
        <p className="muted">
          Panel проверит mTLS identity, владельца, версию и обязательные
          возможности. Процесс Harness не создаётся и не перезапускается.
        </p>
        <form
          className="enrollment-form"
          onSubmit={(event) => void submit(event)}
        >
          <label>
            Хост transport
            <select
              value={hostID}
              onChange={(event) => setHostID(event.target.value)}
              disabled={loading || busy}
              required
            >
              {hosts.map((host) => (
                <option key={host.hostId} value={host.hostId}>
                  {host.displayName} · {host.transport} · {host.availability}
                </option>
              ))}
            </select>
          </label>
          <label>
            Имя Harness
            <input
              value={name}
              onChange={(event) => setName(event.target.value)}
              maxLength={200}
              autoComplete="off"
              required
            />
          </label>
          <label>
            Движок
            <select
              value={adapter}
              onChange={(event) =>
                setAdapter(event.target.value as 'cursor' | 'codex')
              }
              disabled={busy}
            >
              <option value="codex">Codex</option>
              <option value="cursor">Cursor</option>
            </select>
          </label>
          <label>
            Node ID
            <input
              value={nodeID}
              onChange={(event) => setNodeID(event.target.value)}
              placeholder="20000000-0000-4000-8000-000000000001"
              pattern="[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}"
              autoComplete="off"
              spellCheck={false}
              required
            />
          </label>
          <label className="enrollment-wide-field">
            HTTPS URI
            <input
              aria-label="HTTPS URI"
              value={endpointURI}
              onChange={(event) => setEndpointURI(event.target.value)}
              placeholder={
                selectedHost?.transport === 'ssh'
                  ? 'https://127.0.0.1:9443'
                  : 'https://10.20.30.40:9443'
              }
              autoComplete="off"
              spellCheck={false}
              required
            />
            <small>
              {selectedHost?.transport === 'ssh'
                ? 'Для SSH укажите loopback удалённого хоста.'
                : 'Только канонический private IP с явным портом.'}
            </small>
          </label>
          <label className="enrollment-wide-field">
            SHA-256 сертификата Harness
            <input
              value={certificateSHA256}
              onChange={(event) => setCertificateSHA256(event.target.value)}
              minLength={64}
              maxLength={64}
              pattern="[0-9A-Fa-f]{64}"
              autoComplete="off"
              spellCheck={false}
              required
            />
          </label>
          {loading && (
            <output className="muted">Загружаем доступные transports…</output>
          )}
          {!loading && hosts.length === 0 && !error && (
            <p className="notice error">Нет доступных local/SSH transports.</p>
          )}
          {error && (
            <p className="notice error" role="alert">
              {error}
            </p>
          )}
          <footer>
            <button
              type="button"
              className="secondary"
              onClick={onClose}
              disabled={busy}
            >
              Отмена
            </button>
            <button
              type="submit"
              className="primary"
              disabled={busy || loading || !selectedHost}
            >
              {busy ? 'Проверяем…' : 'Проверить и подключить'}
            </button>
          </footer>
        </form>
      </dialog>
    </div>
  );
}
