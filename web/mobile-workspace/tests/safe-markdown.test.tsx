import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { SafeMarkdown } from '../src/safe-markdown';

afterEach(cleanup);

describe('SafeMarkdown', () => {
  it('renders the agent response structure and safe external links', () => {
    render(
      <SafeMarkdown
        markdown={[
          '# Результат проверки',
          '',
          'Найден `config.yaml`:',
          '',
          '- резервная копия цела',
          '- [отчёт](https://example.com/report)',
          '',
          '```sh',
          'sha256sum config.yaml',
          '```',
        ].join('\n')}
      />,
    );

    expect(
      screen.getByRole('heading', { name: 'Результат проверки' }),
    ).toBeDefined();
    expect(screen.getByText('config.yaml').tagName).toBe('CODE');
    expect(screen.getAllByRole('listitem')).toHaveLength(2);
    expect(screen.getByRole('link', { name: 'отчёт' })).toMatchObject({
      href: 'https://example.com/report',
      rel: 'noreferrer noopener',
      target: '_blank',
    });
    expect(screen.getByText('sha256sum config.yaml').tagName).toBe('CODE');
  });

  it('does not execute raw HTML or create unsafe links', () => {
    const { container } = render(
      <SafeMarkdown
        markdown={[
          '<script>window.compromised = true</script>',
          '<img src=x onerror="window.compromised = true">',
          '[опасная ссылка](javascript:alert(1))',
          '[почта](mailto:owner@example.com)',
        ].join('\n\n')}
      />,
    );

    expect(container.querySelector('script')).toBeNull();
    expect(container.querySelector('img')).toBeNull();
    expect(container.innerHTML).not.toContain('javascript:');
    expect(container.innerHTML).not.toContain('mailto:');
    expect(screen.queryByRole('link')).toBeNull();
    expect(screen.getByText('опасная ссылка').tagName).toBe('SPAN');
    expect(screen.getByText('почта').tagName).toBe('SPAN');
  });

  it('renders Markdown images as links without loading remote content', () => {
    const { container } = render(
      <SafeMarkdown markdown="![схема](https://example.com/tracking-pixel.png)" />,
    );

    expect(container.querySelector('img')).toBeNull();
    expect(
      screen.getByRole('link', { name: 'Изображение: схема' }),
    ).toMatchObject({
      href: 'https://example.com/tracking-pixel.png',
      rel: 'noreferrer noopener',
      target: '_blank',
    });
  });
});
