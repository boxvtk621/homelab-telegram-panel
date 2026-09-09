// Read the explicitly supplied credential from stdin; never persist or log it.
import { Cursor } from '@cursor/sdk';
let input = '';
for await (const chunk of process.stdin) {
  input += chunk.toString();
  if (Buffer.byteLength(input) > 8192) process.exit(2);
}
const apiKey = input.trim();
input = '';
console.warn = () => {};
console.error = () => {};
try {
  const models = await Cursor.models.list({ apiKey });
  const available = models.some(model => model.id === 'composer-2.5');
  console.log(JSON.stringify({ stage: 'V2_catalog', status: available ? 'passed' : 'blocked',
    model: 'composer-2.5', modelAvailable: available, providerModelCalls: 0 }));
  process.exit(available ? 0 : 1);
} catch (error) {
  const known = new Set(['AuthenticationError', 'RateLimitError', 'NetworkError', 'ConfigurationError']);
  console.log(JSON.stringify({ stage: 'V2_catalog', status: 'failed',
    errorCode: known.has(error?.name) ? error.name : 'catalog_request_failed', providerModelCalls: 0 }));
  process.exit(1);
}
