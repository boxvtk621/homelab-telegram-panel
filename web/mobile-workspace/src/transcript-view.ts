import schema from '../../../api/transcript-view-v1.schema.json';
import {
  HarnessProtocolError,
  parseContractJson,
  type HarnessSchema,
} from './harness-protocol.ts';
import type {
  TranscriptManifest,
  TranscriptProjectionFixture,
  TranscriptSource,
} from './transcript-view-types.ts';

const contract = schema as HarnessSchema;
const maximumTextBytes = 512 * 1024 * 1024;
const sourceOrder = ['delta', 'final', 'history', 'replica'] as const;

export function sameTranscriptSource(
  left: TranscriptSource,
  right: TranscriptSource,
): boolean {
  return (
    left.kind === right.kind &&
    left.id === right.id &&
    left.index === right.index &&
    left.stream === right.stream
  );
}

export function parseTranscriptManifest(raw: string): TranscriptManifest {
  const manifest = parseContractJson<TranscriptManifest>(
    raw,
    'manifest',
    contract,
  );
  validateManifestRelations(manifest);
  return manifest;
}

function validateManifestRelations(manifest: TranscriptManifest): void {
  const previewBytes = new TextEncoder().encode(manifest.preview).byteLength;
  if (previewBytes > 64 * 1024)
    throw new HarnessProtocolError('transcript_preview_too_large');
  if (!manifest.complete) {
    if (
      manifest.reason !== 'output_limit_exceeded' ||
      manifest.chunks.length > 0
    )
      throw new HarnessProtocolError('transcript_incomplete_invalid');
    return;
  }
  if (manifest.sizeBytes > maximumTextBytes || manifest.chunks.length > 64)
    throw new HarnessProtocolError('transcript_size_invalid');
  if (manifest.sizeBytes === 0) {
    if (manifest.chunks.length !== 0)
      throw new HarnessProtocolError('transcript_empty_invalid');
    return;
  }
  if (manifest.chunks.length === 0)
    throw new HarnessProtocolError('transcript_chunks_missing');
  let offset = 0;
  const artifacts = new Set<string>();
  for (const [index, chunk] of manifest.chunks.entries()) {
    if (
      chunk.index !== index ||
      chunk.offsetBytes !== offset ||
      chunk.sizeBytes < 1 ||
      chunk.sizeBytes > 16 * 1024 * 1024 ||
      artifacts.has(chunk.artifactId)
    ) {
      throw new HarnessProtocolError('transcript_chunk_invalid');
    }
    artifacts.add(chunk.artifactId);
    offset += chunk.sizeBytes;
  }
  if (offset !== manifest.sizeBytes)
    throw new HarnessProtocolError('transcript_coverage_invalid');
}

type Candidate = TranscriptProjectionFixture['candidates'][number];
export type ProjectedMessage = TranscriptProjectionFixture['expected'][number];

function projectionKey(candidate: Candidate): string {
  return `${candidate.attemptId}:${candidate.generation}:${candidate.messageId}`;
}

export function projectTranscriptMessages(
  candidates: readonly Candidate[],
): ProjectedMessage[] {
  const projected = new Map<
    string,
    { winner: Candidate; sources: Set<Candidate['source']> }
  >();
  for (const candidate of candidates) {
    const key = projectionKey(candidate);
    const current = projected.get(key);
    if (!current) {
      projected.set(key, {
        winner: candidate,
        sources: new Set([candidate.source]),
      });
      continue;
    }
    current.sources.add(candidate.source);
    if (candidate.revision === current.winner.revision) {
      if (
        candidate.textHash !== current.winner.textHash ||
        candidate.text !== current.winner.text ||
        candidate.final !== current.winner.final
      ) {
        throw new HarnessProtocolError('transcript_projection_conflict');
      }
      continue;
    }
    if (candidate.revision > current.winner.revision)
      current.winner = candidate;
  }
  return [...projected.values()].map(({ winner, sources }) => ({
    messageId: winner.messageId,
    attemptId: winner.attemptId,
    generation: winner.generation,
    textHash: winner.textHash,
    text: winner.text,
    final: winner.final,
    sources: sourceOrder.filter((source) => sources.has(source)),
  }));
}

export type { TranscriptManifest, TranscriptSource };
