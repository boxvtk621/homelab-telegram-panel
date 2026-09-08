import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import '@/app/globals.css';
import { MobileWorkspace } from '@/components/mobile-workspace';

const root = document.getElementById('root');

if (root === null) {
  throw new Error('mobile workspace root is missing');
}

createRoot(root).render(
  <StrictMode>
    <MobileWorkspace />
  </StrictMode>,
);
