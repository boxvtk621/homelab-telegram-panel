import { ArrowLeft, Download, Upload } from 'lucide-react';
import { useEffect, useMemo, useRef, useState } from 'react';
import {
  configurationAPI,
  validBuildContextManifest,
  type BuildContextManifest,
  type ConfigurationDraft,
  type ConfigurationValidation,
} from './configuration-api';
import { scanConfigurationSecrets } from './configuration-secret-scan';
import { APIError, type Session } from './panel-api';

type Props = {
  session: Session;
  nodeId: string;
  nodeName: string;
  hostId: string;
  onBack: () => void;
  onExpired: () => void;
};
type EditorSnapshot = {
  nodeId: string;
  json: string;
  dockerfile: string;
  buildContextManifest?: BuildContextManifest;
};
type Preset = EditorSnapshot & { engine: 'cursor' | 'codex'; name: string };

const published = {
  cursor:
    'ghcr.io/boxvtk621/homelab-harness-cursor@sha256:f67cafe1082174c78cba6130074c113ed30464c4a59e0f442e67874f1d01670c',
  codex:
    'ghcr.io/boxvtk621/homelab-harness-codex@sha256:3aeed3be5b21123edcd82b8c221027925d322d241e224ad06a56375705919984',
} as const;
const revisions = {
  cursor: '22222222-2222-4222-8222-222222222222',
  codex: '55555555-5555-4555-8555-555555555555',
} as const;

function preset(engine: 'cursor' | 'codex', hostId: string, nodeId: string): Preset {
  return {
    nodeId,
    engine,
    name: engine === 'cursor' ? 'Cursor' : 'Codex',
    json: JSON.stringify(
      {
        schemaVersion: 2,
        name: `${engine === 'cursor' ? 'Cursor' : 'Codex'} managed`,
        engine,
        deployment: {
          kind: 'managed',
          hostId,
          dockerfileRevision: revisions[engine],
        },
        profile: {
          mode: 'universal',
          basePrompt: '',
          instructions: [],
          mcp: [],
          access: { default: 'deny', nativeTools: [] },
        },
      },
      null,
      2,
    ),
    dockerfile: `# Published immutable ${engine} harness image\nFROM ${published[engine]}\nUSER 10001:10001\n`,
  };
}

function readableDiff(before: string, after: string) {
  if (before === after) return '  без изменений';
  const oldLines = before.split('\n');
  const newLines = after.split('\n');
  const lines: string[] = [];
  for (let index = 0; index < Math.max(oldLines.length, newLines.length); index++) {
    if (oldLines[index] === newLines[index]) continue;
    if (oldLines[index] !== undefined) lines.push(`- ${oldLines[index]}`);
    if (newLines[index] !== undefined) lines.push(`+ ${newLines[index]}`);
    if (lines.length >= 18) {
      lines.push('… diff сокращён');
      break;
    }
  }
  return lines.join('\n');
}

function buildContextBindingDiagnostics(snapshot: EditorSnapshot) {
  if (!snapshot.buildContextManifest) return [];
  try {
    const value = JSON.parse(snapshot.json) as {
      deployment?: { kind?: unknown; buildContextRevision?: unknown };
    };
    const deployment = value.deployment;
    if (
      deployment?.kind === 'external' ||
      (deployment?.kind === 'managed' &&
        deployment.buildContextRevision !==
          snapshot.buildContextManifest.revision)
    )
      return [
        {
          code: 'build_context_binding',
          pointer: '/buildContextManifest/revision',
          line: 1,
          column: 1,
          message:
            'Build context manifest должен соответствовать managed deployment.buildContextRevision.',
        },
      ];
  } catch {
    // Invalid raw JSON is a durable draft; the server reports its syntax safely.
  }
  return [];
}

