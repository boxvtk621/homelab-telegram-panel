const CLIENT_INSTANCE_KEY = 'fixik.mobile.client-instance.v1';
let inMemoryClientInstanceID: string | null = null;
const inMemoryDrafts = new Map<string, string>();

export type TelegramThemeParams = {
  bg_color?: string;
  text_color?: string;
  hint_color?: string;
  link_color?: string;
  button_color?: string;
  button_text_color?: string;
  secondary_bg_color?: string;
  header_bg_color?: string;
  accent_text_color?: string;
  section_bg_color?: string;
  section_header_text_color?: string;
  subtitle_text_color?: string;
  destructive_text_color?: string;
};

type TelegramBackButton = {
  show(): void;
  hide(): void;
  onClick(callback: () => void): void;
  offClick(callback: () => void): void;
};

export type TelegramWebApp = {
  initData: string;
  colorScheme: 'light' | 'dark';
  themeParams: TelegramThemeParams;
  BackButton: TelegramBackButton;
  isVersionAtLeast(version: string): boolean;
  ready(): void;
  expand(): void;
  disableVerticalSwipes?(): void;
  onEvent(event: 'themeChanged', callback: () => void): void;
  offEvent(event: 'themeChanged', callback: () => void): void;
};

declare global {
  interface Window {
    Telegram?: { WebApp?: TelegramWebApp };
  }
}

export function telegramWebApp(): TelegramWebApp | null {
  return window.Telegram?.WebApp ?? null;
}

export function initializeTelegramTheme(
  webApp: TelegramWebApp | null,
): () => void {
  const apply = () => {
    const root = document.documentElement;
    const scheme = webApp?.colorScheme;
    root.classList.toggle('dark', scheme === 'dark');
    root.dataset.telegramTheme = scheme ?? 'system';
    applyThemeParams(root, webApp?.themeParams ?? {});
  };
  apply();
  if (webApp === null) {
    return () => undefined;
  }
  webApp.onEvent('themeChanged', apply);
  return () => webApp.offEvent('themeChanged', apply);
}

export function bindTelegramBackButton(
  webApp: TelegramWebApp | null,
  visible: boolean,
  callback: () => void,
): () => void {
  if (webApp === null || !webApp.isVersionAtLeast('6.1')) {
    return () => undefined;
  }
  webApp.BackButton.offClick(callback);
  webApp.BackButton.onClick(callback);
  if (visible) {
    webApp.BackButton.show();
  } else {
    webApp.BackButton.hide();
  }
  return () => {
    webApp.BackButton.offClick(callback);
    webApp.BackButton.hide();
  };
}

export function prepareTelegramWebApp(webApp: TelegramWebApp | null): void {
  if (webApp === null) {
    return;
  }
  webApp.ready();
  webApp.expand();
  if (webApp.isVersionAtLeast('7.7')) {
    webApp.disableVerticalSwipes?.();
  }
}

export function stableClientInstanceID(): string {
  if (inMemoryClientInstanceID !== null) {
    return inMemoryClientInstanceID;
  }
  try {
    const stored = window.localStorage.getItem(CLIENT_INSTANCE_KEY);
    if (stored !== null && validClientInstanceID(stored)) {
      inMemoryClientInstanceID = stored;
      return stored;
    }
  } catch {
    // A stable in-memory value still lets a storage-restricted Telegram view authenticate.
  }
  const generated = `web:${secureUUID()}`;
  inMemoryClientInstanceID = generated;
  try {
    window.localStorage.setItem(CLIENT_INSTANCE_KEY, generated);
  } catch {
    // Storage can be disabled by the embedding browser. Do not block authentication.
  }
  return generated;
}

export function readDialogDraft(dialogID: string): string {
  const key = draftKey(dialogID);
  const inMemory = inMemoryDrafts.get(key);
  if (inMemory !== undefined) {
    return inMemory;
  }
  try {
    const stored = (window.sessionStorage.getItem(key) ?? '').slice(0, 16_384);
    inMemoryDrafts.set(key, stored);
    return stored;
  } catch {
    return '';
  }
}

export function writeDialogDraft(dialogID: string, value: string): void {
  const key = draftKey(dialogID);
  const bounded = value.slice(0, 16_384);
  if (bounded === '') {
    inMemoryDrafts.delete(key);
  } else {
    inMemoryDrafts.set(key, bounded);
  }
  try {
    if (bounded === '') {
      window.sessionStorage.removeItem(key);
    } else {
      window.sessionStorage.setItem(key, bounded);
    }
  } catch {
    // A draft remains available in React state for this view.
  }
}

function validClientInstanceID(value: string): boolean {
  return /^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$/.test(value);
}

function secureUUID(): string {
  if (typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const encoded = Array.from(bytes, (value) =>
    value.toString(16).padStart(2, '0'),
  );
  return `${encoded.slice(0, 4).join('')}-${encoded.slice(4, 6).join('')}-${encoded.slice(6, 8).join('')}-${encoded.slice(8, 10).join('')}-${encoded.slice(10).join('')}`;
}

function draftKey(dialogID: string): string {
  return `fixik.mobile.draft.session.v1:${dialogID}`;
}

function applyThemeParams(root: HTMLElement, theme: TelegramThemeParams): void {
  const mappings: Array<[keyof TelegramThemeParams, string]> = [
    ['bg_color', '--telegram-bg'],
    ['text_color', '--telegram-text'],
    ['hint_color', '--telegram-hint'],
    ['link_color', '--telegram-link'],
    ['button_color', '--telegram-button'],
    ['button_text_color', '--telegram-button-text'],
    ['secondary_bg_color', '--telegram-secondary-bg'],
    ['header_bg_color', '--telegram-header-bg'],
    ['accent_text_color', '--telegram-accent'],
    ['section_bg_color', '--telegram-section-bg'],
    ['section_header_text_color', '--telegram-section-header'],
    ['subtitle_text_color', '--telegram-subtitle'],
    ['destructive_text_color', '--telegram-destructive'],
  ];
  for (const [parameter, property] of mappings) {
    const value = theme[parameter];
    if (typeof value === 'string' && /^#[0-9a-fA-F]{6}$/.test(value)) {
      root.style.setProperty(property, value);
    } else {
      root.style.removeProperty(property);
    }
  }
}
