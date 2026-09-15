import { createHash } from 'node:crypto';
import { describe, expect, it } from 'vitest';
import { IncrementalSHA256 } from '../src/incremental-sha256';

function incremental(parts: Uint8Array[]): string {
  const digest = new IncrementalSHA256();
  for (const part of parts) digest.update(part);
  return digest.digestHex();
}

describe('IncrementalSHA256', () => {
  it('matches standard SHA-256 vectors across block boundaries', () => {
    const encoder = new TextEncoder();
    expect(incremental([])).toBe(createHash('sha256').update('').digest('hex'));
    expect(incremental([encoder.encode('abc')])).toBe(
      createHash('sha256').update('abc').digest('hex'),
    );
    const value = encoder.encode('0123456789abcdef'.repeat(20_000));
    expect(
      incremental([
        value.subarray(0, 1),
        value.subarray(1, 63),
        value.subarray(63, 65_537),
        value.subarray(65_537),
      ]),
    ).toBe(createHash('sha256').update(value).digest('hex'));
  });

  it('cannot be reused after finalization', () => {
    const digest = new IncrementalSHA256();
    digest.update(new Uint8Array([1]));
    digest.digestHex();
    expect(() => digest.update(new Uint8Array([2]))).toThrow('sha256_finished');
    expect(() => digest.digestHex()).toThrow('sha256_finished');
  });
});
