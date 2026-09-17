import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { ConfigurationEditor } from '../src/configuration-editor';
import { validBuildContextManifest } from '../src/configuration-api';
import { scanConfigurationSecrets } from '../src/configuration-secret-scan';

const nodeA = '20000000-0000-4000-8000-000000000001';
const nodeB = '20000000-0000-4000-8000-000000000002';
const host = '10000000-0000-4000-8000-000000000001';
const session = {
  user: { id: 'owner-1', login: 'owner', name: 'owner' },
  csrf: 's'.repeat(43),
  writes_enabled: true,
};

const response = (value: unknown, status = 200) =>
  Promise.resolve(
    new Response(JSON.stringify(value), {
      status,
      headers: { 'Content-Type': 'application/json' },
    }),
  );
const requestBody = (body: BodyInit | null | undefined) => {
  if (typeof body !== 'string') throw new Error('expected JSON request body');
  return JSON.parse(body) as Record<string, unknown>;
};
const validation = { valid: true, diagnostics: [], effectStatus: 'none' as const };
const manifest = {
  revision: '33333333-3333-4333-8333-333333333333',
  assets: [
    {
      path: '.dockerignore',
      assetId: '44444444-4444-4444-8444-444444444444',
      sha256: 'a'.repeat(64),
    },
  ],
};
const managedRaw = `{"schemaVersion":2,"name":"managed","engine":"cursor","deployment":{"kind":"managed","hostId":"${host}","dockerfileRevision":"22222222-2222-4222-8222-222222222222","buildContextRevision":"${manifest.revision}"},"profile":{"mode":"universal","basePrompt":"","instructions":[],"mcp":[],"access":{"default":"deny","nativeTools":[]}}}`;
const draft = (nodeId: string, version = 1) => ({
  schemaId: 'agent-configuration-draft-v2',
  nodeId,
  draftVersion: version,
  rawJsonText: `{"schemaVersion":2,"name":"server","engine":"codex","deployment":{"kind":"external","endpointRef":"harness/codex"},"profile":{"mode":"universal","basePrompt":"","instructions":[],"mcp":[],"access":{"default":"deny","nativeTools":[]}}}`,
  rawDockerfileText: 'FROM scratch\n',
  validation,
});

