import { createHash } from 'node:crypto';
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { harnessAPI, HarnessAPIError } from '../src/harness-api';
import { SafeTextExpansion } from '../src/safe-text-expansion';
import type { Session } from '../src/panel-api';
import type {
  TranscriptManifest,
  TranscriptSource,
} from '../src/transcript-view';

const session: Session = {
  user: { id: '1-1', login: 'owner', name: 'Owner' },
  csrf: 's'.repeat(43),
  writes_enabled: true,
};
const nodeId = '10000000-0000-4000-8000-000000000001';
const dialogId = '20000000-0000-4000-8000-000000000001';
const attemptId = '30000000-0000-4000-8000-000000000001';
const source: TranscriptSource = {
  kind: 'assistant_message',
  id: '40000000-0000-4000-8000-000000000001',
  index: 0,
  stream: 'none',
};
const firstArtifact = '60000000-0000-4000-8000-000000000001';
const secondArtifact = '60000000-0000-4000-8000-000000000002';

function sha256(value: Uint8Array | string): string {
  return createHash('sha256').update(value).digest('hex');
}

function bytes(value: string): Uint8Array<ArrayBuffer> {
  return new TextEncoder().encode(value) as Uint8Array<ArrayBuffer>;
}

function completeManifest(
  preview: string,
  fullText: string,
): TranscriptManifest {
  const first = bytes(fullText.slice(0, preview.length));
  const second = bytes(fullText.slice(preview.length));
  return {
    schemaId: 'transcript-view-v1',
    nodeId,
    dialogId,
    attemptId,
    generation: 1,
    textId: '50000000-0000-4000-8000-000000000001',
    source,
    preview,
    previewTruncated: true,
    redaction: 'none',
    complete: true,
    sizeBytes: first.byteLength + second.byteLength,
    sha256: sha256(bytes(fullText)),
    chunks: [
      {
        index: 0,
        offsetBytes: 0,
        artifactId: firstArtifact,
        sizeBytes: first.byteLength,
        sha256: sha256(first),
      },
      {
        index: 1,
        offsetBytes: first.byteLength,
        artifactId: secondArtifact,
        sizeBytes: second.byteLength,
        sha256: sha256(second),
      },
    ],
  };
}

function renderExpansion(preview: string) {
  return render(
    <SafeTextExpansion
      session={session}
      nodeId={nodeId}
      dialogId={dialogId}
      attemptId={attemptId}
      source={source}
      preview={preview}
      redaction="none"
      truncated
      onExpired={vi.fn()}
    />,
  );
}

afterEach(cleanup);

