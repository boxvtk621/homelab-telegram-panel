import { useEffect, useRef, useState } from 'react';
import { harnessAPI, HarnessAPIError } from './harness-api';
import { IncrementalSHA256 } from './incremental-sha256';
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

const maximumInlineRenderBytes = 2 * 1024 * 1024;

type VerifiedPage = {
  index: number;
  text: string;
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
  const [page, setPage] = useState<VerifiedPage | null>(null);
  const [pageLoading, setPageLoading] = useState(false);
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
      const inline = manifest.sizeBytes <= maximumInlineRenderBytes;
      const hasher = new IncrementalSHA256();
      const parts: string[] = [];
      let firstPage = '';
      for (const chunk of manifest.chunks) {
        const buffer = await harnessAPI.safeTextChunk(
          session,
          nodeId,
          dialogId,
          attemptId,
          manifest.textId,
          manifest.source,
          chunk,
          abort.signal,
        );
        const bytes = new Uint8Array(buffer);
        const chunkHash = hex(await crypto.subtle.digest('SHA-256', buffer));
        if (
          bytes.byteLength !== chunk.sizeBytes ||
          chunkHash !== chunk.sha256
        ) {
          throw new HarnessAPIError(
            200,
            'transcript_chunk_integrity_mismatch',
            'Целостность фрагмента полного текста не подтверждена.',
            false,
            'unknown',
          );
        }
        hasher.update(bytes);
        const decoded = new TextDecoder('utf-8', { fatal: true }).decode(bytes);
        if (inline) parts.push(decoded);
        else if (chunk.index === 0) firstPage = decoded;
      }
      const overallHash = hasher.digestHex();
      if (overallHash !== manifest.sha256) {
        throw new HarnessAPIError(
          200,
          'transcript_integrity_mismatch',
          'Целостность полного текста не подтверждена.',
          false,
          'unknown',
        );
      }
      const text = inline ? parts.join('') : firstPage;
      if (!text.startsWith(manifest.preview)) {
        throw new HarnessAPIError(
          200,
          'transcript_preview_mismatch',
          'Полный текст не продолжает показанный фрагмент.',
          false,
          'unknown',
        );
      }
      if (inline) setFullText(text);
      else setPage({ index: 0, text });
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

  async function showPage(index: number) {
    if (
      !manifest?.complete ||
      pageLoading ||
      index < 0 ||
      index >= manifest.chunks.length
    )
      return;
    const abort = new AbortController();
    loadAbort.current?.abort();
    loadAbort.current = abort;
    setPageLoading(true);
    setError('');
    try {
      const chunk = manifest.chunks[index];
      const buffer = await harnessAPI.safeTextChunk(
        session,
        nodeId,
        dialogId,
        attemptId,
        manifest.textId,
        manifest.source,
        chunk,
        abort.signal,
      );
      const hash = hex(await crypto.subtle.digest('SHA-256', buffer));
      if (buffer.byteLength !== chunk.sizeBytes || hash !== chunk.sha256) {
        throw new HarnessAPIError(
          200,
          'transcript_chunk_integrity_mismatch',
          'Целостность фрагмента полного текста не подтверждена.',
          false,
          'unknown',
        );
      }
      setPage({
        index,
        text: new TextDecoder('utf-8', { fatal: true }).decode(buffer),
      });
    } catch (cause) {
      if (abort.signal.aborted) return;
      if (cause instanceof HarnessAPIError && cause.status === 401) {
        onExpired();
        return;
      }
      setError(safeFailure(cause));
    } finally {
      if (!abort.signal.aborted) setPageLoading(false);
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
      {phase === 'shown' && page && manifest?.complete && (
        <div className="safe-text-paged">
          <p className="muted safe-text-status">
            Полный текст проверен по SHA-256. Показан фрагмент {page.index + 1}{' '}
            из {manifest.chunks.length}; в памяти остаётся только один фрагмент.
          </p>
          <div className="safe-text-page-controls">
            <button
              type="button"
              onClick={() => showPage(page.index - 1)}
              disabled={pageLoading || page.index === 0}
            >
              Предыдущий
            </button>
            <button
              type="button"
              onClick={() => showPage(page.index + 1)}
              disabled={
                pageLoading || page.index + 1 === manifest.chunks.length
              }
            >
              Следующий
            </button>
          </div>
          {renderMarkdown && (
            <p className="muted safe-text-status">
              Для большого ответа разметка отключена, чтобы не перегружать
              вкладку.
            </p>
          )}
          <pre className="safe-text-full">{page.text}</pre>
        </div>
      )}
      {phase === 'shown' &&
        !page &&
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
