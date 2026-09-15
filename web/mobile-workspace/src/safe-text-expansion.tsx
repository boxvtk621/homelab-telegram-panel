import { useEffect, useRef, useState } from 'react';
import { harnessAPI, HarnessAPIError } from './harness-api';
import type { Session } from './panel-api';
import { SafeMarkdown } from './safe-markdown';
import type {
  TranscriptManifest,
  TranscriptSource,
} from './transcript-view.ts';

type Props = {
  session: Session;
  nodeId: string;
  dialogId: string;
  attemptId: string;
  source: TranscriptSource;
  preview: string;
  redaction: 'none' | 'applied';
  truncated: boolean;
  renderMarkdown?: boolean;
  onExpired: () => void;
};

function hex(bytes: ArrayBuffer): string {
  return Array.from(new Uint8Array(bytes), (value) =>
    value.toString(16).padStart(2, '0'),
  ).join('');
}

function safeFailure(error: unknown): string {
  return error instanceof HarnessAPIError
    ? error.message
    : 'Не удалось проверить полный текст.';
}

export function SafeTextExpansion(props: Props) {
  if (!props.truncated) return null;
  const { source } = props;
  const identity = [
    props.nodeId,
    props.dialogId,
    props.attemptId,
    source.kind,
    source.id,
    source.index,
    source.stream,
    props.redaction,
    props.preview,
  ].join('\u0000');
  return <ResolvedSafeTextExpansion key={identity} {...props} />;
}