function editor(nodeId = nodeA) {
  return (
    <ConfigurationEditor
      session={session}
      nodeId={nodeId}
      nodeName={`Node ${nodeId.at(-1)}`}
      hostId={host}
      onBack={() => undefined}
      onExpired={() => undefined}
    />
  );
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it('keeps portable build-context path parity including root dotfiles', () => {
  const withPath = (path: string) => ({
    ...manifest,
    assets: [{ ...manifest.assets[0], path }],
  });
  for (const path of ['.dockerignore', 'src/main.go', 'src/foo..bar'])
    expect(validBuildContextManifest(withPath(path))).toBe(true);
  for (const path of [
    '',
    '/absolute',
    'src/',
    'src//main.go',
    'src/./x',
    'src/../x',
    'src\\main.go',
    'src/new\nline',
  ])
    expect(validBuildContextManifest(withPath(path))).toBe(false);
});

it('rejects non-string build-context manifest references without coercion', () => {
  for (const invalid of [
    { ...manifest, revision: null },
    { ...manifest, assets: [{ ...manifest.assets[0], path: null }] },
    { ...manifest, assets: [{ ...manifest.assets[0], assetId: 42 }] },
    { ...manifest, assets: [{ ...manifest.assets[0], sha256: false }] },
  ])
    expect(validBuildContextManifest(invalid)).toBe(false);
});

it('blocks malformed unquoted secrets locally without validate or save fetch', async () => {
  const fetcher = vi.fn(() => response({ error: 'configuration_draft_not_found' }, 404));
  vi.stubGlobal('fetch', fetcher);
  render(editor());
  await screen.findByText('Новый несохранённый черновик.');
  fetcher.mockClear();
  fireEvent.change(screen.getByLabelText('JSON конфигурации'), {
    target: { value: '{schemaVersion: 2, password: super-secret' },
  });
  fireEvent.click(screen.getByRole('button', { name: 'Validate' }));
  fireEvent.click(screen.getByRole('button', { name: 'Save draft (не Apply)' }));
  expect(fetcher).not.toHaveBeenCalled();
  expect(await screen.findByText(/запрос не отправлен/i)).toBeDefined();
  const diagnostics = scanConfigurationSecrets('{token: value', 'FROM scratch');
  expect(diagnostics).toHaveLength(1);
  expect(JSON.stringify(diagnostics)).not.toContain('value');
});

it('blocks decoded escaped secret keys and values locally without requests', async () => {
  const fetcher = vi.fn(() =>
    response({ error: 'configuration_draft_not_found' }, 404),
  );
  vi.stubGlobal('fetch', fetcher);
  render(editor());
  await screen.findByText('Новый несохранённый черновик.');
  const canonical = draft(nodeA).rawJsonText;
  for (const raw of [
    String.raw`{"\u0074oken":"fixture-secret-value"`,
    String.raw`{"profile":{"basePrompt":"-----BEGIN \u0052SA PRIVATE KEY-----"`,
    canonical.replace(
      '"schemaVersion":2',
      '"schemaVersion":2,"\\u0074oken":"fixture-secret-value"',
    ),
    canonical.replace(
      '"basePrompt":""',
      '"basePrompt":"-----BEGIN \\u0052SA PRIVATE KEY-----"',
    ),
  ]) {
    fetcher.mockClear();
    fireEvent.change(screen.getByLabelText('JSON конфигурации'), {
      target: { value: raw },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Validate' }));
    fireEvent.click(screen.getByRole('button', { name: 'Save draft (не Apply)' }));
    expect(fetcher).not.toHaveBeenCalled();
  }
  const diagnostics = scanConfigurationSecrets(
    '{"\\u0074oken":"fixture-secret-value"',
    '',
  );
  expect(diagnostics.some((item) => item.pointer === '/token')).toBe(true);
  expect(JSON.stringify(diagnostics)).not.toContain('fixture-secret-value');
});

it('shows a readable preset diff and preserves manual text until Replace', async () => {
  vi.stubGlobal('fetch', vi.fn(() => response({ error: 'configuration_draft_not_found' }, 404)));
  render(editor());
  await screen.findByText('Новый несохранённый черновик.');
  const json = screen.getByLabelText('JSON конфигурации') as HTMLTextAreaElement;
  fireEvent.change(json, { target: { value: '{"manual":true}' } });
  fireEvent.click(screen.getByRole('button', { name: 'Preset Codex' }));
  expect(json.value).toBe('{"manual":true}');
  expect(screen.getByText(/- \{"manual":true\}/)).toBeDefined();
  fireEvent.click(screen.getByRole('button', { name: 'Replace' }));
  expect(json.value).toContain('"engine": "codex"');
  expect((screen.getByLabelText('Dockerfile конфигурации') as HTMLTextAreaElement).value).toContain('3aeed3be5b21123edcd82b8c221027925d322d241e224ad06a56375705919984');
});

it('ignores an old node read after switching to a new keyed editor', async () => {
  let resolveOld!: (value: Response) => void;
  const old = new Promise<Response>((resolve) => {
    resolveOld = resolve;
  });
  const fetcher = vi.fn((url: string) =>
    url.endsWith(nodeA)
      ? old
      : response({ error: 'configuration_draft_not_found' }, 404),
  );
  vi.stubGlobal('fetch', fetcher);
  const view = render(<div>{editor(nodeA)}</div>);
  view.rerender(<div key={nodeB}>{editor(nodeB)}</div>);
  await screen.findByText('Новый несохранённый черновик.');
  resolveOld(await response(draft(nodeA)));
  await Promise.resolve();
  expect((screen.getByLabelText('JSON конфигурации') as HTMLTextAreaElement).value).toContain('Cursor managed');
  expect(screen.queryByDisplayValue(/"name":"server"/)).toBeNull();
});

it('ignores a deferred validation response after a keyed node switch', async () => {
  let resolveValidation!: (value: Response) => void;
  const deferred = new Promise<Response>((resolve) => {
    resolveValidation = resolve;
  });
  const fetcher = vi.fn((_url: string, options?: RequestInit) =>
    options?.body
      ? deferred
      : response({ error: 'configuration_draft_not_found' }, 404),
  );
  vi.stubGlobal('fetch', fetcher);
  const view = render(<div key={nodeA}>{editor(nodeA)}</div>);
  await screen.findByText('Новый несохранённый черновик.');
  fireEvent.click(screen.getByRole('button', { name: 'Validate' }));
  view.rerender(<div key={nodeB}>{editor(nodeB)}</div>);
  await screen.findByText('Новый несохранённый черновик.');
  resolveValidation(
    await response({
      schemaId: 'agent-configuration-validate-v2',
      validation,
    }),
  );
  await Promise.resolve();
  expect(screen.queryByText(/Текущий текст валиден/)).toBeNull();
  expect(
    (screen.getByRole('button', { name: 'Export refs' }) as HTMLButtonElement)
      .disabled,
  ).toBe(true);
});

it('preserves local text and exposes the server version on a 409', async () => {
  const fetcher = vi.fn((url: string, options?: RequestInit) => {
    if (!options?.body) return response(draft(nodeA));
    return response(
      {
        schemaId: 'agent-configuration-conflict-v2',
        error: 'configuration_version_conflict',
        current: draft(nodeA, 2),
      },
      409,
    );
  });
  vi.stubGlobal('fetch', fetcher);
  render(editor());
  await screen.findByText('Черновик v1 восстановлен.');
  const json = screen.getByLabelText('JSON конфигурации') as HTMLTextAreaElement;
  fireEvent.change(json, { target: { value: '{"local":true}' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save draft (не Apply)' }));
  await screen.findByText(/Серверная версия v2/);
  expect(json.value).toBe('{"local":true}');
  expect(screen.getByRole('button', { name: 'Оставить локальную' })).toBeDefined();
});

it('sends semantic build-context mismatch to Save and preserves returned invalid draft', async () => {
  const mismatch = {
    ...draft(nodeA),
    buildContextManifest: manifest,
    validation: {
      valid: false,
      effectStatus: 'none' as const,
      diagnostics: [
        {
          code: 'build_context_binding',
          pointer: '/buildContextManifest/revision',
          line: 1,
          column: 1,
          message: 'Manifest mismatch.',
        },
      ],
    },
  };
  const fetcher = vi.fn((_url: string, options?: RequestInit) =>
    options?.body ? response({ ...mismatch, draftVersion: 2 }) : response(mismatch),
  );
  vi.stubGlobal('fetch', fetcher);
  render(editor());
  await screen.findByText('Черновик v1 восстановлен.');
  fetcher.mockClear();
  fireEvent.click(screen.getByRole('button', { name: 'Save draft (не Apply)' }));
  await screen.findByText('Черновик v2 сохранён. Ничего не применено.');
  expect(fetcher).toHaveBeenCalledTimes(1);
  expect(requestBody(fetcher.mock.calls[0]?.[1]?.body).buildContextManifest).toEqual(manifest);
  expect(screen.getByText('Manifest mismatch.')).toBeDefined();
  expect((screen.getByRole('button', { name: 'Export refs' }) as HTMLButtonElement).disabled).toBe(true);
});

it('clears a loaded manifest on preset Replace before Save', async () => {
  const loaded = { ...draft(nodeA), rawJsonText: managedRaw, buildContextManifest: manifest };
  let savedBody: Record<string, unknown> | undefined;
  const fetcher = vi.fn((_url: string, options?: RequestInit) => {
    if (!options?.body) return response(loaded);
    savedBody = requestBody(options.body);
    return response({
      ...draft(nodeA, 2),
      rawJsonText:
        typeof savedBody!.rawJsonText === 'string'
          ? savedBody!.rawJsonText
          : '',
    });
  });
  vi.stubGlobal('fetch', fetcher);
  render(editor());
  await screen.findByText('Черновик v1 восстановлен.');
  fireEvent.click(screen.getByRole('button', { name: 'Preset Codex' }));
  fireEvent.click(screen.getByRole('button', { name: 'Replace' }));
  fireEvent.click(screen.getByRole('button', { name: 'Save draft (не Apply)' }));
  await screen.findByText('Черновик v2 сохранён. Ничего не применено.');
  expect(savedBody).not.toHaveProperty('buildContextManifest');
});

it('clears a loaded manifest on raw import before Save', async () => {
  const loaded = { ...draft(nodeA), rawJsonText: managedRaw, buildContextManifest: manifest };
  let savedBody: Record<string, unknown> | undefined;
  const fetcher = vi.fn((_url: string, options?: RequestInit) => {
    if (!options?.body) return response(loaded);
    savedBody = requestBody(options.body);
    return response({
      ...draft(nodeA, 2),
      rawJsonText:
        typeof savedBody!.rawJsonText === 'string'
          ? savedBody!.rawJsonText
          : '',
    });
  });
  vi.stubGlobal('fetch', fetcher);
  const view = render(editor());
  await screen.findByText('Черновик v1 восстановлен.');
  const file = new File(['{"raw":true}'], 'raw.json', { type: 'application/json' });
  fireEvent.change(view.container.querySelector('input[type="file"]')!, {
    target: { files: [file] },
  });
  await waitFor(() =>
    expect((screen.getByLabelText('JSON конфигурации') as HTMLTextAreaElement).value).toBe('{"raw":true}'),
  );
  fireEvent.click(screen.getByRole('button', { name: 'Save draft (не Apply)' }));
  await screen.findByText('Черновик v2 сохранён. Ничего не применено.');
  expect(savedBody).not.toHaveProperty('buildContextManifest');
});

it('clears a loaded manifest when restoring a server conflict without one', async () => {
  const loaded = { ...draft(nodeA), rawJsonText: managedRaw, buildContextManifest: manifest };
  const bodies: Record<string, unknown>[] = [];
  const fetcher = vi.fn((_url: string, options?: RequestInit) => {
    if (!options?.body) return response(loaded);
    bodies.push(requestBody(options.body));
    if (bodies.length === 1)
      return response(
        {
          schemaId: 'agent-configuration-conflict-v2',
          error: 'configuration_version_conflict',
          current: draft(nodeA, 2),
        },
        409,
      );
    return response(draft(nodeA, 3));
  });
  vi.stubGlobal('fetch', fetcher);
  render(editor());
  await screen.findByText('Черновик v1 восстановлен.');
  fireEvent.click(screen.getByRole('button', { name: 'Save draft (не Apply)' }));
  await screen.findByText(/Серверная версия v2/);
  fireEvent.click(screen.getByRole('button', { name: 'Восстановить серверную' }));
  fireEvent.click(screen.getByRole('button', { name: 'Save draft (не Apply)' }));
  await screen.findByText('Черновик v3 сохранён. Ничего не применено.');
  expect(bodies[1]).not.toHaveProperty('buildContextManifest');
});

it('round-trips JSON, Dockerfile, and manifest refs in the local export package', async () => {
  const value = {
    ...draft(nodeA),
    rawJsonText: `{"schemaVersion":2,"name":"managed","engine":"cursor","deployment":{"kind":"managed","hostId":"${host}","dockerfileRevision":"22222222-2222-4222-8222-222222222222","buildContextRevision":"${manifest.revision}"},"profile":{"mode":"universal","basePrompt":"","instructions":[],"mcp":[],"access":{"default":"deny","nativeTools":[]}}}`,
    buildContextManifest: manifest,
  };
  vi.stubGlobal('fetch', vi.fn(() => response(value)));
  let exported: Blob | undefined;
  render(editor());
  await screen.findByText('Черновик v1 восстановлен.');
  Object.defineProperty(URL, 'createObjectURL', {
    configurable: true,
    value: vi.fn((blob: Blob) => {
      exported = blob;
      return 'blob:configuration';
    }),
  });
  Object.defineProperty(URL, 'revokeObjectURL', {
    configurable: true,
    value: vi.fn(),
  });
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(
    () => undefined,
  );
  fireEvent.click(screen.getByRole('button', { name: 'Export refs' }));
  await waitFor(() => expect(exported).toBeDefined());
  const packageValue = JSON.parse(await exported!.text());
  expect(packageValue.schemaId).toBe('agent-configuration-export-v2');
  expect(packageValue.rawDockerfileText).toBe('FROM scratch\n');
  expect(packageValue.buildContextManifest).toEqual(manifest);
});

it('imports the versioned package with JSON, Dockerfile, and manifest refs', async () => {
  vi.stubGlobal('fetch', vi.fn(() => response({ error: 'configuration_draft_not_found' }, 404)));
  const view = render(editor());
  await screen.findByText('Новый несохранённый черновик.');
  const imported = {
    schemaId: 'agent-configuration-export-v2',
    rawJsonText: '{"imported":true}',
    rawDockerfileText: 'FROM imported\n',
    buildContextManifest: {
      revision: '33333333-3333-4333-8333-333333333333',
      assets: [
        {
          path: 'src/foo..bar',
          assetId: '44444444-4444-4444-8444-444444444444',
          sha256: 'b'.repeat(64),
        },
      ],
    },
  };
  const file = new File([JSON.stringify(imported)], 'configuration.json', {
    type: 'application/json',
  });
  fireEvent.change(view.container.querySelector('input[type="file"]')!, {
    target: { files: [file] },
  });
  await waitFor(() =>
    expect((screen.getByLabelText('JSON конфигурации') as HTMLTextAreaElement).value).toBe(imported.rawJsonText),
  );
  expect((screen.getByLabelText('Dockerfile конфигурации') as HTMLTextAreaElement).value).toBe(imported.rawDockerfileText);
});

it('re-runs the escaped-key secret gate before export and creates no blob', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(() =>
      response({
        ...draft(nodeA),
        rawJsonText: '{"\\u0074oken":"fixture-secret-value"',
      }),
    ),
  );
  const createObjectURL = vi.fn(() => 'blob:should-not-exist');
  Object.defineProperty(URL, 'createObjectURL', {
    configurable: true,
    value: createObjectURL,
  });
  render(editor());
  await screen.findByText('Черновик v1 восстановлен.');
  fireEvent.click(screen.getByRole('button', { name: 'Export refs' }));
  expect(createObjectURL).not.toHaveBeenCalled();
  expect(await screen.findByText(/запрос не отправлен/i)).toBeDefined();
});

it('re-runs the decoded PEM secret gate before export and creates no blob', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(() =>
      response({
        ...draft(nodeA),
        rawJsonText:
          String.raw`{"profile":{"basePrompt":"-----BEGIN \u0052SA PRIVATE KEY-----"`,
      }),
    ),
  );
  const createObjectURL = vi.fn(() => 'blob:should-not-exist');
  Object.defineProperty(URL, 'createObjectURL', {
    configurable: true,
    value: createObjectURL,
  });
  render(editor());
  await screen.findByText('Черновик v1 восстановлен.');
  fireEvent.click(screen.getByRole('button', { name: 'Export refs' }));
  expect(createObjectURL).not.toHaveBeenCalled();
  expect(await screen.findByText(/запрос не отправлен/i)).toBeDefined();
});
