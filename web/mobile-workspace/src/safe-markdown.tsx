import Markdown from 'react-markdown';
import remarkGfm from 'remark-gfm';

function safeUrl(url: string): string {
  try {
    const parsed = new URL(url);
    return parsed.protocol === 'https:' || parsed.protocol === 'http:'
      ? parsed.href
      : '';
  } catch {
    return '';
  }
}

export function SafeMarkdown({ markdown }: { markdown: string }) {
  return (
    <div className="safe-markdown">
      <Markdown
        components={{
          a: ({ children, href }) =>
            href ? (
              <a href={href} rel="noreferrer noopener" target="_blank">
                {children}
              </a>
            ) : (
              <span>{children}</span>
            ),
          h1: ({ children }) => <h4>{children}</h4>,
          h2: ({ children }) => <h4>{children}</h4>,
          h3: ({ children }) => <h4>{children}</h4>,
          img: ({ alt, src }) =>
            src ? (
              <a
                className="markdown-image-reference"
                href={src}
                rel="noreferrer noopener"
                target="_blank"
              >
                Изображение: {alt || 'открыть ссылку'}
              </a>
            ) : (
              <span className="markdown-image-reference">
                Изображение: {alt || 'ссылка недоступна'}
              </span>
            ),
        }}
        remarkPlugins={[remarkGfm]}
        skipHtml
        urlTransform={safeUrl}
      >
        {markdown}
      </Markdown>
    </div>
  );
}