function ResolvedSafeTextExpansion({
  session,
  nodeId,
  dialogId,
  attemptId,
  source,
  preview,
  redaction,
  renderMarkdown = false,
  onExpired,
}: Props) {
  const sourceKind = source.kind;
  const sourceId = source.id;
  const sourceIndex = source.index;
  const sourceStream = source.stream;
  const [manifest, setManifest] = useState<TranscriptManifest | null>(null);
  const [phase, setPhase] = useState<
    'resolving' | 'missing' | 'ready' | 'loading' | 'shown' | 'error'
  >('resolving');
  const [fullText, setFullText] = useState('');
  const [error, setError] = useState('');
  const loadAbort = useRef<AbortController | null>(null);

  useEffect(() => {
    const abort = new AbortController();
    loadAbort.current = abort;
    const requestedSource = {
      kind: sourceKind,
      id: sourceId,
      index: sourceIndex,
      stream: sourceStream,
    } as TranscriptSource;
    void harnessAPI
      .safeTextManifest(
        session,
        nodeId,
        dialogId,
        attemptId,
        requestedSource,
        abort.signal,
      )
      .then((value) => {
        if (abort.signal.aborted) return;
        if (
          value.preview !== preview ||
          !value.previewTruncated ||
          value.redaction !== redaction
        ) {
          throw new HarnessAPIError(
            200,
            'transcript_preview_mismatch',
            'Описание полного текста не совпадает с показанным результатом.',
            false,
            'unknown',
          );
        }
        setManifest(value);
        setPhase('ready');
      })
      .catch((cause) => {
        if (abort.signal.aborted) return;
        if (cause instanceof HarnessAPIError && cause.status === 401) {
          onExpired();
          return;
        }
        if (
          cause instanceof HarnessAPIError &&
          cause.status === 404 &&
          cause.code === 'not_found'
        ) {
          setPhase('missing');
          return;
        }
        setError(safeFailure(cause));
        setPhase('error');
      });
    return () => abort.abort();
  }, [
    attemptId,
    dialogId,
    nodeId,
    onExpired,
    preview,
    redaction,
    session,
    sourceId,
    sourceIndex,
    sourceKind,
    sourceStream,
  ]);

  useEffect(
    () => () => {
      loadAbort.current?.abort();
    },
    [],
  );

  async function reveal() {
    if (!manifest?.complete || phase === 'loading') return;
    const abort = new AbortController();
    loadAbort.current?.abort();
    loadAbort.current = abort;
    setPhase('loading');
    setError('');
    try {
      const output = new Uint8Array(manifest.sizeBytes);
      for (const chunk of manifest.chunks) {
        const metadata = await harnessAPI.artifactMetadata(
          session,
          nodeId,
          chunk.artifactId,
          abort.signal,
        );
        const expectedCallId =
          sourceKind === 'assistant_message' ? undefined : sourceId;
        if (
          metadata.nodeId !== nodeId ||
          metadata.dialogId !== dialogId ||
          metadata.attemptId !== attemptId ||
          metadata.artifactId !== chunk.artifactId ||
          metadata.callId !== expectedCallId ||
          metadata.name !==
            `safe-text-${String(chunk.index).padStart(2, '0')}.txt` ||
          metadata.mediaType !== 'text/plain; charset=utf-8' ||
          metadata.sizeBytes !== chunk.sizeBytes ||
          metadata.sha256 !== chunk.sha256 ||
          metadata.redaction !== manifest.redaction ||
          metadata.truncated ||
          metadata.disposition !== 'attachment'
        ) {
          throw new HarnessAPIError(
            200,
            'transcript_chunk_scope_mismatch',
            'Фрагмент полного текста относится к другому источнику.',
            false,
            'unknown',
          );
        }
        const binary = await harnessAPI.artifact(
          session,
          nodeId,
          chunk.artifactId,
          abort.signal,
        );
        const chunkHash = hex(
          await crypto.subtle.digest('SHA-256', binary.bytes),
        );
        if (
          binary.bytes.byteLength !== chunk.sizeBytes ||
          chunkHash !== chunk.sha256 ||
          binary.mediaType !== 'application/octet-stream' ||
          !/^attachment(?:;|$)/i.test(binary.disposition)
        ) {
          throw new HarnessAPIError(
            200,
            'transcript_chunk_integrity_mismatch',
            'Целостность фрагмента полного текста не подтверждена.',
            false,
            'unknown',
          );
        }
        output.set(new Uint8Array(binary.bytes), chunk.offsetBytes);
      }
      const overallHash = hex(
        await crypto.subtle.digest('SHA-256', output.buffer),
      );
      if (overallHash !== manifest.sha256) {
        throw new HarnessAPIError(
          200,
          'transcript_integrity_mismatch',
          'Целостность полного текста не подтверждена.',
          false,
          'unknown',
        );
      }
      const text = new TextDecoder('utf-8', { fatal: true }).decode(output);
      if (!text.startsWith(manifest.preview)) {
        throw new HarnessAPIError(
          200,
          'transcript_preview_mismatch',
          'Полный текст не продолжает показанный фрагмент.',
          false,
          'unknown',
        );
      }
      setFullText(text);
      setPhase('shown');
    } catch (cause) {
      if (abort.signal.aborted) return;
      if (cause instanceof HarnessAPIError && cause.status === 401) {
        onExpired();
        return;
      }
      setError(safeFailure(cause));
      setPhase('error');
    }
  }

  if (phase === 'resolving')
    return <p className="muted safe-text-status">Проверяем полный текст…</p>;
  if (phase === 'missing')
    return (
      <p className="muted safe-text-status">
        Полный текст этого результата не сохранён.
      </p>
    );
  if (manifest && !manifest.complete)
    return (
      <p className="notice safe-text-status">
        Полный текст не сохранён: превышен допустимый объём вывода.
      </p>
    );
  return (
    <div className="safe-text-expansion">
      {manifest?.complete && phase !== 'shown' && (
        <button type="button" onClick={reveal} disabled={phase === 'loading'}>
          {phase === 'loading'
            ? 'Проверяем полный текст…'
            : `Показать полный текст (${manifest.sizeBytes} байт)`}
        </button>
      )}
      {phase === 'shown' &&
        (renderMarkdown ? (
          <div className="safe-text-full">
            <SafeMarkdown markdown={fullText} />
          </div>
        ) : (
          <pre className="safe-text-full">{fullText}</pre>
        ))}
      {error && <p className="notice error safe-text-status">{error}</p>}
    </div>
  );
}
