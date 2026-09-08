import { describe, expect, it, vi } from 'vitest';

import {
  bindTelegramBackButton,
  prepareTelegramWebApp,
  type TelegramWebApp,
} from '@/lib/telegram';

function fakeTelegram(versionAtLeast: (version: string) => boolean) {
  return {
    initData: '',
    colorScheme: 'light',
    themeParams: {},
    isVersionAtLeast: vi.fn(versionAtLeast),
    ready: vi.fn(),
    expand: vi.fn(),
    disableVerticalSwipes: vi.fn(),
    onEvent: vi.fn(),
    offEvent: vi.fn(),
    BackButton: {
      show: vi.fn(),
      hide: vi.fn(),
      onClick: vi.fn(),
      offClick: vi.fn(),
    },
  } satisfies TelegramWebApp;
}

describe('Telegram WebApp compatibility guards', () => {
  it('does not call BackButton before Telegram 6.1', () => {
    const telegram = fakeTelegram(() => false);

    const cleanup = bindTelegramBackButton(telegram, true, () => undefined);
    cleanup();

    expect(telegram.isVersionAtLeast).toHaveBeenCalledWith('6.1');
    expect(telegram.BackButton.show).not.toHaveBeenCalled();
    expect(telegram.BackButton.hide).not.toHaveBeenCalled();
    expect(telegram.BackButton.onClick).not.toHaveBeenCalled();
  });

  it('prepares every supported WebApp but changes swipes only from 7.7', () => {
    const legacy = fakeTelegram(() => false);
    prepareTelegramWebApp(legacy);
    expect(legacy.ready).toHaveBeenCalledOnce();
    expect(legacy.expand).toHaveBeenCalledOnce();
    expect(legacy.disableVerticalSwipes).not.toHaveBeenCalled();

    const current = fakeTelegram((version) => version === '7.7');
    prepareTelegramWebApp(current);
    expect(current.disableVerticalSwipes).toHaveBeenCalledOnce();
  });
});
