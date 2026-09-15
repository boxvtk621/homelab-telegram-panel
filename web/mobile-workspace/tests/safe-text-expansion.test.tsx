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

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

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
    const chunkRead = vi
      .spyOn(harnessAPI, 'safeTextChunk')
      .mockImplementation(
        async (
          _session,
          _nodeId,
          _dialogId,
          _attemptId,
          _textId,
          _source,
          chunk,
        ) => {
          const value = chunks.get(chunk.artifactId);
          if (!value) throw new Error('unknown chunk');
          return value.buffer;
        },
      );

    const firstView = renderExpansion(preview);
    fireEvent.click(
      await screen.findByRole('button', { name: /Показать полный текст/ }),
    );
    expect(await screen.findByText(fullText)).toBeDefined();
    expect(chunkRead).toHaveBeenCalledTimes(2);
    firstView.unmount();

    renderExpansion(preview);
    fireEvent.click(
      await screen.findByRole('button', { name: /Показать полный текст/ }),
    );
    expect(await screen.findByText(fullText)).toBeDefined();
    expect(manifestRead).toHaveBeenCalledTimes(2);
    expect(chunkRead).toHaveBeenCalledTimes(4);
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
    const chunkRead = vi.spyOn(harnessAPI, 'safeTextChunk');
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
    expect(chunkRead).not.toHaveBeenCalled();
  });

  it('fails closed when the exact chunk endpoint rejects another scope', async () => {
    const preview = 'safe preview ';
    const fullText = `${preview}never shown`;
    const manifest = completeManifest(preview, fullText);
    vi.spyOn(harnessAPI, 'safeTextManifest').mockResolvedValue(manifest);
    const chunkRead = vi
      .spyOn(harnessAPI, 'safeTextChunk')
      .mockRejectedValue(
        new HarnessAPIError(
          404,
          'not_found',
          'Фрагмент полного текста относится к другому источнику.',
        ),
      );
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
    expect(chunkRead).toHaveBeenCalledTimes(1);
  });

  it('fails closed when downloaded bytes do not match the manifest hash', async () => {
    const preview = 'safe preview ';
    const fullText = `${preview}never shown`;
    const manifest = completeManifest(preview, fullText);
    vi.spyOn(harnessAPI, 'safeTextManifest').mockResolvedValue(manifest);
    vi.spyOn(harnessAPI, 'safeTextChunk').mockResolvedValue(
      bytes('safe previex ').buffer,
    );
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

  it('verifies a large result incrementally and keeps only one navigable chunk rendered', async () => {
    const preview = 'large preview\n';
    const fullText = `${preview}${'x'.repeat(2 * 1024 * 1024)}\nlarge-tail`;
    const manifest = completeManifest(preview, fullText);
    const chunks = new Map([
      [firstArtifact, bytes(fullText.slice(0, preview.length))],
      [secondArtifact, bytes(fullText.slice(preview.length))],
    ]);
    vi.spyOn(harnessAPI, 'safeTextManifest').mockResolvedValue(manifest);
    vi.spyOn(harnessAPI, 'safeTextChunk').mockImplementation(
      async (
        _session,
        _nodeId,
        _dialogId,
        _attemptId,
        _textId,
        _source,
        chunk,
      ) => {
        const value = chunks.get(chunk.artifactId);
        if (!value) throw new Error('unknown chunk');
        return value.buffer;
      },
    );
    renderExpansion(preview);
    fireEvent.click(
      await screen.findByRole('button', { name: /Показать полный текст/ }),
    );
    expect(await screen.findByText(/Показан фрагмент 1 из 2/)).toBeDefined();
    expect(screen.queryByText(/large-tail/)).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Следующий' }));
    expect(await screen.findByText(/large-tail/)).toBeDefined();
    expect(screen.getByText(/Показан фрагмент 2 из 2/)).toBeDefined();
  });
});