export function ConfigurationEditor({
  session,
  nodeId,
  nodeName,
  hostId,
  onBack,
  onExpired,
}: Props) {
  const defaults = useMemo(() => preset('cursor', hostId, nodeId), [hostId, nodeId]);
  const [json, setJSON] = useState('');
  const [dockerfile, setDockerfile] = useState('');
  const [version, setVersion] = useState(0);
  const [buildContextManifest, setBuildContextManifest] =
    useState<BuildContextManifest>();
  const [validation, setValidation] = useState<ConfigurationValidation>();
  const [validatedSnapshot, setValidatedSnapshot] = useState<EditorSnapshot>();
  const [message, setMessage] = useState('Загрузка черновика…');
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [candidate, setCandidate] = useState<Preset>();
  const [conflict, setConflict] = useState<ConfigurationDraft>();
  const input = useRef<HTMLInputElement>(null);
  const generation = useRef(0);
  const editorRevision = useRef(0);
  const operation = useRef<AbortController | undefined>(undefined);

  useEffect(() => {
    const requestGeneration = ++generation.current;
    operation.current?.abort();
    const abort = new AbortController();
    operation.current = abort;
    configurationAPI
      .read(nodeId, abort.signal)
      .then((draft) => {
        if (abort.signal.aborted || generation.current !== requestGeneration) return;
        setJSON(draft.rawJsonText);
        setDockerfile(draft.rawDockerfileText);
        setBuildContextManifest(draft.buildContextManifest);
        setVersion(draft.draftVersion);
        setValidation(draft.validation);
        setValidatedSnapshot({
          nodeId,
          json: draft.rawJsonText,
          dockerfile: draft.rawDockerfileText,
          buildContextManifest: draft.buildContextManifest,
        });
        setMessage(`Черновик v${draft.draftVersion} восстановлен.`);
      })
      .catch((cause) => {
        if (abort.signal.aborted || generation.current !== requestGeneration) return;
        if (cause instanceof APIError && cause.status === 404) {
          setJSON(defaults.json);
          setDockerfile(defaults.dockerfile);
          setMessage('Новый несохранённый черновик.');
        } else if (cause instanceof APIError && cause.status === 401) onExpired();
        else setMessage('Не удалось загрузить черновик.');
      })
      .finally(() => {
        if (!abort.signal.aborted && generation.current === requestGeneration) setLoading(false);
      });
    return () => abort.abort();
  }, [defaults, nodeId, onExpired]);

  const changed = (
    nextJSON: string,
    nextDockerfile: string,
    ...manifestArgument: [BuildContextManifest?]
  ) => {
    editorRevision.current++;
    setJSON(nextJSON);
    setDockerfile(nextDockerfile);
    setBuildContextManifest(
      manifestArgument.length === 0
        ? buildContextManifest
        : manifestArgument[0],
    );
    setValidation(undefined);
    setValidatedSnapshot(undefined);
    setConflict(undefined);
    setMessage('Есть несохранённые изменения. Save не Apply.');
  };

  const localSecretGate = (snapshot: EditorSnapshot) => {
    const diagnostics = scanConfigurationSecrets(
      snapshot.json,
      snapshot.dockerfile,
    );
    if (diagnostics.length === 0) return false;
    setValidation({ valid: false, diagnostics, effectStatus: 'none' });
    setValidatedSnapshot(undefined);
    setMessage(
      'Обнаружено secret-like содержимое. Запрос не отправлен; удалите значение и используйте credentialRef.',
    );
    return true;
  };

  const applyLocalSemanticDiagnostics = (snapshot: EditorSnapshot) => {
    const diagnostics = buildContextBindingDiagnostics(snapshot);
    if (diagnostics.length === 0) return false;
    setValidation({ valid: false, diagnostics, effectStatus: 'none' });
    setValidatedSnapshot(snapshot);
    setMessage(
      'Manifest не соответствует buildContextRevision. Черновик можно сохранить invalid, но нельзя экспортировать.',
    );
    return true;
  };

  const begin = (snapshot: EditorSnapshot) => {
    operation.current?.abort();
    const abort = new AbortController();
    operation.current = abort;
    const requestGeneration = generation.current;
    const requestRevision = editorRevision.current;
    setBusy(true);
    return { abort, snapshot, requestGeneration, requestRevision };
  };
  const isCurrent = (requestGeneration: number, requestRevision: number) =>
    generation.current === requestGeneration &&
    editorRevision.current === requestRevision;

  const validate = async () => {
    const snapshot = { nodeId, json, dockerfile, buildContextManifest };
    if (localSecretGate(snapshot) || applyLocalSemanticDiagnostics(snapshot))
      return;
    const request = begin(snapshot);
    try {
      const result = await configurationAPI.validate(
        session,
        snapshot.json,
        snapshot.dockerfile,
        snapshot.buildContextManifest,
        request.abort.signal,
      );
      if (!isCurrent(request.requestGeneration, request.requestRevision)) return;
      setValidation(result.validation);
      setValidatedSnapshot(snapshot);
      setMessage(result.validation.valid ? 'Проверка пройдена. Эффектов нет; черновик можно сохранить.' : 'Исправьте ошибки и повторите проверку.');
    } catch (cause) {
      if (request.abort.signal.aborted || !isCurrent(request.requestGeneration, request.requestRevision)) return;
      if (cause instanceof APIError && cause.status === 401) onExpired();
      else setMessage('Проверка недоступна. Черновик не изменён.');
    } finally {
      if (isCurrent(request.requestGeneration, request.requestRevision)) setBusy(false);
    }
  };

  const save = async () => {
    const snapshot = { nodeId, json, dockerfile, buildContextManifest };
    if (localSecretGate(snapshot)) return;
    const request = begin(snapshot);
    try {
      const draft = await configurationAPI.save(
        session,
        nodeId,
        version,
        snapshot.json,
        snapshot.dockerfile,
        snapshot.buildContextManifest,
        request.abort.signal,
      );
      if (!isCurrent(request.requestGeneration, request.requestRevision)) return;
      setVersion(draft.draftVersion);
      setValidation(draft.validation);
      setValidatedSnapshot(snapshot);
      setConflict(undefined);
      setMessage(`Черновик v${draft.draftVersion} сохранён. Ничего не применено.`);
    } catch (cause) {
      if (request.abort.signal.aborted || !isCurrent(request.requestGeneration, request.requestRevision)) return;
      const serverDraft = configurationAPI.conflict(cause, nodeId);
      if (serverDraft) {
        setConflict(serverDraft);
        setMessage(`Конфликт: локальный текст сохранён в редакторе; сервер остаётся на v${serverDraft.draftVersion}.`);
      } else if (cause instanceof APIError && cause.status === 401) onExpired();
      else setMessage('Сохранение не выполнено; локальный текст сохранён.');
    } finally {
      if (isCurrent(request.requestGeneration, request.requestRevision)) setBusy(false);
    }
  };

  const exportDraft = () => {
    const snapshot = { nodeId, json, dockerfile, buildContextManifest };
    if (localSecretGate(snapshot)) return;
    if (
      !validation?.valid ||
      !validatedSnapshot ||
      validatedSnapshot.nodeId !== snapshot.nodeId ||
      validatedSnapshot.json !== snapshot.json ||
      validatedSnapshot.dockerfile !== snapshot.dockerfile ||
      JSON.stringify(validatedSnapshot.buildContextManifest) !==
        JSON.stringify(snapshot.buildContextManifest)
    ) {
      setMessage('Сначала проверьте текущую версию текста.');
      return;
    }
    const blob = new Blob(
      [
        JSON.stringify(
          {
            schemaId: 'agent-configuration-export-v2',
            rawJsonText: snapshot.json,
            rawDockerfileText: snapshot.dockerfile,
            ...(snapshot.buildContextManifest
              ? { buildContextManifest: snapshot.buildContextManifest }
              : {}),
          },
          null,
          2,
        ),
      ],
      { type: 'application/json' },
    );
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement('a');
    anchor.href = url;
    anchor.download = `configuration-${nodeId}.json`;
    anchor.click();
    URL.revokeObjectURL(url);
  };

  const importDraft = async (file?: File) => {
    if (!file || file.size > 3 * 1024 * 1024) {
      setMessage('Configuration package должен быть не больше 3 MiB.');
      return;
    }
    const text = await file.text();
    try {
      const value = JSON.parse(text) as Record<string, unknown>;
      if (value.schemaId === 'agent-configuration-export-v2') {
        const allowed = [
          'buildContextManifest',
          'rawDockerfileText',
          'rawJsonText',
          'schemaId',
        ];
        if (
          Object.keys(value).some((key) => !allowed.includes(key)) ||
          typeof value.rawJsonText !== 'string' ||
          typeof value.rawDockerfileText !== 'string' ||
          new TextEncoder().encode(value.rawJsonText).length > 256 * 1024 ||
          new TextEncoder().encode(value.rawDockerfileText).length >
            128 * 1024 ||
          (value.buildContextManifest !== undefined &&
            !validBuildContextManifest(value.buildContextManifest))
        )
          throw new Error('invalid package');
        changed(
          value.rawJsonText,
          value.rawDockerfileText,
          value.buildContextManifest as BuildContextManifest | undefined,
        );
        const importedSnapshot = {
          nodeId,
          json: value.rawJsonText,
          dockerfile: value.rawDockerfileText,
          buildContextManifest: value.buildContextManifest as
            | BuildContextManifest
            | undefined,
        };
        if (!localSecretGate(importedSnapshot))
          applyLocalSemanticDiagnostics(importedSnapshot);
      } else {
        changed(text, dockerfile, undefined);
        localSecretGate({ nodeId, json: text, dockerfile });
      }
    } catch {
      setMessage('Import должен быть versioned configuration package или raw JSON configuration.');
    }
  };

  const choosePreset = (engine: 'cursor' | 'codex') => {
    setCandidate(preset(engine, hostId, nodeId));
    setMessage('Просмотрите diff. Текст изменится только после явного Replace.');
  };

  return (
    <section className="configuration-workspace" aria-labelledby="configuration-title" aria-busy={loading || busy}>
      <header className="configuration-header">
        <button type="button" className="secondary" onClick={onBack}>
          <ArrowLeft aria-hidden="true" size={16} /> Назад к реестру
        </button>
        <div>
          <p className="eyebrow">Harness configuration</p>
          <h2 id="configuration-title">{nodeName}</h2>
          <p>Черновик v{version} · Save не Apply · effectStatus: none</p>
        </div>
      </header>

      <div className="configuration-toolbar" aria-label="Инструменты конфигурации">
        <button type="button" className="secondary" disabled={loading || busy} onClick={() => choosePreset('cursor')}>Preset Cursor</button>
        <button type="button" className="secondary" disabled={loading || busy} onClick={() => choosePreset('codex')}>Preset Codex</button>
        <button type="button" className="secondary" disabled={loading || busy} onClick={() => input.current?.click()}><Upload aria-hidden="true" size={15} /> Import JSON</button>
        <input ref={input} className="visually-hidden" type="file" accept="application/json,.json" onChange={(event) => void importDraft(event.target.files?.[0])} />
        <button type="button" className="secondary" disabled={loading || busy || !validation?.valid} onClick={exportDraft}><Download aria-hidden="true" size={15} /> Export refs</button>
      </div>

      {candidate && (
        <section className="preset-review" aria-label={`Предпросмотр замены preset ${candidate.name}`}>
          <div><strong>Preset {candidate.name}: JSON diff</strong><pre>{readableDiff(json, candidate.json)}</pre></div>
          <div><strong>Dockerfile diff</strong><pre>{readableDiff(dockerfile, candidate.dockerfile)}</pre></div>
          <div className="preset-actions">
            <button type="button" disabled={loading || busy} onClick={() => { changed(candidate.json, candidate.dockerfile, undefined); setCandidate(undefined); }}>Replace</button>
            <button type="button" className="secondary" disabled={loading || busy} onClick={() => setCandidate(undefined)}>Отмена</button>
          </div>
        </section>
      )}

      <div className="configuration-grid">
        <div className="configuration-editors">
          <label>JSON schemaVersion 2<textarea aria-label="JSON конфигурации" spellCheck={false} disabled={loading || busy} value={json} onChange={(event) => changed(event.target.value, dockerfile)} /></label>
          <label>Dockerfile<textarea aria-label="Dockerfile конфигурации" spellCheck={false} disabled={loading || busy} value={dockerfile} onChange={(event) => changed(json, event.target.value)} /></label>
        </div>
        <aside className="configuration-validation" aria-label="Результат проверки">
          <h3>Validation</h3>
          {validation?.valid ? <p className="notice success">Текущий текст валиден. Следующее действие: Save draft.</p> : validation ? (
            <ol className="configuration-errors" aria-label="Ошибки конфигурации">
              {validation.diagnostics.map((diagnostic, index) => <li key={`${diagnostic.code}-${diagnostic.pointer}-${index}`}><code>{diagnostic.pointer || '/'}</code><span>строка {diagnostic.line}, столбец {diagnostic.column}</span><span>{diagnostic.message}</span></li>)}
            </ol>
          ) : <p className="muted">Запустите Validate. Проверка не вызывает DNS, HTTP, registry, build или apply.</p>}
          {conflict && <div className="notice error" role="alert"><p>Серверная версия v{conflict.draftVersion}; локальный текст не изменён.</p><button type="button" className="secondary" onClick={() => { changed(conflict.rawJsonText, conflict.rawDockerfileText, conflict.buildContextManifest); setVersion(conflict.draftVersion); setValidation(conflict.validation); setValidatedSnapshot({ nodeId, json: conflict.rawJsonText, dockerfile: conflict.rawDockerfileText, buildContextManifest: conflict.buildContextManifest }); setConflict(undefined); setMessage('Серверная версия восстановлена для сравнения.'); }}>Восстановить серверную</button><button type="button" className="secondary" onClick={() => { setVersion(conflict.draftVersion); setConflict(undefined); setMessage('Локальный текст сохранён; повторите Validate перед Save.'); }}>Оставить локальную</button></div>}
        </aside>
      </div>

      <output className="notice configuration-message" aria-live="polite">{message}</output>
      <footer className="configuration-actions">
        <button type="button" className="secondary" onClick={validate} disabled={loading || busy}>Validate</button>
        <button type="button" onClick={save} disabled={loading || busy || !session.writes_enabled}>Save draft (не Apply)</button>
      </footer>
    </section>
  );
}
