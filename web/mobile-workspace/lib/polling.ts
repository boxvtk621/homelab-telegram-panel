const EVENT_POLL_FOREGROUND_MS = 2_000;
const EVENT_POLL_BACKGROUND_MS = 10_000;
const EVENT_POLL_JITTER = 0.2;

export function eventPollDelay(
  visibility: DocumentVisibilityState,
  random: () => number = Math.random,
): number {
  const base =
    visibility === 'visible'
      ? EVENT_POLL_FOREGROUND_MS
      : EVENT_POLL_BACKGROUND_MS;
  const unit = Math.min(1, Math.max(0, random()));
  return Math.round(
    base * (1 - EVENT_POLL_JITTER + 2 * EVENT_POLL_JITTER * unit),
  );
}
