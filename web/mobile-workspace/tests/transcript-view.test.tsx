import fixtures from '../../../api/transcript-view-v1.fixtures.json';
import { describe, expect, it } from 'vitest';
import {
  parseTranscriptManifest,
  projectTranscriptMessages,
} from '../src/transcript-view.ts';
import type { TranscriptProjectionFixture } from '../src/transcript-view-types.ts';

describe('transcript-view-v1', () => {
  it('strictly accepts and rejects the canonical manifest fixtures', () => {
    for (const fixture of fixtures.fixtures.filter(
      (value) => value.contractType === 'manifest',
    )) {
      const parse = () =>
        parseTranscriptManifest(JSON.stringify(fixture.value));
      if (fixture.shapeValid) expect(parse).not.toThrow();
      else expect(parse).toThrow();
    }
  });

  it('merges delta, final, history and replica by exact semantic identity', () => {
    const fixture = fixtures.fixtures.find(
      (value) => value.name === 'projection.delta_final_history_replica',
    );
    expect(fixture).toBeDefined();
    const value = fixture?.value as TranscriptProjectionFixture;
    expect(projectTranscriptMessages(value.candidates)).toEqual(value.expected);
    expect(value.expected).toHaveLength(2);
    expect(value.expected[0].text).toBe(value.expected[1].text);
    expect(value.expected[0].messageId).not.toBe(value.expected[1].messageId);
  });

  it('fails closed on conflicting values at one revision', () => {
    const fixture = fixtures.fixtures.find(
      (value) => value.name === 'projection.delta_final_history_replica',
    )?.value as TranscriptProjectionFixture;
    expect(() =>
      projectTranscriptMessages([
        fixture.candidates[1],
        { ...fixture.candidates[1], text: 'conflict' },
      ]),
    ).toThrow('transcript_projection_conflict');
  });
});