describe('SafeTextExpansion', () => {
  it('checks exact chunk scope and hashes before showing full text after remount', async () => {
    const preview = 'safe preview ';
    const fullText = `${preview}continued after refresh`;
    const manifest = completeManifest(preview, fullText);
    const chunks = new Map([
      [firstArtifact, bytes(fullText.slice(0, preview.length))],
      [secondArtifact, bytes(fullText.slice(preview.length))],
    ]);
    const manifestRead = vi
      .spyOn(harnessAPI, 'safeTextManifest')
      .mockResolvedValue(manifest);
    const metadataRead = vi
      .spyOn(harnessAPI, 'artifactMetadata')
      .mockImplementation(async (_session, requestedNodeId, artifactId) => {
        const chunk = manifest.chunks.find(
          (candidate) => candidate.artifactId === artifactId,
        );
        if (!chunk) throw new Error('unknown chunk');
        return {
          protocolVersion: 1,
          schemaId: 'harness-wire-v2',
          nodeId: requestedNodeId,
          dialogId,
          attemptId,
          artifactId,
          name: `safe-text-${String(chunk.index).padStart(2, '0')}.txt`,
          mediaType: 'text/plain; charset=utf-8',
          sizeBytes: chunk.sizeBytes,
          sha256: chunk.sha256,
          redaction: 'none',
          truncated: false,
          disposition: 'attachment',
        };
      });
    const artifactRead = vi
      .spyOn(harnessAPI, 'artifact')
      .mockImplementation(async (_session, _nodeId, artifactId) => {
        const chunk = chunks.get(artifactId);
        if (!chunk) throw new Error('unknown chunk');
        return {
          bytes: chunk.buffer,
          mediaType: 'application/octet-stream',
          disposition: 'attachment; filename="safe-text.txt"',
        };
      });

    const firstView = renderExpansion(preview);
    fireEvent.click(
      await screen.findByRole('button', { name: /Показать полный текст/ }),
    );
    expect(await screen.findByText(fullText)).toBeDefined();
    expect(metadataRead).toHaveBeenCalledTimes(2);
    expect(artifactRead).toHaveBeenCalledTimes(2);
    firstView.unmount();

    renderExpansion(preview);
    fireEvent.click(
      await screen.findByRole('button', { name: /Показать полный текст/ }),
    );
    expect(await screen.findByText(fullText)).toBeDefined();
    expect(manifestRead).toHaveBeenCalledTimes(2);
    expect(metadataRead).toHaveBeenCalledTimes(4);
    expect(artifactRead).toHaveBeenCalledTimes(4);
  });

  it('marks an old truncated result as unavailable instead of inventing content', async () => {
    vi.spyOn(harnessAPI, 'safeTextManifest').mockRejectedValue(
      new HarnessAPIError(404, 'not_found', 'not found'),
    );
    renderExpansion('old preview');
    expect(
      await screen.findByText('Полный текст этого результата не сохранён.'),
    ).toBeDefined();
    expect(screen.queryByRole('button')).toBeNull();
  });

  it('shows an explicit output limit without requesting incomplete chunks', async () => {
    const artifactRead = vi.spyOn(harnessAPI, 'artifact');
    vi.spyOn(harnessAPI, 'safeTextManifest').mockResolvedValue({
      schemaId: 'transcript-view-v1',
      nodeId,
      dialogId,
      attemptId,
      generation: 1,
      textId: '50000000-0000-4000-8000-000000000002',
      source,
      preview: 'limited preview',
      previewTruncated: true,
      redaction: 'none',
      complete: false,
      reason: 'output_limit_exceeded',
      sizeBytes: 1024 * 1024,
      sha256: sha256('limited preview'),
      chunks: [],
    });
    renderExpansion('limited preview');
    expect(
      await screen.findByText(
        'Полный текст не сохранён: превышен допустимый объём вывода.',
      ),
    ).toBeDefined();
    expect(artifactRead).not.toHaveBeenCalled();
  });

  it('fails closed when artifact metadata is bound to another attempt', async () => {
    const preview = 'safe preview ';
    const fullText = `${preview}never shown`;
    const manifest = completeManifest(preview, fullText);
    vi.spyOn(harnessAPI, 'safeTextManifest').mockResolvedValue(manifest);
    vi.spyOn(harnessAPI, 'artifactMetadata').mockResolvedValue({
      protocolVersion: 1,
      schemaId: 'harness-wire-v2',
      nodeId,
      dialogId,
      attemptId: '30000000-0000-4000-8000-000000000009',
      artifactId: firstArtifact,
      name: 'safe-text-00.txt',
      mediaType: 'text/plain; charset=utf-8',
      sizeBytes: manifest.chunks[0].sizeBytes,
      sha256: manifest.chunks[0].sha256,
      redaction: 'none',
      truncated: false,
      disposition: 'attachment',
    });
    const artifactRead = vi.spyOn(harnessAPI, 'artifact');
    renderExpansion(preview);
    fireEvent.click(
      await screen.findByRole('button', { name: /Показать полный текст/ }),
    );
    await waitFor(() =>
      expect(
        screen.getByText(
          'Фрагмент полного текста относится к другому источнику.',
        ),
      ).toBeDefined(),
    );
    expect(screen.queryByText(fullText)).toBeNull();
    expect(artifactRead).not.toHaveBeenCalled();
  });

  it('fails closed when downloaded bytes do not match the manifest hash', async () => {
    const preview = 'safe preview ';
    const fullText = `${preview}never shown`;
    const manifest = completeManifest(preview, fullText);
    const first = manifest.chunks[0];
    vi.spyOn(harnessAPI, 'safeTextManifest').mockResolvedValue(manifest);
    vi.spyOn(harnessAPI, 'artifactMetadata').mockResolvedValue({
      protocolVersion: 1,
      schemaId: 'harness-wire-v2',
      nodeId,
      dialogId,
      attemptId,
      artifactId: first.artifactId,
      name: 'safe-text-00.txt',
      mediaType: 'text/plain; charset=utf-8',
      sizeBytes: first.sizeBytes,
      sha256: first.sha256,
      redaction: 'none',
      truncated: false,
      disposition: 'attachment',
    });
    vi.spyOn(harnessAPI, 'artifact').mockResolvedValue({
      bytes: bytes('safe previex ').buffer,
      mediaType: 'application/octet-stream',
      disposition: 'attachment',
    });
    renderExpansion(preview);
    fireEvent.click(
      await screen.findByRole('button', { name: /Показать полный текст/ }),
    );
    expect(
      await screen.findByText(
        'Целостность фрагмента полного текста не подтверждена.',
      ),
    ).toBeDefined();
    expect(screen.queryByText(fullText)).toBeNull();
  });
});
