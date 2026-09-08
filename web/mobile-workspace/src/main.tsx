import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import './panel.css';
import { Panel } from './panel';

const root = document.getElementById('root');

if (root === null) {
  throw new Error('mobile workspace root is missing');
}

createRoot(root).render(
  <StrictMode>
    <Panel />
  </StrictMode>,
);
